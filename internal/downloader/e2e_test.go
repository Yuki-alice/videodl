package downloader

import (
	"context"
	"os"
	"testing"
	"time"
)

// 端到端 DASH：公开测试 MPD -> 最低档 -> 音视频下载 -> 混流。
func TestE2EDash(t *testing.T) {
	if os.Getenv("VIDEODL_E2E") == "" {
		t.Skip("set VIDEODL_E2E=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	mpd := "https://dash.akamaized.net/akamai/bbb_30fps/bbb_30fps.mpd"

	vars, err := ParseDashVariants(ctx, mpd, Headers{UserAgent: "Mozilla/5.0"})
	if err != nil || len(vars) == 0 {
		t.Fatalf("ParseDashVariants: %v", err)
	}
	t.Logf("档位 %d 档，最低: %s", len(vars), vars[len(vars)-1].Label)
	low := vars[len(vars)-1]

	outDir := t.TempDir()
	out, err := DownloadTask(ctx, Options{MediaURL: low.URL, OutDir: outDir, Workers: 16}, t.TempDir(), func(d, n int) {
		if n > 0 && (d == 1 || d == n || d%50 == 0) {
			t.Logf("进度 %d/%d", d, n)
		}
	})
	if err != nil {
		t.Fatalf("DownloadTask: %v", err)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() < 1<<20 {
		t.Fatalf("产物异常: %v size=%d", err, st.Size())
	}
	t.Logf("OK: %s (%.1fMB)", out, float64(st.Size())/(1<<20))
}
// 需外网，仅 VIDEODL_E2E=1 时跑，避免 CI flaky。
func TestE2EPublic(t *testing.T) {
	if os.Getenv("VIDEODL_E2E") == "" {
		t.Skip("set VIDEODL_E2E=1 to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	master := "https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8"

	vars, err := ParseVariants(ctx, master, "", "", "")
	if err != nil || len(vars) == 0 {
		t.Fatalf("ParseVariants: %v", err)
	}
	t.Logf("档位 %d 档，最低: %s", len(vars), vars[len(vars)-1].Label)
	low := vars[len(vars)-1]

	outDir := t.TempDir()
	out, err := DownloadTask(ctx, Options{MediaURL: low.URL, OutDir: outDir, Workers: 16}, t.TempDir(), func(d, n int) {
		if n > 0 && (d == 1 || d == n || d%100 == 0) {
			t.Logf("进度 %d/%d", d, n)
		}
	})
	if err != nil {
		t.Fatalf("DownloadTask: %v", err)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() < 1<<20 {
		t.Fatalf("产物异常: %v size=%d", err, st.Size())
	}
	t.Logf("OK: %s (%.1fMB)", out, float64(st.Size())/(1<<20))
}
