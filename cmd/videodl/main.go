// Command videodl 是 videodl 核心的 CLI 前端。
//
// 存在意义：internal/ 下的嗅探与下载能力完全不依赖 Wails / 前端构建，
// 命令行壳让核心可以脱离 GUI 独立构建与脚本化调用，同时作为浏览器扩展的
// 本地后端（-json 输出便于扩展解析）。
//
// 用法示例：
//
//	videodl -probe https://example.com/page           轻嗅探，只列候选
//	videodl -probe -deep https://example.com/page     重嗅探（无头浏览器）
//	videodl -variants https://cdn/a/master.m3u8       列清晰度档位
//	videodl https://cdn/a/master.m3u8                 直接下载
//	videodl -pick 2 https://example.com/page          下载第 2 个候选
//	videodl -pick all https://example.com/page        批量下载全部候选
//	videodl -json -probe https://example.com/page     机器可读输出
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"videodl/internal/downloader"
	"videodl/internal/sniffer"
)

const version = "0.1.0"

// defaultUA 用 Windows Chrome UA：CLI 主要跑在 Windows 上，
// 沿用原 GUI 里硬编码的 macOS UA 反而更容易被站点识破。
const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

type options struct {
	probe    bool
	deep     bool
	noDeep   bool
	deepWait int
	variants bool
	mse      bool
	mseDur   int
	pick     string
	outDir   string
	fileName string
	workers  int
	speedKB  int64
	referer  string
	cookie   string
	ua       string
	asJSON   bool
	quiet    bool
	showVer  bool
}

func main() {
	enableUTF8Console()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	var o options
	fs := flag.NewFlagSet("videodl", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }

	fs.BoolVar(&o.probe, "probe", false, "只嗅探并列出候选，不下载")
	fs.BoolVar(&o.deep, "deep", false, "嗅探时使用重嗅探（无头浏览器，较慢但覆盖率高）")
	fs.BoolVar(&o.noDeep, "no-deep", false, "轻嗅探无结果时不要自动回退到重嗅探")
	fs.IntVar(&o.deepWait, "deep-wait", 12, "重嗅探渲染等待秒数")
	fs.BoolVar(&o.variants, "variants", false, "只列出清晰度档位，不下载")
	fs.BoolVar(&o.mse, "mse", false, "对页面做 MSE 劫持：把播放器实际收到的媒体数据抓下来（blob 回源）")
	fs.IntVar(&o.mseDur, "mse-dur", 20, "MSE 采集窗口秒数")
	fs.StringVar(&o.pick, "pick", "", "选第 N 个候选（1 起算）或 all 表示全部")
	fs.StringVar(&o.outDir, "o", "", "输出目录，默认 ~/Downloads/videodl")
	fs.StringVar(&o.fileName, "F", "", "输出文件名（不含目录）")
	fs.IntVar(&o.workers, "j", 16, "分片并发数")
	fs.Int64Var(&o.speedKB, "speed", 0, "全局限速 KB/s，0 表示不限速")
	fs.StringVar(&o.referer, "referer", "", "回源 Referer（过防盗链）")
	fs.StringVar(&o.cookie, "cookie", "", "回源 Cookie（登录态，过鉴权站）")
	fs.StringVar(&o.ua, "ua", defaultUA, "User-Agent")
	fs.BoolVar(&o.asJSON, "json", false, "以 JSON 输出（stdout 只放结果，进度走 stderr）")
	fs.BoolVar(&o.quiet, "quiet", false, "不打印进度")
	fs.BoolVar(&o.showVer, "version", false, "打印版本号")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if o.showVer {
		fmt.Printf("videodl %s\n", version)
		return nil
	}

	rawURL := strings.TrimSpace(fs.Arg(0))
	if rawURL == "" {
		fmt.Fprint(os.Stderr, usageText)
		return fmt.Errorf("缺少 URL 参数")
	}
	if err := validateURL(rawURL); err != nil {
		return err
	}
	if o.workers < 1 {
		o.workers = 1
	}
	if o.deepWait <= 0 {
		o.deepWait = 12
	}
	if o.outDir == "" {
		o.outDir = defaultOutDir()
	}
	if o.speedKB < 0 {
		o.speedKB = 0
	}
	downloader.SetSpeedLimit(o.speedKB << 10)

	// Ctrl+C 取消：分片保留在任务目录，重跑同一条命令即续传。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch {
	case o.variants:
		return runVariants(ctx, o, rawURL)
	case o.mse:
		return runMSE(ctx, o, rawURL)
	case o.probe:
		_, err := sniffAndReport(ctx, o, rawURL)
		return err
	case isMediaURL(rawURL):
		return runDownload(ctx, o, rawURL, rawURL, "")
	default:
		cands, err := sniffAndReport(ctx, o, rawURL)
		if err != nil {
			return err
		}
		return downloadFromCandidates(ctx, o, rawURL, cands)
	}
}

// validateURL 提前拦掉明显不合法的输入，避免把 net/http 的底层报错直接甩给用户。
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL 解析失败: %s", raw)
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("URL 缺少主机名: %s", raw)
		}
		return nil
	case "":
		return fmt.Errorf("URL 缺少协议前缀（需要 http:// 或 https://）: %s", raw)
	default:
		return fmt.Errorf("不支持的协议 %q，只支持 http/https: %s", u.Scheme, raw)
	}
}

// isMediaURL 判断入参本身是否就是媒体直链（不用嗅探直接下）。
func isMediaURL(u string) bool {
	l := strings.ToLower(strings.Split(u, "?")[0])
	for _, ext := range []string{".m3u8", ".mpd", ".mp4", ".webm", ".flv", ".mov", ".m4s", ".ts"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	return false
}

// sniff 按 flag 选择轻嗅探 / 重嗅探，轻嗅探空结果时按需自动回退。
func sniff(ctx context.Context, o options, pageURL string) ([]sniffer.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !o.deep {
		cands, err := sniffer.Probe(pageURL)
		if err != nil {
			return nil, err
		}
		if len(cands) > 0 || o.noDeep {
			return cands, nil
		}
		if !o.quiet {
			fmt.Fprintln(os.Stderr, "轻嗅探没结果，自动改用重嗅探（无头浏览器）…")
		}
	}
	if !o.quiet {
		fmt.Fprintf(os.Stderr, "重嗅探中（渲染 %d 秒）…\n", o.deepWait)
	}
	return sniffer.DeepProbe(pageURL, o.deepWait)
}

// sniffAndReport 嗅探并输出候选列表（-json 走 stdout，人类可读走 stderr）。
func sniffAndReport(ctx context.Context, o options, pageURL string) ([]sniffer.Candidate, error) {
	cands, err := sniff(ctx, o, pageURL)
	if err != nil {
		return nil, err
	}
	printed := filtered(cands)
	if o.asJSON {
		return cands, emitJSON(jsonOut{
			OK: true, Command: "probe", URL: pageURL,
			Count: len(printed), Candidates: printed,
		})
	}
	if len(printed) == 0 {
		fmt.Fprintln(os.Stderr, "没有发现可下载的候选。可能用了 DRM 或私有协议。")
		return cands, nil
	}
	fmt.Fprintf(os.Stderr, "发现 %d 个候选：\n", len(printed))
	for i, c := range printed {
		fmt.Fprintf(os.Stderr, "  [%d] %-12s %s\n", i+1, c.Kind, c.URL)
	}
	return cands, nil
}

// downloadFromCandidates 智能路径：唯一候选直接下，多候选按 -pick 选择。
func downloadFromCandidates(ctx context.Context, o options, pageURL string, cands []sniffer.Candidate) error {
	list := filtered(cands)
	switch {
	case len(list) == 0:
		return fmt.Errorf("没有可下载的候选，请用 -probe 查看详情")
	case o.pick == "" && len(list) == 1:
		c := list[0]
		return runDownload(ctx, o, pageURL, c.URL, c.Referer)
	case o.pick == "":
		fmt.Fprintf(os.Stderr, "有 %d 个候选，请用 -pick N 选择，或 -pick all 全部下载：\n", len(list))
		for i, c := range list {
			fmt.Fprintf(os.Stderr, "  [%d] %-12s %s\n", i+1, c.Kind, c.URL)
		}
		return nil
	case strings.EqualFold(o.pick, "all"):
		var failed int
		for i, c := range list {
			fmt.Fprintf(os.Stderr, "(%d/%d) %s\n", i+1, len(list), c.URL)
			if err := runDownload(ctx, o, pageURL, c.URL, c.Referer); err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "  失败: %v\n", err)
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d/%d 个任务失败", failed, len(list))
		}
		return nil
	default:
		n, err := strconv.Atoi(o.pick)
		if err != nil || n < 1 || n > len(list) {
			return fmt.Errorf("-pick 需要是 1 到 %d 之间的序号，或 all", len(list))
		}
		c := list[n-1]
		return runDownload(ctx, o, pageURL, c.URL, c.Referer)
	}
}

// runDownload 执行一次下载。inputURL 用于日志与文件名兜底，mediaURL 是真正回源地址。
func runDownload(ctx context.Context, o options, inputURL, mediaURL, referer string) error {
	if referer == "" {
		referer = o.referer
	}
	if referer == "" && !isMediaURL(inputURL) {
		referer = inputURL // 页面内嵌资源一般校验 Referer
	}
	opt := downloader.Options{
		MediaURL:  mediaURL,
		Referer:   referer,
		UserAgent: o.ua,
		Cookie:    o.cookie,
		OutDir:    o.outDir,
		FileName:  o.fileName,
		Workers:   o.workers,
	}
	unit := "分片"
	if !isHLSOrDASH(mediaURL) {
		unit = "字节"
	}
	stateDir := cliStateDir(mediaURL)
	out, err := downloader.DownloadTask(ctx, opt, stateDir, newProgress(unit, o.quiet))
	if !o.quiet {
		clearLine()
	}
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "已中断，进度已保存；重跑同一条命令即可续传。\n")
			return nil
		}
		return err
	}
	if o.asJSON {
		return emitJSON(jsonOut{OK: true, Command: "download", URL: mediaURL, Output: out})
	}
	fmt.Fprintf(os.Stderr, "完成: %s\n", out)
	return nil
}

// runMSE 做 MSE 劫持：把播放器实际经由 appendBuffer 收到的数据抓回本地。
// 用于「能正常播放但嗅不到任何 URL」的站点——那是 blob 回源的唯一出路。
func runMSE(ctx context.Context, o options, pageURL string) error {
	if !o.quiet {
		fmt.Fprintf(os.Stderr, "MSE 劫持中（渲染 %d 秒 + 采集 %d 秒）…\n", o.deepWait, o.mseDur)
	}
	tracks, err := sniffer.CaptureMSE(ctx, pageURL, sniffer.MSECaptureOptions{
		OutDir:   o.outDir,
		WaitSec:  o.deepWait,
		Duration: time.Duration(o.mseDur) * time.Second,
	})
	if err != nil {
		return err
	}
	if o.asJSON {
		return emitJSON(jsonOut{OK: true, Command: "mse", URL: pageURL, Count: len(tracks), Tracks: tracks})
	}
	if len(tracks) == 0 {
		fmt.Fprintln(os.Stderr, "没抓到 MSE 数据。可能该站不用 MSE，或需要更长采集窗口（-mse-dur）。")
		return nil
	}
	fmt.Fprintf(os.Stderr, "抓到 %d 条媒体轨：\n", len(tracks))
	for _, tr := range tracks {
		fmt.Fprintf(os.Stderr, "  %-46s %8.2fMB  %4d 块  %s\n",
			tr.MimeType, float64(tr.Bytes)/(1<<20), tr.Chunks, tr.Path)
	}
	fmt.Fprintln(os.Stderr, "提示：同一视频的 video/audio 是分开的两条轨，需要合并时用 ffmpeg -i 视频 -i 音频 -c copy out.mp4")
	return nil
}

// runVariants 列出清晰度档位（HLS master / DASH MPD / 直链单档）。
func runVariants(ctx context.Context, o options, mediaURL string) error {
	h := downloader.Headers{Referer: o.referer, UserAgent: o.ua, Cookie: o.cookie}
	var (
		list []downloader.Variant
		err  error
	)
	if strings.Contains(strings.ToLower(mediaURL), ".mpd") {
		list, err = downloader.ParseDashVariants(ctx, mediaURL, h)
	} else {
		list, err = downloader.ParseVariants(ctx, mediaURL, o.referer, o.cookie, o.ua)
	}
	if err != nil {
		return err
	}
	if o.asJSON {
		return emitJSON(jsonOut{OK: true, Command: "variants", URL: mediaURL, Variants: list})
	}
	fmt.Fprintf(os.Stderr, "%d 档清晰度（已按码率降序）：\n", len(list))
	for i, v := range list {
		fmt.Fprintf(os.Stderr, "  [%d] %s\n", i+1, v.Label)
	}
	return nil
}

// filtered 去掉无法回源的 blob 项，保持原始嗅探顺序。
func filtered(cands []sniffer.Candidate) []sniffer.Candidate {
	out := make([]sniffer.Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Kind == "blob" || c.URL == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

func isHLSOrDASH(u string) bool {
	l := strings.ToLower(u)
	return strings.Contains(l, ".m3u8") || strings.Contains(l, ".mpd")
}

// cliStateDir 用 mediaURL 的哈希做续传目录，保证同一地址重跑能命中同一份分片。
func cliStateDir(mediaURL string) string {
	sum := sha256.Sum256([]byte(mediaURL))
	id := "cli-" + hex.EncodeToString(sum[:6])
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "videodl", id)
	}
	return filepath.Join(home, ".videodl", "tasks", id)
}

func defaultOutDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(home, "Downloads", "videodl")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// ---------- 进度渲染 ----------

// newProgress 返回一个限流到 ~10Hz 的进度回调，渲染到 stderr 以免污染 -json 的 stdout。
func newProgress(unit string, quiet bool) downloader.Progress {
	start := time.Now()
	var lastMS atomic.Int64
	return func(done, total int) {
		if quiet {
			return
		}
		now := time.Now()
		if now.UnixMilli()-lastMS.Load() < 100 {
			return
		}
		lastMS.Store(now.UnixMilli())
		elapsed := now.Sub(start).Seconds()
		speed := ""
		if elapsed > 0.5 {
			speed = fmt.Sprintf(" %s%s/s", human(float64(done)/elapsed, unit), unit)
		}
		if total > 0 {
			pct := float64(done) / float64(total) * 100
			if pct > 100 {
				pct = 100
			}
			fmt.Fprintf(os.Stderr, "\r%s %5.1f%%  %s/%s%s",
				bar(pct, 24), pct, human(float64(done), unit), human(float64(total), unit), speed)
			return
		}
		fmt.Fprintf(os.Stderr, "\r%s%s", human(float64(done), unit), speed)
	}
}

func bar(pct float64, width int) string {
	filled := int(float64(width) * pct / 100)
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
}

// human 按单位格式化：分片计数直接取整，字节走 1024 进制。
func human(v float64, unit string) string {
	if unit == "分片" {
		return strconv.FormatInt(int64(v), 10)
	}
	const step = 1024
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for v >= step && i < len(units)-1 {
		v /= step
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f%s", v, units[i])
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

func clearLine() { fmt.Fprint(os.Stderr, "\r\033[K") }

// ---------- JSON 输出 ----------

// jsonOut 是给浏览器扩展 / 脚本消费的统一信封。
type jsonOut struct {
	OK         bool                 `json:"ok"`
	Command    string               `json:"command"`
	URL        string               `json:"url,omitempty"`
	Count      int                  `json:"count,omitempty"`
	Candidates []sniffer.Candidate  `json:"candidates,omitempty"`
	Variants   []downloader.Variant `json:"variants,omitempty"`
	Tracks     []sniffer.MSETrack   `json:"tracks,omitempty"`
	Output     string               `json:"output,omitempty"`
	Error      string               `json:"error,omitempty"`
}

func emitJSON(v jsonOut) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

const usageText = `videodl ` + version + ` — 视频嗅探下载器（CLI）

用法:
  videodl [flags] <url>

模式（默认：媒体直链直接下；页面 URL 先嗅探再下）:
  -probe              只嗅探并列出候选
  -variants           只列出清晰度档位
  -deep               嗅探时使用重嗅探（无头浏览器）
  -mse                对页面做 MSE 劫持，把播放器实际收到的数据抓下来（blob 回源）
  -no-deep            轻嗅探无结果时不自动回退重嗅探
  -pick N|all         选第 N 个候选下载，或全部

参数:
  -o <dir>            输出目录（默认 ~/Downloads/videodl）
  -F <name>           输出文件名
  -j <n>              分片并发数（默认 16）
  -speed <KB/s>       全局限速，0 不限（默认 0）
  -referer <s>        回源 Referer
  -cookie <s>         回源 Cookie
  -ua <s>             User-Agent
  -json               以 JSON 输出到 stdout（进度走 stderr）
  -quiet              不打印进度
  -deep-wait <sec>    渲染等待秒数，重嗅探与 MSE 采集共用（默认 12）
  -mse-dur <sec>      MSE 采集窗口秒数（默认 20）
  -version            打印版本号

示例:
  videodl -probe https://example.com/page
  videodl -probe -deep -json https://example.com/page
  videodl -variants https://cdn.example.com/master.m3u8
  videodl -mse -mse-dur 30 -o E:/videos https://example.com/page
  videodl -pick 1 -o E:/videos https://example.com/page
  videodl "https://cdn.example.com/1/master.m3u8"
`
