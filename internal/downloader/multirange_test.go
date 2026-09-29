package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeData 生成位置相关的伪随机数据。
// 刻意不用重复模式：一旦偏移算错，字节比对就能立刻暴露。
func makeData(n int) []byte {
	data := make([]byte, n)
	x := uint32(0x9E3779B9)
	for i := range data {
		x = x*1664525 + 1013904223
		data[i] = byte(x >> 24)
	}
	return data
}

func parseRangeStart(rng string) int64 {
	var start int64
	fmt.Sscanf(rng, "bytes=%d-", &start)
	return start
}

// rangeServer 支持 Range 的测试文件服务，可精确让「从某个偏移开始的分块」失败，
// 用来验证分块重试、续传与凭证失效。
type rangeServer struct {
	srv    *httptest.Server
	data   []byte
	mut    time.Time
	mu     sync.Mutex
	ranges []string
	failAt int64 // 起始偏移等于它的分块请求会失败；-1 表示不失败
	failN  int   // 剩余失败次数
}

func newRangeServer(t *testing.T, data []byte, failAt int64, failN int) *rangeServer {
	t.Helper()
	s := &rangeServer{data: data, mut: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), failAt: failAt, failN: failN}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rangeServer) URL(path string) string { return s.srv.URL + path }

func (s *rangeServer) handle(w http.ResponseWriter, r *http.Request) {
	if rng := r.Header.Get("Range"); rng != "" {
		s.mu.Lock()
		s.ranges = append(s.ranges, rng)
		fail := s.failAt >= 0 && s.failN > 0 && parseRangeStart(rng) == s.failAt
		if fail {
			s.failN--
		}
		s.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
	}
	s.mu.Lock()
	mut := s.mut
	s.mu.Unlock()
	http.ServeContent(w, r, "big.bin", mut, bytes.NewReader(s.data))
}

func (s *rangeServer) rangeRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *rangeServer) resetRanges() {
	s.mu.Lock()
	s.ranges = nil
	s.mu.Unlock()
}

func (s *rangeServer) setFail(failAt int64, failN int) {
	s.mu.Lock()
	s.failAt, s.failN = failAt, failN
	s.mu.Unlock()
}

// ---------- splitChunks ----------

func TestSplitChunks(t *testing.T) {
	cases := []struct {
		name    string
		total   int64
		workers int
		want    int
	}{
		{"零字节", 0, 8, 0},
		{"负数当空处理", -1, 8, 0},
		{"不足一块只切一块", 512 << 10, 8, 1},
		{"恰好两块", 2 << 20, 2, 2},
		{"大文件按 workers 切", 100 << 20, 8, 8},
		{"workers 多于块数上限", 3 << 20, 64, 3},
		{"workers 为 0 退化为单块", 4 << 20, 0, 1},
		{"workers 为负同样退化", 4 << 20, -3, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitChunks(c.total, c.workers)
			if len(got) != c.want {
				t.Fatalf("块数 = %d, 期望 %d", len(got), c.want)
			}
			if c.want == 0 {
				return
			}
			var sum int64
			for i, ch := range got {
				if ch.Index != i {
					t.Fatalf("索引不连续: %d != %d", ch.Index, i)
				}
				if ch.End < ch.Start {
					t.Fatalf("块 %d 区间非法: %d-%d", i, ch.Start, ch.End)
				}
				if len(got) > 1 && ch.size() < minChunkSize {
					t.Fatalf("块 %d 小于下限: %d", i, ch.size())
				}
				if i > 0 && ch.Start != got[i-1].End+1 {
					t.Fatalf("块 %d 与前块不连续", i)
				}
				sum += ch.size()
			}
			if got[0].Start != 0 {
				t.Fatalf("未从 0 开始: %d", got[0].Start)
			}
			if got[len(got)-1].End != c.total-1 {
				t.Fatalf("未覆盖到末尾: %d != %d", got[len(got)-1].End, c.total-1)
			}
			if sum != c.total {
				t.Fatalf("覆盖不全: %d != %d", sum, c.total)
			}
		})
	}
}

// ---------- 多线程下载主路径 ----------

func TestMultiRangeDownload(t *testing.T) {
	const size = 6 << 20 // 超过 2MB 阈值
	data := makeData(size)
	s := newRangeServer(t, data, -1, 0)
	dir := t.TempDir()

	var maxDone, seenTotal atomic.Int64
	out, err := Download(context.Background(), Options{
		MediaURL: s.URL("/big.bin"), OutDir: dir, FileName: "big.bin", Workers: 4,
	}, func(d, n int) {
		if int64(d) > maxDone.Load() {
			maxDone.Store(int64(d))
		}
		seenTotal.Store(int64(n))
	})
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("内容不一致（偏移拼接有误）")
	}

	rngs := s.rangeRequests()
	if len(rngs) < 2 {
		t.Fatalf("应发出多个 Range 请求，实际 %d 个: %v", len(rngs), rngs)
	}
	// 每个请求都必须是闭区间分块，且拼起来刚好覆盖整个文件。
	var covered int64
	for _, r := range rngs {
		var a, b int64
		if _, err := fmt.Sscanf(r, "bytes=%d-%d", &a, &b); err != nil {
			t.Fatalf("Range 格式异常: %q", r)
		}
		if a < 0 || b < a || b >= int64(size) {
			t.Fatalf("Range 越界: %q", r)
		}
		covered += b - a + 1
	}
	if covered != int64(size) {
		t.Fatalf("Range 总覆盖 %d, 期望 %d", covered, size)
	}
	if seenTotal.Load() != int64(size) || maxDone.Load() != int64(size) {
		t.Fatalf("进度未走满: done=%d total=%d", maxDone.Load(), seenTotal.Load())
	}
	// 完工后临时文件与凭证都要清掉。
	if _, err := os.Stat(out + ".mrp"); !os.IsNotExist(err) {
		t.Fatal(".mrp 未清理")
	}
	if _, err := os.Stat(out + ".mrp.json"); !os.IsNotExist(err) {
		t.Fatal(".mrp.json 未清理")
	}
}

// ---------- 分块失败 -> 续传只补缺 ----------

func TestMultiRangeResume(t *testing.T) {
	const size = 6 << 20
	const workers = 4
	data := makeData(size)
	chunks := splitChunks(size, workers)
	last := chunks[len(chunks)-1]

	// 让最后一块反复失败，逼出「部分完成」的中间态。
	s := newRangeServer(t, data, last.Start, 1<<30)
	dir := t.TempDir()
	opt := Options{MediaURL: s.URL("/big.bin"), OutDir: dir, FileName: "big.bin", Workers: workers}
	out := filepath.Join(dir, "big.bin")

	if _, err := Download(context.Background(), opt, nil); err == nil {
		t.Fatal("最后一块一直失败，下载应当报错")
	}
	state, err := loadRangeState(out + ".mrp.json")
	if err != nil {
		t.Fatalf("应留下续传凭证: %v", err)
	}
	if len(state.Done) != len(chunks) {
		t.Fatalf("凭证分块数不对: %d != %d", len(state.Done), len(chunks))
	}
	if state.Done[len(chunks)-1] {
		t.Fatal("失败的那块不应标记为完成")
	}
	finished := 0
	for _, ok := range state.Done {
		if ok {
			finished++
		}
	}
	if finished != len(chunks)-1 {
		t.Fatalf("应完成 %d 块，实际 %d", len(chunks)-1, finished)
	}
	if state.Validator == "" {
		t.Fatal("凭证应记录 Last-Modified 校验标识")
	}

	// 恢复后重跑：只该补下缺失的那一块。
	s.setFail(-1, 0)
	s.resetRanges()
	got, err := Download(context.Background(), opt, nil)
	if err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	rngs := s.rangeRequests()
	if len(rngs) != 1 {
		t.Fatalf("续传应只补 1 块，实际 %d 个请求: %v", len(rngs), rngs)
	}
	if parseRangeStart(rngs[0]) != last.Start {
		t.Fatalf("补下的块起点不对: %q, 期望 %d", rngs[0], last.Start)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, data) {
		t.Fatal("续传后内容不一致")
	}
}

// ---------- 服务端谎报 Range 支持 -> 回退单连接 ----------

func TestMultiRangeFallbackWhenServerIgnoresRange(t *testing.T) {
	const size = 6 << 20
	data := makeData(size)
	var rangeHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes") // 谎报
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Header.Get("Range") != "" {
			rangeHits.Add(1)
			// 无视 Range，直接吐全量（200 而非 206）
		}
		w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	out, err := Download(context.Background(), Options{
		MediaURL: srv.URL + "/liar.bin", OutDir: dir, FileName: "liar.bin", Workers: 4,
	}, nil)
	if err != nil {
		t.Fatalf("回退单连接后应成功: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, data) {
		t.Fatal("回退后内容不一致")
	}
	if rangeHits.Load() == 0 {
		t.Fatal("应当至少试探过一次 Range")
	}
	if _, err := os.Stat(out + ".mrp"); !os.IsNotExist(err) {
		t.Fatal("回退时应清掉 .mrp")
	}
	if _, err := os.Stat(out + ".mrp.json"); !os.IsNotExist(err) {
		t.Fatal("回退时应清掉 .mrp.json")
	}
}

// ---------- 远端文件变更 -> 凭证作废、整体重下 ----------

func TestMultiRangeValidatorInvalidation(t *testing.T) {
	const size = 6 << 20
	const workers = 4
	data := makeData(size)
	chunks := splitChunks(size, workers)
	last := chunks[len(chunks)-1]

	s := newRangeServer(t, data, last.Start, 1<<30)
	dir := t.TempDir()
	opt := Options{MediaURL: s.URL("/big.bin"), OutDir: dir, FileName: "big.bin", Workers: workers}
	out := filepath.Join(dir, "big.bin")

	if _, err := Download(context.Background(), opt, nil); err == nil {
		t.Fatal("应失败并留下凭证")
	}

	// 远端文件的 Last-Modified 变了：旧分块一个都不能信。
	s.mu.Lock()
	s.mut = s.mut.Add(48 * time.Hour)
	s.mu.Unlock()
	s.setFail(-1, 0)
	s.resetRanges()

	if _, err := Download(context.Background(), opt, nil); err != nil {
		t.Fatalf("重下失败: %v", err)
	}
	if got := len(s.rangeRequests()); got != len(chunks) {
		t.Fatalf("校验标识变化后应重下全部 %d 块，实际 %d 块", len(chunks), got)
	}
	b, _ := os.ReadFile(out)
	if !bytes.Equal(b, data) {
		t.Fatal("内容不一致")
	}
}

// ---------- 小文件仍走单连接（不应被多线程路径接管）----------

func TestSmallFileStaysSingleConnection(t *testing.T) {
	data := makeData(512 << 10) // 小于 2MB 阈值
	s := newRangeServer(t, data, -1, 0)
	dir := t.TempDir()
	out, err := Download(context.Background(), Options{
		MediaURL: s.URL("/small.bin"), OutDir: dir, FileName: "small.bin", Workers: 8,
	}, nil)
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if got := len(s.rangeRequests()); got != 0 {
		t.Fatalf("小文件不应发 Range 请求，实际 %d 个", got)
	}
	b, _ := os.ReadFile(out)
	if !bytes.Equal(b, data) {
		t.Fatal("内容不一致")
	}
}

// ---------- 单元：凭证解析与校验标识 ----------

func TestRangeStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "v.mp4")
	s := rangeState{Total: 123, ChunkSize: 41, Validator: "lm:x", Done: []bool{true, false, true}}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(multiRangeStatePath(out), data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadRangeState(multiRangeStatePath(out))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != s.Total || got.ChunkSize != s.ChunkSize || got.Validator != s.Validator || len(got.Done) != 3 || !got.Done[0] || got.Done[1] {
		t.Fatalf("往返不一致: %+v", got)
	}
	if multiRangePartPath(out) == out+".part" {
		t.Fatal(".mrp 必须与单连接的 .part 区分开")
	}
}

func TestEtagOrLastModified(t *testing.T) {
	h := http.Header{}
	if etagOrLastModified(h) != "" {
		t.Fatal("空头应返回空串")
	}
	h.Set("ETag", `"abc"`)
	if got := etagOrLastModified(h); got != `etag:"abc"` {
		t.Fatalf("ETag 优先: %q", got)
	}
	h = http.Header{}
	h.Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
	if got := etagOrLastModified(h); !strings.HasPrefix(got, "lm:") {
		t.Fatalf("Last-Modified 兜底: %q", got)
	}
}
