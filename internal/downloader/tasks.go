package downloader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

// Manifest 续传凭证：分片 URL 快照。恢复时重新解析清单，
// 分片数 + 首尾 URL 一致才复用已下分片，否则清空重下
// （KEY 每次重新解析时现取现用，不持久化，避免过期）。
type Manifest struct {
	MediaURL string   `json:"mediaUrl"`
	SegURLs  []string `json:"segUrls"`
	Total    int      `json:"total"`
}

// DownloadTask 带断点续传的下载：分片持久化在 stateDir/segs，
// 中断后再次调用即续传。直链走普通单流下载（暂不支持 Range 续传）。
func DownloadTask(ctx context.Context, opt Options, stateDir string, on Progress) (string, error) {
	normalizeOpt(&opt)
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return "", err
	}
	if !isM3U8(opt.MediaURL) && !isMPD(opt.MediaURL) {
		return downloadFile(ctx, opt, on)
	}
	if isMPD(opt.MediaURL) {
		segsDir := filepath.Join(stateDir, "dash")
		if err := os.MkdirAll(segsDir, 0o755); err != nil {
			return "", err
		}
		return downloadDASH(ctx, opt, segsDir, on)
	}
	segsDir := filepath.Join(stateDir, "segs")
	if err := os.MkdirAll(segsDir, 0o755); err != nil {
		return "", err
	}
	client := newClient()
	_, segs, err := resolveSegments(ctx, client, opt.MediaURL, opt.headers())
	if err != nil {
		return "", err
	}
	if !manifestMatch(stateDir, opt.MediaURL, segs) {
		_ = os.RemoveAll(segsDir)
		_ = os.MkdirAll(segsDir, 0o755)
		_ = saveManifest(stateDir, opt.MediaURL, segs)
	}
	paths, err := fetchSegments(ctx, client, opt, segs, segsDir, on)
	if err != nil {
		return "", err
	}
	out := outputPath(opt)
	if err := mergeSegments(ctx, paths, segsDir, out); err != nil {
		return "", err
	}
	if on != nil {
		on(len(segs), len(segs))
	}
	return out, nil
}

func manifestPath(stateDir string) string { return filepath.Join(stateDir, "manifest.json") }

func saveManifest(stateDir, mediaURL string, segs []segment) error {
	urls := make([]string, len(segs))
	for i, s := range segs {
		urls[i] = s.URL
	}
	data, _ := json.Marshal(Manifest{MediaURL: mediaURL, SegURLs: urls, Total: len(urls)})
	return os.WriteFile(manifestPath(stateDir), data, 0o644)
}

func manifestMatch(stateDir, mediaURL string, segs []segment) bool {
	data, err := os.ReadFile(manifestPath(stateDir))
	if err != nil {
		return false
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	if m.MediaURL != mediaURL || m.Total != len(segs) || len(m.SegURLs) != len(segs) {
		return false
	}
	if len(segs) == 0 {
		return false
	}
	return m.SegURLs[0] == segs[0].URL && m.SegURLs[len(segs)-1] == segs[len(segs)-1].URL
}
