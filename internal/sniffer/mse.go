package sniffer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// MSETrack 一次 MSE 劫持抓到的一条轨（通常视频/音频各一条）。
// 同一条轨的所有 appendBuffer 载荷按到达顺序拼接即为完整媒体流：
// fMP4 的第一个载荷是 init 段（含 moov），所以拼出来可直接播放。
type MSETrack struct {
	MimeType string `json:"mimeType"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	Chunks   int    `json:"chunks"`
}

// MSECaptureOptions MSE 劫持参数。
type MSECaptureOptions struct {
	OutDir        string        // 落盘目录
	WaitSec       int           // 打开页面后等播放器起播的秒数，默认 8
	Duration      time.Duration // 采集窗口，默认 20s
	MaxTotalBytes int64         // 总采集上限，默认 1GB，防止无节制写盘
}

// CaptureMSE 打开页面并把 MSE 喂给播放器的数据抓回本地。
//
// 这是「blob 回源」的真正解法：blob: 只是内存句柄，数据实际经由
// SourceBuffer.appendBuffer() 进入播放器，所以在那里复制一份就能拿到完整流。
// 对「能正常播放但嗅不到任何 URL」的站点，这是唯一的出路。
func CaptureMSE(ctx context.Context, pageURL string, o MSECaptureOptions) ([]MSETrack, error) {
	if o.OutDir == "" {
		return nil, fmt.Errorf("OutDir 为空")
	}
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return nil, err
	}
	if o.WaitSec <= 0 {
		o.WaitSec = 8
	}
	if o.Duration <= 0 {
		o.Duration = 20 * time.Second
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = 1 << 30
	}

	bin, err := findBrowser()
	if err != nil {
		return nil, err
	}
	launchURL, err := launcher.New().Bin(bin).Headless(true).
		Set("autoplay-policy", "no-user-gesture-required").
		Set("mute-audio").
		Launch()
	if err != nil {
		return nil, fmt.Errorf("无头浏览器启动失败: %w", err)
	}
	browser := rod.New().ControlURL(launchURL)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("连接无头浏览器失败: %w", err)
	}
	defer func() { _ = browser.Close() }()

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("建新标签页失败: %w", err)
	}
	defer func() { _ = page.Close() }()
	page = page.Timeout(90 * time.Second)

	if _, err := page.EvalOnNewDocument(mseHookJS); err != nil {
		return nil, fmt.Errorf("注入 MSE 钩子失败: %w", err)
	}
	if err := page.Navigate(pageURL); err != nil {
		return nil, fmt.Errorf("打开页面失败: %w", err)
	}
	_ = page.WaitLoad()

	// 给播放器留出初始化时间，再补一刀静音自动播放逼它开始喂数据。
	if !sleepCtx(ctx, time.Duration(o.WaitSec)*time.Second) {
		return nil, ctx.Err()
	}
	_, _ = page.Eval(`() => [...document.querySelectorAll('video,audio')].forEach(v=>{try{v.muted=true;v.play().catch(()=>{});}catch(e){}})`)
	sinks := map[string]*mseSink{}
	closed := false
	closeAll := func() {
		if closed {
			return
		}
		closed = true
		for _, s := range sinks {
			_ = s.file.Close()
		}
	}
	defer closeAll()

	deadline := time.Now().Add(o.Duration)
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()

	var (
		captured    int64
		emptyStreak int
	)
loop:
	for {
		if ctx.Err() != nil {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
		}

		n, err := drainMSE(page, sinks, o)
		if err != nil {
			// 页面跳转或关闭会让 eval 失败；此时结束采集，已抓到的仍然有效。
			break
		}
		captured += n
		if n == 0 {
			emptyStreak++
			// 已经有数据、且连续 3 秒没有新载荷，认为播放器不再产出。
			if emptyStreak >= 10 && captured > 0 {
				break
			}
			continue
		}
		emptyStreak = 0
		if captured >= o.MaxTotalBytes {
			break
		}
	}

	closeAll() // 先关闭再把空轨删掉（Windows 上打开中的文件删不掉）

	out := make([]MSETrack, 0, len(sinks))
	for _, s := range sinks {
		if s.bytes == 0 {
			_ = os.Remove(s.path)
			continue
		}
		out = append(out, MSETrack{
			MimeType: s.mimeType, Path: s.path, Bytes: s.bytes, Chunks: s.chunks,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out, nil
}

// mseSink 一条轨的落盘目标。
type mseSink struct {
	mimeType string
	path     string
	file     *os.File
	bytes    int64
	chunks   int
}

func newMSESink(outDir, mimeType string, idx int) (*mseSink, error) {
	name := fmt.Sprintf("mse-%d-%s%s", idx+1, sanitizeMSE(mimeType), mseExt(mimeType))
	path := filepath.Join(outDir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &mseSink{mimeType: mimeType, path: path, file: f}, nil
}

// drainMSE 排空 JS 侧队列，把本 tick 抓到的载荷写入对应轨文件。
// 单次预算有限，所以在一个 tick 内循环多次直到队列排空。
func drainMSE(page *rod.Page, sinks map[string]*mseSink, o MSECaptureOptions) (int64, error) {
	const perCall = 512 << 10 // base64 字符预算
	const maxCalls = 16       // 单 tick 上限，约 8MB base64，避免长时间占住主线程

	var total int64
	for i := 0; i < maxCalls; i++ {
		raw, err := evalString(page, drainFnJS, perCall)
		if err != nil {
			return total, err
		}
		if raw == "" || raw == "{}" {
			break
		}
		var batch map[string][]string
		if err := json.Unmarshal([]byte(raw), &batch); err != nil {
			return total, fmt.Errorf("解析 MSE 载荷失败: %w", err)
		}
		for mime, chunks := range batch {
			sink, ok := sinks[mime]
			if !ok {
				s, err := newMSESink(o.OutDir, mime, len(sinks))
				if err != nil {
					return total, err
				}
				sinks[mime] = s
				sink = s
			}
			for _, enc := range chunks {
				b, derr := base64.StdEncoding.DecodeString(enc)
				if derr != nil {
					continue // 坏块跳过，不因单块失败放弃整轨
				}
				if o.MaxTotalBytes > 0 && total >= o.MaxTotalBytes {
					return total, nil
				}
				if _, werr := sink.file.Write(b); werr != nil {
					return total, werr
				}
				sink.bytes += int64(len(b))
				sink.chunks++
				total += int64(len(b))
			}
		}
	}
	return total, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// sanitizeMSE 把 mimeType 压成安全的文件名片段。
func sanitizeMSE(m string) string {
	var b strings.Builder
	for _, r := range m {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		s = "track"
	}
	return s
}

// mseExt 按 mimeType 猜输出扩展名。fMP4 拼出来就是 .mp4，可直接播放。
func mseExt(m string) string {
	l := strings.ToLower(m)
	switch {
	case strings.Contains(l, "webm"):
		return ".webm"
	case strings.Contains(l, "mp4"), strings.Contains(l, "m4s"):
		return ".mp4"
	case strings.Contains(l, "mp2t"):
		return ".ts"
	default:
		return ".bin"
	}
}

// mseHookJS 在 document 创建前注入。
// 钩子只做「复制 + 入队」，不做任何解码，避免拖慢播放器。
//
// 关键点：轨名（mimeType）必须在 addSourceBuffer(mime) 时就记下来。
// 本机实测部分 Chrome 构建下 `sb.mimeType` 为 undefined 且
// SourceBuffer.prototype 上没有该属性，只靠它会导致视频/音频两条轨
// 混进同一个键、拼接成交错垃圾。用 WeakMap 按实例绑定才可靠。
const mseHookJS = `(function(){
  if (window.__vl_mse) { return; }
  var st = { tracks: {} };
  window.__vl_mse = st;

  var mimeMap = null, anonMap = null, anonSeq = 0;
  try { mimeMap = new WeakMap(); anonMap = new WeakMap(); } catch (e) {}

  function toB64(u8) {
    var s = '', CH = 0x8000;
    for (var i = 0; i < u8.length; i += CH) {
      s += String.fromCharCode.apply(null, u8.subarray(i, i + CH));
    }
    return btoa(s);
  }

  function asBytes(buf) {
    try {
      if (buf instanceof ArrayBuffer) { return new Uint8Array(buf); }
      if (ArrayBuffer.isView(buf)) {
        return new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength);
      }
    } catch (e) {}
    return null;
  }

  // keyOf 给每个 SourceBuffer 一个稳定且互不相同的轨名。
  // 最坏情况退回按实例编号的匿名名，绝不把两条轨合并。
  function keyOf(sb) {
    try { if (mimeMap) { var m = mimeMap.get(sb); if (m) { return m; } } } catch (e) {}
    try { if (sb && sb.mimeType) { return String(sb.mimeType); } } catch (e) {}
    try {
      if (anonMap) {
        var a = anonMap.get(sb);
        if (!a) { a = 'unknown-track-' + (++anonSeq); anonMap.set(sb, a); }
        return a;
      }
    } catch (e) {}
    return 'unknown-track';
  }

  function push(key, u8) {
    if (!u8 || !u8.length) { return; }
    var k = key || 'application/octet-stream';
    var t = st.tracks[k];
    if (!t) { t = st.tracks[k] = { q: [], n: 0, b: 0 }; }
    try { t.q.push(toB64(u8)); } catch (e) { return; }
    t.n++; t.b += u8.length;
  }

  function hookMediaSource() {
    var proto = window.MediaSource && window.MediaSource.prototype;
    if (!proto || !proto.addSourceBuffer || proto.__vlMSHooked) { return; }
    var orig = proto.addSourceBuffer;
    proto.addSourceBuffer = function (mime) {
      var sb = orig.apply(this, arguments);
      try {
        var mt = null;
        try { mt = sb && sb.mimeType; } catch (e) {}
        if (!mt) { mt = mime; } // 实例属性不可用时，用调用方传入的 MIME
        if (mt && mimeMap) { mimeMap.set(sb, String(mt)); }
      } catch (e) {}
      return sb;
    };
    proto.__vlMSHooked = true;
  }

  function hookSourceBuffer() {
    var proto = window.SourceBuffer && window.SourceBuffer.prototype;
    if (!proto || !proto.appendBuffer || proto.__vlSBHooked) { return; }
    var orig = proto.appendBuffer;
    proto.appendBuffer = function (buf) {
      try { push(keyOf(this), asBytes(buf)); } catch (e) {}
      return orig.apply(this, arguments);
    };
    proto.__vlSBHooked = true;
  }

  function install() { hookMediaSource(); hookSourceBuffer(); }

  install();
  document.addEventListener('DOMContentLoaded', install);

  // 兜底：不少站点的 blob 是整个文件一次性做出来的，用 createObjectURL 直接读全量。
  try {
    if (window.URL && URL.createObjectURL) {
      var _create = URL.createObjectURL.bind(URL);
      URL.createObjectURL = function (o) {
        try {
          if (o instanceof Blob && /^(video|audio)\//i.test(o.type || '')) {
            o.arrayBuffer().then(function (ab) {
              push(String(o.type), new Uint8Array(ab));
            }).catch(function () {});
          }
        } catch (e) {}
        return _create.apply(URL, arguments);
      };
    }
  } catch (e) {}
})();
`

// drainFnJS 供 Go 侧按预算批量取走队列，是「JS 入队 / Go 拉取」的拉取端。
// 用拉而不是推（绑定回调），是为了避免在播放线程里做 IPC。
// 必须是函数形式（rod 会对它调用 .apply），预算通过参数传入。
const drainFnJS = `(budget) => {
  var st = window.__vl_mse;
  if (!st) { return '{}'; }
  var out = {}, left = budget;
  for (var k in st.tracks) {
    if (left <= 0) { break; }
    var t = st.tracks[k], take = [];
    while (t.q.length > 0 && left > 0) {
      var s = t.q.shift();
      take.push(s);
      left -= s.length;
    }
    if (take.length > 0) { out[k] = take; }
  }
  return JSON.stringify(out);
}`
