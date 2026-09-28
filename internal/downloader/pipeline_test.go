package downloader

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// 本地 HLS 桩：master(2档) + media(AES-128,3分片) + key，验证解析/下载/解密/续传。
func TestPipelineLocal(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := []string{"SEGMENT-ZERO-!!!!", "SEGMENT-ONE-!!!!!", "SEGMENT-TWO-!!!!!"}

	var hits int64
	mux := http.NewServeMux()
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=640x360\n/low.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=5000,RESOLUTION=1280x720\n/hi.m3u8\n")
	})
	mux.HandleFunc("/hi.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-KEY:METHOD=AES-128,URI=\"/key\"\n#EXTINF:10,\n/s0.ts\n#EXTINF:10,\n/s1.ts\n#EXTINF:10,\n/s2.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/low.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) { w.Write(key) })
	enc := func(i int) []byte {
		iv := make([]byte, 16)
		iv[15] = byte(i) // 与下载端缺省 IV 规则一致
		block, _ := aes.NewCipher(key)
		p := []byte(plain[i])
		p = append(p, bytes.Repeat([]byte{byte(16 - len(p)%16)}, 16-len(p)%16)...)
		out := make([]byte, len(p))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, p)
		return out
	}
	mux.HandleFunc("/s0.ts", func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&hits, 1); w.Write(enc(0)) })
	mux.HandleFunc("/s1.ts", func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&hits, 1); w.Write(enc(1)) })
	mux.HandleFunc("/s2.ts", func(w http.ResponseWriter, r *http.Request) { atomic.AddInt64(&hits, 1); w.Write(enc(2)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()

	// 1. 清晰度档位：应 2 档且按带宽降序
	vars, err := ParseVariants(ctx, srv.URL+"/master.m3u8", "", "", "")
	if err != nil || len(vars) != 2 || vars[0].Bandwidth != 5000 || vars[0].Resolution != "1280x720" {
		t.Fatalf("ParseVariants 错误: %v %+v", err, vars)
	}

	// 2. master 自动选最高档 + 分片解析
	_, segs, err := resolveSegments(ctx, newClient(), srv.URL+"/master.m3u8", Headers{UserAgent: "UT"})
	if err != nil || len(segs) != 3 {
		t.Fatalf("resolveSegments 错误: %v (%d)", err, len(segs))
	}

	// 3. 下载 + 解密
	dir := t.TempDir()
	opt := Options{MediaURL: srv.URL + "/hi.m3u8", Workers: 2}
	paths, err := fetchSegments(ctx, newClient(), opt, segs, dir, nil)
	if err != nil {
		t.Fatalf("fetchSegments 错误: %v", err)
	}
	for i, p := range paths {
		b, _ := os.ReadFile(p)
		if string(b) != plain[i] {
			t.Fatalf("分片 %d 解密不对: %q", i, b)
		}
	}
	if atomic.LoadInt64(&hits) != 3 {
		t.Fatalf("分片请求数不对: %d", hits)
	}

	// 4. 续传：删掉 s1，重新拉应只请求 1 次
	os.Remove(segPath(dir, 1))
	atomic.StoreInt64(&hits, 0)
	if _, err := fetchSegments(ctx, newClient(), opt, segs, dir, nil); err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Fatalf("续传应只下 1 个分片，实际 %d", hits)
	}
	b, _ := os.ReadFile(segPath(dir, 1))
	if string(b) != plain[1] {
		t.Fatalf("续传分片内容不对: %q", b)
	}

	// 5. manifest 匹配逻辑
	state := t.TempDir()
	if manifestMatch(state, opt.MediaURL, segs) {
		t.Fatal("空目录不应匹配")
	}
	if err := saveManifest(state, opt.MediaURL, segs); err != nil {
		t.Fatal(err)
	}
	if !manifestMatch(state, opt.MediaURL, segs) {
		t.Fatal("同清单应匹配")
	}
	if _, err := os.Stat(filepath.Join(state, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}
