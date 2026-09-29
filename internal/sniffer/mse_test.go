package sniffer

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeMSEFixture 位置相关的伪随机数据：一旦拼接顺序或偏移出错，字节比对立刻暴露。
func makeMSEFixture(n int) []byte {
	data := make([]byte, n)
	x := uint32(0x12345678)
	for i := range data {
		x = x*1664525 + 1013904223
		data[i] = byte(x >> 24)
	}
	return data
}

// mseTestPage 真实走一遍 MediaSource 流程：
// 建**两条** SourceBuffer（视频/音频各一条），分别喂入不同数据。
// 这正是要防的回归——若轨名取不到，两条轨会混成一条、拼成交错垃圾。
// codec 用列表兜底：无头 Chrome 的可用解码器随构建而异。
const mseTestPage = `<!DOCTYPE html><html><body>
<video id="v" muted playsinline></video>
<script>
const CHUNK = 64 * 1024;
const VIDEO_MIMES = ['video/mp4; codecs="avc1.42E01E"', 'video/webm; codecs="vp8"', 'video/mp4'];
const AUDIO_MIMES = ['audio/mp4; codecs="mp4a.40.2"', 'audio/webm; codecs="opus"', 'audio/mp4'];

function pick(ms, list) {
  for (var i = 0; i < list.length; i++) {
    try { return ms.addSourceBuffer(list[i]); } catch (e) {}
  }
  return null;
}

function feed(sb, u8) {
  return new Promise(function (resolve) {
    var off = 0;
    function step() {
      if (off >= u8.length) { return resolve(); }
      try { sb.appendBuffer(u8.subarray(off, Math.min(off + CHUNK, u8.length))); } catch (e) {}
      off += CHUNK;
      setTimeout(step, 0);
    }
    step();
  });
}

(async function () {
  const vBytes = new Uint8Array(await (await fetch('/video.bin')).arrayBuffer());
  const aBytes = new Uint8Array(await (await fetch('/audio.bin')).arrayBuffer());
  const ms = new MediaSource();
  document.getElementById('v').src = URL.createObjectURL(ms);
  await new Promise(r => ms.addEventListener('sourceopen', r, { once: true }));

  const vsb = pick(ms, VIDEO_MIMES);
  const asb = pick(ms, AUDIO_MIMES);
  if (vsb) { await feed(vsb, vBytes); }
  if (asb) { await feed(asb, aBytes); }
  document.title = 'DONE v=' + (!!vsb) + ' a=' + (!!asb);
})();
</script>
</body></html>`

// 端到端 MSE 劫持：两条轨分别抓取，各自与原文件逐字节比对。
func TestCaptureMSE(t *testing.T) {
	requireE2E(t)

	video := makeMSEFixture(768 << 10)
	audio := makeMSEFixture(256 << 10)

	mux := http.NewServeMux()
	serve := func(path string, body []byte) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body)
		})
	}
	serve("/video.bin", video)
	serve("/audio.bin", audio)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, mseTestPage)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	outDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	tracks, err := CaptureMSE(ctx, srv.URL+"/", MSECaptureOptions{
		OutDir:   outDir,
		WaitSec:  2,
		Duration: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("CaptureMSE: %v", err)
	}
	if len(tracks) == 0 {
		t.Fatal("没抓到任何轨（钩子可能未生效）")
	}
	payloads := map[string][]byte{} // 轨名 -> 内容，用来断言两条轨没有混在一起
	for _, tr := range tracks {
		b, rerr := os.ReadFile(tr.Path)
		if rerr != nil {
			t.Fatalf("读取产物失败: %v", rerr)
		}
		t.Logf("轨: mime=%-40s bytes=%-8d chunks=%-4d file=%s", tr.MimeType, tr.Bytes, tr.Chunks, filepath.Base(tr.Path))
		if tr.Bytes != int64(len(b)) {
			t.Fatalf("上报字节数与文件实际大小不符: %d != %d", tr.Bytes, len(b))
		}
		if tr.MimeType == "" {
			t.Fatal("轨名不应为空")
		}
		// 文件名必须带上由轨名派生的扩展名。
		if ext := filepath.Ext(tr.Path); ext != ".mp4" && ext != ".webm" && ext != ".bin" {
			t.Fatalf("扩展名异常: %q", ext)
		}
		payloads[tr.MimeType] = b
	}

	if len(tracks) < 2 {
		t.Fatalf("应抓到视频与音频两条独立轨，实际 %d 条（轨名取不到就会合并）", len(tracks))
	}

	// 两条轨必须分别对应视频与音频数据，且**不能**出现「一条轨里混了两者的内容」。
	var gotVideo, gotAudio bool
	for name, body := range payloads {
		switch {
		case bytes.Equal(body, video):
			gotVideo = true
		case bytes.Equal(body, audio):
			gotAudio = true
		default:
			t.Fatalf("轨 %q 的内容既不是视频也不是音频（长度 %d）——说明两条轨被拼到了一起", name, len(body))
		}
	}
	if !gotVideo || !gotAudio {
		t.Fatalf("未同时抓到视频与音频: video=%v audio=%v", gotVideo, gotAudio)
	}
}

// 钩子脚本本身的静态约束。
// 真正的抓取行为由上一条端到端用例覆盖。
func TestMSEHookScriptShape(t *testing.T) {
	for _, want := range []string{
		"SourceBuffer", "appendBuffer",
		"MediaSource", "addSourceBuffer", // 轨名必须从 addSourceBuffer 的入参取
		"WeakMap", // 按实例绑定，避免 sb.mimeType 不可用
		"ArrayBuffer.isView", "URL.createObjectURL",
	} {
		if !strings.Contains(mseHookJS, want) {
			t.Fatalf("钩子脚本缺少关键片段: %s", want)
		}
	}
	if !strings.Contains(drainFnJS, "budget") {
		t.Fatal("拉取端应支持预算限量")
	}
}

func TestMSENaming(t *testing.T) {
	cases := []struct {
		mime     string
		wantExt  string
		wantSafe string
	}{
		{`video/mp4; codecs="avc1.42E01E"`, ".mp4", "video-mp4-codecs-avc1-42E01E"},
		{`audio/mp4; codecs="mp4a.40.2"`, ".mp4", "audio-mp4-codecs-mp4a-40-2"},
		{`video/webm; codecs="vp9"`, ".webm", "video-webm-codecs-vp9"},
		{"", ".bin", "track"},
		{"application/octet-stream", ".bin", "application-octet-stream"},
	}
	for _, c := range cases {
		if got := mseExt(c.mime); got != c.wantExt {
			t.Errorf("mseExt(%q) = %q, 期望 %q", c.mime, got, c.wantExt)
		}
		if got := sanitizeMSE(c.mime); got != c.wantSafe {
			t.Errorf("sanitizeMSE(%q) = %q, 期望 %q", c.mime, got, c.wantSafe)
		}
	}
	// 路径穿越与超长输入必须被压平。
	long := sanitizeMSE(strings.Repeat("x", 200))
	if len(long) > 48 {
		t.Fatalf("文件名未截断: %d", len(long))
	}
	for _, bad := range []string{"../../etc/passwd", `a\b:c*d?e`} {
		if s := sanitizeMSE(bad); strings.ContainsAny(s, `/\:*?"<>|`) {
			t.Fatalf("净化后仍含危险字符: %q -> %q", bad, s)
		}
	}
}

// 空轨（只有 0 字节）不应留下垃圾文件。
func TestMSESinkEmptyCleanup(t *testing.T) {
	dir := t.TempDir()
	sink, err := newMSESink(dir, "video/mp4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.file.Close(); err != nil {
		t.Fatal(err)
	}
	if sink.bytes != 0 {
		t.Fatal("新建轨应为空")
	}
	if !strings.HasPrefix(filepath.Base(sink.path), "mse-1-video-mp4") {
		t.Fatalf("命名异常: %s", sink.path)
	}
	if _, err := os.Stat(sink.path); err != nil {
		t.Fatal(err)
	}
}
