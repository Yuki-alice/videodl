# videodl

对齐夸克浏览器下载体验的桌面嗅探下载器（学习用，仅支持公开非 DRM 资源）。

Go + Wails + Svelte：轻嗅探 / 重嗅探（rod 无头浏览器，拦截 XHR/fetch + blob 回源）/
浏览嗅探（受控浏览器边逛边发现）/ HLS(m3u8) / DASH(mpd) / 直链下载 /
AES-128 解密 / ffmpeg 合并混流 / 断点续传 / Cookie 登录态透传 / 清晰度选择。

## Live Development

```bash
wails dev
```

## Building

```bash
wails build
```

产物：`build/bin/videodl.app`

## Testing

```bash
go test ./internal/...                 # 本地桩，秒级
VIDEODL_E2E=1 go test ./internal/downloader/ -run 'TestE2E(Public|Dash)' -v
```
