package downloader

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// 直链 Range 续传：完整下载 -> 模拟中断留半截 .part -> 续传只拉后半段。
func TestDirectFileResume(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789ABCDEF"), 64*1024) // 1MB
	var mu sync.Mutex
	var ranges []string
	mux := http.NewServeMux()
	mux.HandleFunc("/f.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if rng := r.Header.Get("Range"); rng != "" {
			mu.Lock()
			ranges = append(ranges, rng)
			mu.Unlock()
			var start int
			fmt.Sscanf(rng, "bytes=%d-", &start)
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-start))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[start:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Write(data)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx := context.Background()
	dir := t.TempDir()
	opt := Options{MediaURL: srv.URL + "/f.mp4", OutDir: dir, FileName: "f.mp4", Workers: 1}

	// 完整下载
	out, err := Download(ctx, opt, nil)
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, data) {
		t.Fatal("完整下载内容不对")
	}

	// 模拟中断：删成片，留一半 .part
	os.Remove(out)
	half := data[:len(data)/2]
	if err := os.WriteFile(out+".part", half, 0o644); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ranges = nil
	mu.Unlock()
	out2, err := Download(ctx, opt, nil)
	if err != nil || out2 != out {
		t.Fatalf("续传失败: %v", err)
	}
	mu.Lock()
	n := len(ranges)
	mu.Unlock()
	if n != 1 || !strings.HasPrefix(ranges[0], "bytes=524288-") {
		t.Fatalf("续传没用 Range 或起点不对: %v", ranges)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, data) {
		t.Fatal("续传后内容不对")
	}
}
