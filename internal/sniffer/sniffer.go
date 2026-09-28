package sniffer

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Candidate 嗅探到的可下载资源：只做明文直链/HLS/DASH，DRM 不碰。
// Cookie 为重嗅探从无头浏览器读取的登录态（仅用于回源下载，不展示）。
type Candidate struct {
	URL       string `json:"url"`
	Kind      string `json:"kind"` // mp4 | m3u8-master | m3u8-media | mpd | page-video
	Label     string `json:"label"`
	Referer   string `json:"referer"`
	UserAgent string `json:"userAgent"`
	Cookie    string `json:"cookie"`
}

const defaultUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

var (
	m3u8Re = regexp.MustCompile(`https?://[^\s"'<>\\]+\.m3u8[^\s"'<>]*`)
	mpdRe  = regexp.MustCompile(`https?://[^\s"'<>\\]+\.mpd[^\s"'<>]*`)
	mp4Re  = regexp.MustCompile(`https?://[^\s"'<>\\]+\.(mp4|webm|flv|mov|m4s)[^\s"'<>]*`)
	videoTagRe = regexp.MustCompile(`(?i)<video[^>]+src=["']([^"']+)["']|<source[^>]+src=["']([^"']+)["']`)
)

// Probe 输入页面URL/视频URL -> 返回候选直链。轻嗅探：直链判断 + HTML 抓取。
// 重嗅探（rod 无头浏览器拦截 XHR/fetch，后续迭代）预留接口，直接复用 Candidate 结构。
func Probe(pageURL string) ([]Candidate, error) {
	u, err := url.Parse(pageURL)
	if err != nil || u.Scheme == "" {
		return nil, fmt.Errorf("URL 不合法: %s", pageURL)
	}
	lower := strings.ToLower(u.Path)
	switch {
	case strings.HasSuffix(lower, ".m3u8"):
		return []Candidate{{URL: pageURL, Kind: "m3u8-media", Label: "HLS 直链", UserAgent: defaultUA}}, nil
	case strings.HasSuffix(lower, ".mpd"):
		return []Candidate{{URL: pageURL, Kind: "mpd", Label: "DASH 直链", UserAgent: defaultUA}}, nil
	case strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".webm") || strings.HasSuffix(lower, ".mov") || strings.HasSuffix(lower, ".flv"):
		return []Candidate{{URL: pageURL, Kind: "mp4", Label: "文件直链", UserAgent: defaultUA}}, nil
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest("GET", pageURL, nil)
	req.Header.Set("User-Agent", defaultUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("抓取页面失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	html := string(body)
	seen := map[string]bool{}
	var out []Candidate
	add := func(raw, kind, label string) {
		abs := absolutize(u, strings.TrimSpace(raw))
		if abs == "" || seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, Candidate{URL: abs, Kind: kind, Label: label, Referer: pageURL, UserAgent: defaultUA})
	}
	for _, m := range m3u8Re.FindAllString(html, 10) {
		add(m, "m3u8-media", "页面内嵌 HLS")
	}
	for _, m := range mpdRe.FindAllString(html, 5) {
		add(m, "mpd", "页面内嵌 DASH")
	}
	for _, m := range videoTagRe.FindAllStringSubmatch(html, 10) {
		src := m[1]
		if src == "" {
			src = m[2]
		}
		if src == "" {
			continue
		}
		kind := "page-video"
		if strings.Contains(src, ".m3u8") {
			kind = "m3u8-media"
		}
		add(src, kind, "video 标签")
	}
	if len(out) == 0 {
		for _, m := range mp4Re.FindAllString(html, 10) {
			add(m, "mp4", "页面内嵌直链")
		}
	}
	return out, nil
}

func absolutize(base *url.URL, raw string) string {
	raw = strings.Trim(raw, `"'`)
	if raw == "" || strings.HasPrefix(raw, "blob:") || strings.HasPrefix(raw, "data:") {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}
