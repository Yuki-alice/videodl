package downloader

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Variant master 清单里的一档清晰度，供 GUI 手动选择。
type Variant struct {
	URL        string `json:"url"`
	Bandwidth  int    `json:"bandwidth"`
	Resolution string `json:"resolution"`
	Codecs     string `json:"codecs"`
	Label      string `json:"label"`
}

// ParseVariants 拉取 m3u8 并解析清晰度档位。
// 非 master 清单返回单档“默认”，URL 即原地址，前端可直接下载。
func ParseVariants(ctx context.Context, mediaURL, referer, cookie, userAgent string) ([]Variant, error) {
	if userAgent == "" {
		userAgent = "Mozilla/5.0"
	}
	data, err := doGet(ctx, newClient(), mediaURL, Headers{Referer: referer, UserAgent: userAgent, Cookie: cookie})
	if err != nil {
		return nil, err
	}
	text := string(data)
	if !strings.Contains(text, "#EXT-X-STREAM-INF") {
		return []Variant{{URL: mediaURL, Label: "默认清晰度"}}, nil
	}
	var out []Variant
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			continue
		}
		v := Variant{Bandwidth: -1}
		attrs := splitAttrs(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		for k, val := range attrs {
			switch k {
			case "BANDWIDTH":
				if n, e := strconv.Atoi(val); e == nil {
					v.Bandwidth = n
				}
			case "RESOLUTION":
				v.Resolution = val
			case "CODECS":
				v.Codecs = strings.Trim(val, `"`)
			}
		}
		// 下一行非 # 开头的即分档 URI。
		uri := ""
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l == "" {
				continue
			}
			if !strings.HasPrefix(l, "#") {
				uri = l
			}
			break
		}
		if uri == "" {
			continue
		}
		v.URL = resolve(mediaURL, uri)
		v.Label = variantLabel(v, len(out)+1)
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("master 清单解析失败，未找到分档")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bandwidth > out[j].Bandwidth })
	return out, nil
}

func splitAttrs(s string) map[string]string {
	m := map[string]string{}
	var cur strings.Builder
	inQuote := false
	parts := []string{}
	for _, r := range s {
		switch r {
		case '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case ',':
			if inQuote {
				cur.WriteRune(r)
			} else {
				parts = append(parts, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	parts = append(parts, cur.String())
	for _, p := range parts {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) == 2 {
			m[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return m
}

func variantLabel(v Variant, idx int) string {
	res := v.Resolution
	if res == "" {
		res = fmt.Sprintf("档位%d", idx)
	}
	if v.Bandwidth > 0 {
		return fmt.Sprintf("%s · %.1fMbps", res, float64(v.Bandwidth)/1e6)
	}
	return res
}
