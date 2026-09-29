package downloader

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Progress 回调：done/total 分片数。
type Progress func(done, total int)

// Options 下载参数，Header 透传嗅探阶段的 Referer/UA/Cookie 以过防盗链。
type Options struct {
	MediaURL  string
	Referer   string
	UserAgent string
	Cookie    string
	OutDir    string
	FileName  string
	Workers   int
}

// Headers 回源请求头：嗅探到的登录态需要原样带回，否则鉴权站回源 403。
type Headers struct {
	Referer   string
	UserAgent string
	Cookie    string
}

func (h Headers) apply(req *http.Request) {
	if h.UserAgent != "" {
		req.Header.Set("User-Agent", h.UserAgent)
	}
	if h.Referer != "" {
		req.Header.Set("Referer", h.Referer)
	}
	if h.Cookie != "" {
		req.Header.Set("Cookie", h.Cookie)
	}
}

func (opt Options) headers() Headers {
	ua := opt.UserAgent
	if ua == "" {
		ua = "Mozilla/5.0"
	}
	return Headers{Referer: opt.Referer, UserAgent: ua, Cookie: opt.Cookie}
}

// Download 输入视频URL -> 下m3u8文本 -> 解析ts -> 并发下 -> AES-128解密 -> ffmpeg合并。
// 只处理明文 HLS + 文件直链；DRM/私有加密直接返回错误。
// 一次性任务（临时目录，用完即删）；需断点续传走 DownloadTask。
func Download(ctx context.Context, opt Options, on Progress) (string, error) {
	normalizeOpt(&opt)
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return "", err
	}
	if isM3U8(opt.MediaURL) {
		return downloadHLS(ctx, opt, on)
	}
	if isMPD(opt.MediaURL) {
		tmpDir, err := os.MkdirTemp("", "videodl-dash-*")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmpDir)
		return downloadDASH(ctx, opt, tmpDir, on)
	}
	return downloadFile(ctx, opt, on)
}

func isMPD(u string) bool { return strings.Contains(strings.ToLower(u), ".mpd") }

func normalizeOpt(opt *Options) {
	if opt.Workers <= 0 {
		opt.Workers = 16
	}
	if opt.UserAgent == "" {
		opt.UserAgent = "Mozilla/5.0"
	}
}

func isM3U8(u string) bool { return strings.Contains(strings.ToLower(u), ".m3u8") }

func newClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

func doGet(ctx context.Context, client *http.Client, rawURL string, h Headers) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	h.apply(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, rawURL)
	}
	return io.ReadAll(io.LimitReader(pacedReader{resp.Body}, 200<<20))
}

type segment struct {
	URL string
	Key []byte
	IV  []byte
}

func resolve(base, ref string) string {
	b, err1 := url.Parse(base)
	r, err2 := url.Parse(strings.TrimSpace(ref))
	if err1 != nil || err2 != nil {
		return strings.TrimSpace(ref)
	}
	return b.ResolveReference(r).String()
}

func segPath(dir string, idx int) string {
	return filepath.Join(dir, fmt.Sprintf("seg-%06d.ts", idx))
}

func downloadHLS(ctx context.Context, opt Options, on Progress) (string, error) {
	client := newClient()
	_, segs, err := resolveSegments(ctx, client, opt.MediaURL, opt.headers())
	if err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp("", "videodl-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	paths, err := fetchSegments(ctx, client, opt, segs, tmpDir, on)
	if err != nil {
		return "", err
	}
	out := outputPath(opt)
	if err := mergeSegments(ctx, paths, tmpDir, out); err != nil {
		return "", err
	}
	if on != nil {
		on(len(segs), len(segs))
	}
	return out, nil
}

// fetchSegments 并发下载分片并做 AES-128 解密，已存在且非空的分片文件直接跳过（断点续传）。
func fetchSegments(ctx context.Context, client *http.Client, opt Options, segs []segment, dir string, on Progress) ([]string, error) {
	// 预扫描：已完成的分片不再下。
	have := make([]bool, len(segs))
	done := 0
	for i := range segs {
		if st, err := os.Stat(segPath(dir, i)); err == nil && st.Size() > 0 {
			have[i] = true
			done++
		}
	}
	if on != nil {
		on(done, len(segs))
	}

	type job struct {
		idx int
		seg segment
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	paths := make([]string, len(segs))
	for i := range segs {
		if have[i] {
			paths[i] = segPath(dir, i)
		}
	}
	for w := 0; w < opt.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				// 分片抖动是常态：3 次指数退避重试，扛住后才判任务失败。
				raw, err := fetchWithRetry(ctx, client, j.seg.URL, opt.headers(), 3)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				if len(j.seg.Key) > 0 {
					iv := j.seg.IV
					if iv == nil {
						iv = make([]byte, 16)
						n := j.idx
						for i := 15; i >= 8 && n > 0; i-- {
							iv[i] = byte(n)
							n >>= 8
						}
					}
					raw, err = aesDecryptCBC(raw, j.seg.Key, iv)
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("分片%d解密失败: %w", j.idx, err)
						}
						mu.Unlock()
						continue
					}
				}
				if err := os.WriteFile(segPath(dir, j.idx), raw, 0o644); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				paths[j.idx] = segPath(dir, j.idx)
				done++
				d, t := done, len(segs)
				mu.Unlock()
				if on != nil {
					on(d, t)
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i, s := range segs {
			if have[i] {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case jobs <- job{idx: i, seg: s}:
			}
		}
	}()
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err() // 暂停/取消优先返回，保留已下分片供续传
	}
	if firstErr != nil {
		return nil, firstErr
	}
	for _, p := range paths {
		if p == "" {
			return nil, fmt.Errorf("有分片下载失败，任务中止")
		}
	}
	return paths, nil
}

func outputPath(opt Options) string {
	name := opt.FileName
	if name == "" {
		host := "video"
		if u, err := url.Parse(opt.MediaURL); err == nil && u.Hostname() != "" {
			host = sanitizeFileName(u.Hostname())
		}
		name = fmt.Sprintf("%s-%d.mp4", host, time.Now().Unix())
	}
	if !strings.HasSuffix(strings.ToLower(name), ".mp4") {
		name += ".mp4"
	}
	return filepath.Join(opt.OutDir, name)
}

func sanitizeFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, s)
	s = strings.Trim(s, "-.")
	if len(s) > 40 {
		s = s[len(s)-40:]
	}
	if s == "" {
		s = "video"
	}
	return s
}

// fetchWithRetry 分片拉取 + 指数退避重试，ctx 取消优先返回。
func fetchWithRetry(ctx context.Context, client *http.Client, rawURL string, h Headers, tries int) ([]byte, error) {
	var err error
	var raw []byte
	for i := 0; i < tries; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		raw, err = doGet(ctx, client, rawURL, h)
		if err == nil {
			return raw, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<i) * time.Second):
		}
	}
	return nil, err
}

func mergeSegments(ctx context.Context, paths []string, dir string, outPath string) error {
	ffmpeg, err := findFFmpeg()
	if err != nil {
		return err
	}
	listFile := filepath.Join(dir, "filelist.txt")
	var sb strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&sb, "file '%s'\n", strings.ReplaceAll(p, "'", "'\\''"))
	}
	if err := os.WriteFile(listFile, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, ffmpeg, "-f", "concat", "-safe", "0", "-i", listFile, "-c", "copy", "-y", outPath)
	if eb, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg 合并失败: %v\n%s", err, string(eb))
	}
	return nil
}

// downloadFile 直链下载：体积够大且服务端支持 Range 时走多线程分块（.mrp + .mrp.json 续传），
// 否则回落单连接顺序下载（.part 断点续传，完工改名）。
func downloadFile(ctx context.Context, opt Options, on Progress) (string, error) {
	h := opt.headers()
	name := opt.FileName
	if name == "" {
		name = filepath.Base(strings.Split(opt.MediaURL, "?")[0])
		if name == "" || !strings.Contains(name, ".") {
			name = fmt.Sprintf("video-%d.mp4", time.Now().Unix())
		}
	}
	out := filepath.Join(opt.OutDir, name)
	client := newClient()

	// 一次 HEAD 探清体积与 Range 支持情况，两条路径共用，避免重复握手。
	total, acceptRanges, validator := probeRemote(ctx, client, opt.MediaURL, h)

	if acceptRanges && total >= multiRangeThreshold {
		err := downloadMultiRange(ctx, client, opt, h, total, validator, out, on)
		switch {
		case err == nil:
			_ = os.Remove(multiRangeStatePath(out))
			if rerr := os.Rename(multiRangePartPath(out), out); rerr != nil {
				return "", rerr
			}
			if on != nil {
				on(int(total), int(total))
			}
			return out, nil
		case errors.Is(err, errRangeUnsupported):
			// 服务端谎报支持 Range：清掉半成品，回退单连接重下。
			_ = os.Remove(multiRangePartPath(out))
			_ = os.Remove(multiRangeStatePath(out))
			total, acceptRanges = -1, false
		case ctx.Err() != nil:
			return "", ctx.Err() // 中断：保留 .mrp 与凭证供续传
		default:
			return "", err
		}
	}

	part := out + ".part"

	var offset int64
	if st, err := os.Stat(part); err == nil && st.Size() > 0 {
		if acceptRanges && (total < 0 || st.Size() < total) {
			offset = st.Size() // 续传
		} else if total >= 0 && st.Size() >= total {
			_ = os.Rename(part, out) // 上次已下完只差改名
			if on != nil {
				on(int(total), int(total))
			}
			return out, nil
		} else {
			_ = os.Remove(part) // 不支持 Range，重下
		}
	}

	req, _ := http.NewRequestWithContext(ctx, "GET", opt.MediaURL, nil)
	h.apply(req)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK && offset > 0 {
		// 服务器无视 Range，从头下。
		offset = 0
		_ = os.Remove(part)
	}
	if total < 0 && resp.ContentLength > 0 {
		total = resp.ContentLength + offset
	}
	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	body := io.Reader(resp.Body)
	if speedLimit.Load() > 0 {
		body = pacedReader{resp.Body}
	}
	buf := make([]byte, 256*1024)
	done := offset
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return "", werr
			}
			done += int64(n)
			if on != nil {
				on(int(done), int(total))
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err // 中断保留 .part，下次续传
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(part, out); err != nil {
		return "", err
	}
	return out, nil
}

func findFFmpeg() (string, error) {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	// GUI 双击启动时 PATH 里可能没有 brew，加上常见位置兜底。
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 ffmpeg，请先 brew install ffmpeg")
}

func aesDecryptCBC(data, key, iv []byte) ([]byte, error) {
	if len(key) != 16 && len(key) != 24 && len(key) != 32 {
		return nil, fmt.Errorf("非法 KEY 长度 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("密文长度不对齐")
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	// PKCS7 unpad
	pad := int(out[len(out)-1])
	if pad <= 0 || pad > block.BlockSize() || pad > len(out) {
		return out, nil // 有些站无 padding，直接返回
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return out, nil
		}
	}
	return bytes.Clone(out[:len(out)-pad]), nil
}
