package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errRangeUnsupported 表示服务端明明声明支持 Range、实际却返回 200，
// 调用方应回退到单连接下载。
var errRangeUnsupported = errors.New("服务端未按 Range 响应")

// multiRangeThreshold 低于这个体积不值得开多线程：
// 多开若干请求的握手/TLS 开销会盖过并行收益。
const multiRangeThreshold = 2 << 20 // 2MB

// minChunkSize 单块下限，避免把文件切得太碎导致请求数爆炸。
const minChunkSize = 1 << 20 // 1MB

// rangeChunkTries 单个分块的尝试次数（首次 + 重试），与 HLS 分片路径保持一致。
const rangeChunkTries = 3

// rangeChunk 一个 Range 分块，End 为闭区间末字节（HTTP Range 语义）。
type rangeChunk struct {
	Index int
	Start int64
	End   int64
}

func (c rangeChunk) size() int64 { return c.End - c.Start + 1 }

// splitChunks 把 total 字节切成不超过 workers 块，每块至少 minChunkSize。
func splitChunks(total int64, workers int) []rangeChunk {
	if total <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	n := int64(workers)
	if n*minChunkSize > total {
		n = total / minChunkSize
	}
	if n < 1 {
		n = 1
	}
	chunkSize := (total + n - 1) / n
	var out []rangeChunk
	for start := int64(0); start < total; start += chunkSize {
		end := start + chunkSize - 1
		if end > total-1 {
			end = total - 1
		}
		out = append(out, rangeChunk{Index: len(out), Start: start, End: end})
	}
	return out
}

// rangeState 多线程下载的续传凭证，落在 <out>.mrp.json。
// Validator 存 ETag 或 Last-Modified：远端文件变了就整体作废重下，
// 避免拿旧分块拼出新旧混杂的坏文件。
type rangeState struct {
	Total     int64  `json:"total"`
	ChunkSize int64  `json:"chunkSize"`
	Validator string `json:"validator"`
	Done      []bool `json:"done"`
}

// downloadMultiRange 多线程 Range 分块下载。
// 全部完成才由调用方改名；中断或部分失败时，已完成分块记在凭证里，重跑即续传。
// out 是最终成片路径，临时数据与凭证由它派生（<out>.mrp / <out>.mrp.json）。
func downloadMultiRange(
	ctx context.Context,
	client *http.Client,
	opt Options,
	h Headers,
	total int64,
	validator string,
	out string,
	on Progress,
) error {
	part := multiRangePartPath(out)
	statePath := multiRangeStatePath(out)

	chunks := splitChunks(total, opt.Workers)
	if len(chunks) == 0 {
		return errRangeUnsupported
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	done := make([]bool, len(chunks))
	var carried int64

	// 只有「尺寸对得上 + total/chunkSize/validator/分块数全一致」才复用旧进度。
	st, statErr := os.Stat(part)
	if statErr == nil && st.Size() == total {
		if s, serr := loadRangeState(statePath); serr == nil &&
			s.Total == total &&
			s.ChunkSize == chunks[0].size() &&
			s.Validator == validator &&
			len(s.Done) == len(done) {
			copy(done, s.Done)
			for i, ok := range done {
				if ok {
					carried += chunks[i].size()
				}
			}
		}
	} else if err := f.Truncate(total); err != nil {
		// 首次下载或残留文件尺寸不符：预分配并从头开始。
		return err
	}

	var (
		completed atomic.Int64
		written   atomic.Int64
		mu        sync.Mutex // 同时保护 done 与 firstErr，二者必须共用一把锁
		firstErr  error
	)
	completed.Store(carried)
	written.Store(carried)
	if on != nil {
		on(int(completed.Load()), int(total))
	}

	// saveState 在 mu 内完成「快照 + 落盘」：
	// done 只在 mu 下读写；落盘也必须在锁内串行，否则多个 worker 并发写
	// 同一个文件会写出交错损坏的 JSON。先写 .tmp 再 rename，保证原子替换
	// （进程被杀时宁可留下旧凭证，也不留半截的）。
	saveState := func() {
		mu.Lock()
		defer mu.Unlock()
		s := rangeState{Total: total, ChunkSize: chunks[0].size(), Validator: validator, Done: done}
		data, merr := json.Marshal(s)
		if merr != nil {
			return
		}
		tmp := statePath + ".tmp"
		if werr := os.WriteFile(tmp, data, 0o644); werr != nil {
			return
		}
		_ = os.Rename(tmp, statePath)
	}
	if carried > 0 {
		saveState()
	}

	workers := opt.Workers
	if workers > len(chunks) {
		workers = len(chunks)
	}
	if workers < 1 {
		workers = 1
	}

	// 分派前一次性确定待下载清单：此后不再读 done，
	// 避免 dispatcher 与 worker 并发访问同一 slice。
	var todo []rangeChunk
	for i, c := range chunks {
		if !done[i] {
			todo = append(todo, c)
		}
	}

	pending := make(chan rangeChunk)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range pending {
				if ctx.Err() != nil {
					return
				}
				cerr := fetchRangeChunk(ctx, client, opt.MediaURL, h, f, c, func(n int64) {
					if on != nil {
						on(int(written.Add(n)), int(total))
					}
				})
				if cerr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = cerr
					}
					mu.Unlock()
					if errors.Is(cerr, errRangeUnsupported) {
						return // 配置级问题，没必要继续其它分块
					}
					continue
				}
				mu.Lock()
				done[c.Index] = true
				mu.Unlock()
				if on != nil {
					on(int(completed.Add(c.size())), int(total))
				}
				saveState()
			}
		}()
	}

	go func() {
		defer close(pending)
		for _, c := range todo {
			select {
			case <-ctx.Done():
				return
			case pending <- c:
			}
		}
	}()
	wg.Wait()

	if ctx.Err() != nil {
		return ctx.Err() // 保留进度供续传
	}
	if errors.Is(firstErr, errRangeUnsupported) {
		return errRangeUnsupported
	}
	if firstErr != nil {
		return firstErr
	}
	for i, ok := range done {
		if !ok {
			return fmt.Errorf("分块 %d 未完成", i)
		}
	}
	return nil
}

// fetchRangeChunk 拉一个分块，带指数退避重试。
// 重试时用 WriteAt 从原偏移覆盖，数据始终正确；进度先回滚再重报，避免虚高。
func fetchRangeChunk(
	ctx context.Context,
	client *http.Client,
	rawURL string,
	h Headers,
	f *os.File,
	c rangeChunk,
	onBytes func(int64),
) error {
	var (
		reported int64
		err      error
	)
	rollback := func() {
		if reported > 0 && onBytes != nil {
			onBytes(-reported)
		}
		reported = 0
	}
	for i := 0; i < rangeChunkTries; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rollback()
		err = fetchRangeChunkOnce(ctx, client, rawURL, h, f, c, func(n int64) {
			reported += n
			if onBytes != nil {
				onBytes(n)
			}
		})
		if err == nil {
			return nil
		}
		if errors.Is(err, errRangeUnsupported) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<i) * time.Second):
		}
	}
	return err
}

// fetchRangeChunkOnce 单次分块请求。
// os.File.WriteAt 不共享文件游标，多个 goroutine 并发写不同偏移是安全的。
func fetchRangeChunkOnce(
	ctx context.Context,
	client *http.Client,
	rawURL string,
	h Headers,
	f *os.File,
	c rangeChunk,
	onBytes func(int64),
) error {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return err
	}
	h.apply(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", c.Start, c.End))

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// 正常
	case http.StatusOK:
		return errRangeUnsupported // 服务端无视 Range，返回了全量
	default:
		return fmt.Errorf("分块 %d HTTP %d", c.Index, resp.StatusCode)
	}

	body := io.Reader(resp.Body)
	if speedLimit.Load() > 0 {
		body = pacedReader{resp.Body}
	}
	buf := make([]byte, 256<<10)
	off := c.Start
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
			off += int64(n)
			if onBytes != nil {
				onBytes(int64(n))
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return rerr
		}
	}
	if got := off - c.Start; got != c.size() {
		return fmt.Errorf("分块 %d 长度不足: %d/%d", c.Index, got, c.size())
	}
	return nil
}

func loadRangeState(path string) (*rangeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s rangeState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// etagOrLastModified 取远端文件校验标识，两者都无则返回空串（此时不做变更检测）。
func etagOrLastModified(h http.Header) string {
	if v := h.Get("ETag"); v != "" {
		return "etag:" + v
	}
	if v := h.Get("Last-Modified"); v != "" {
		return "lm:" + v
	}
	return ""
}

// probeRemote 用 HEAD 探远端体积与 Range 支持情况。
// total < 0 表示探不到（服务端不支持 HEAD），调用方回退到 GET 自行判断。
func probeRemote(ctx context.Context, client *http.Client, rawURL string, h Headers) (total int64, acceptRanges bool, validator string) {
	total = -1
	req, err := http.NewRequestWithContext(ctx, "HEAD", rawURL, nil)
	if err != nil {
		return
	}
	h.apply(req)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return
	}
	if resp.ContentLength >= 0 {
		total = resp.ContentLength
	}
	acceptRanges = strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	validator = etagOrLastModified(resp.Header)
	return
}

// multiRangePartPath 多线程下载的数据文件路径，与单连接的 .part 显式区分，
// 避免两种模式的残留互相误判成有效续传数据。
func multiRangePartPath(out string) string { return out + ".mrp" }

// multiRangeStatePath 多线程下载的续传凭证路径。
func multiRangeStatePath(out string) string { return out + ".mrp.json" }
