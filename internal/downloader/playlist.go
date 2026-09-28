package downloader

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// resolveSegments 拉 m3u8 文本并解析为分片列表。
// master 清单自动选带宽最高的 variant（GUI 手动选清晰度走 ParseVariants）。
// 返回最终 media 清单 URL + 分片（含 AES KEY/IV，快照式，续传时重新解析刷新）。
func resolveSegments(ctx context.Context, client *http.Client, mediaURL string, h Headers) (string, []segment, error) {
	data, err := doGet(ctx, client, mediaURL, h)
	if err != nil {
		return "", nil, err
	}
	text := string(data)
	if strings.Contains(text, "#EXT-X-STREAM-INF") {
		best, bw := "", -1
		sc := bufio.NewScanner(strings.NewReader(text))
		nextIsURI := false
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
				nextIsURI = true
				if i := strings.Index(line, "BANDWIDTH="); i >= 0 {
					num := ""
					for _, c := range line[i+10:] {
						if c >= '0' && c <= '9' {
							num += string(c)
						} else {
							break
						}
					}
					if n, e := strconv.Atoi(num); e == nil && n > bw {
						bw = n
					}
				}
				continue
			}
			if nextIsURI && line != "" && !strings.HasPrefix(line, "#") {
				nextIsURI = false
				if bw >= 0 {
					best = line
				}
			}
		}
		if best == "" {
			return "", nil, fmt.Errorf("master 清单解析失败，未找到 variant")
		}
		return resolveSegments(ctx, client, resolve(mediaURL, best), h)
	}

	var segs []segment
	keyCache := map[string][]byte{}
	var curKey []byte
	var curIV []byte
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#EXT-X-KEY") {
			method, uri, ivHex := "", "", ""
			for _, part := range strings.Split(strings.TrimPrefix(line, "#EXT-X-KEY:"), ",") {
				kv := strings.SplitN(part, "=", 2)
				if len(kv) != 2 {
					continue
				}
				switch strings.TrimSpace(kv[0]) {
				case "METHOD":
					method = strings.TrimSpace(kv[1])
				case "URI":
					uri = strings.Trim(strings.TrimSpace(kv[1]), `"`)
				case "IV":
					ivHex = strings.TrimSpace(kv[1])
				}
			}
			if method != "" && method != "AES-128" && method != "NONE" {
				return "", nil, fmt.Errorf("不支持的加密方式 %s（DRM/私有加密不在支持范围）", method)
			}
			if method == "AES-128" {
				keyURL := resolve(mediaURL, uri)
				kb, ok := keyCache[keyURL]
				if !ok {
					kb, err = doGet(ctx, client, keyURL, h)
					if err != nil {
						return "", nil, fmt.Errorf("KEY 下载失败: %w", err)
					}
					keyCache[keyURL] = kb
				}
				curKey = kb
				if strings.HasPrefix(strings.ToLower(ivHex), "0x") {
					ivHex = ivHex[2:]
				}
				if ivHex != "" {
					iv := make([]byte, 16)
					for i := 0; i < 16 && i*2+2 <= len(ivHex); i++ {
						n, _ := strconv.ParseUint(ivHex[i*2:i*2+2], 16, 8)
						iv[i] = byte(n)
					}
					curIV = iv
				} else {
					curIV = nil // 缺省 IV = 分片序号，下载时填充
				}
			} else {
				curKey, curIV = nil, nil
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		segs = append(segs, segment{URL: resolve(mediaURL, line), Key: curKey, IV: curIV})
	}
	if len(segs) == 0 {
		return "", nil, fmt.Errorf("m3u8 里没有分片")
	}
	return mediaURL, segs, nil
}
