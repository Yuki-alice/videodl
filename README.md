# videodl

对齐夸克浏览器下载体验的嗅探下载器（学习用，仅支持公开非 DRM 资源）。

## 架构：一核三壳

核心能力完全不依赖 GUI，可被多种前端复用。

```
        MV3 浏览器扩展          CLI / TUI            桌面 GUI（可选）
        原生嗅探·真登录态       脚本化·可管道         下载中心·边下边播
              │                     │                      │
              └──── localhost ──────┴──────────────────────┘
                            │
                    ┌───────▼────────┐
                    │   Go 核心       │  internal/
                    │ 零 GUI 依赖      │  HLS·DASH / AES-128 / 多线程 Range / ffmpeg 合流
                    └────────────────┘
```

- `internal/sniffer` —— 轻嗅探（HTTP + 正则）、重嗅探（rod 无头 Chromium）、
  浏览嗅探（受控有头浏览器边逛边发现）
- `internal/downloader` —— HLS(m3u8) / DASH(mpd) / 直链 / AES-128 解密 /
  ffmpeg 合并混流 / 分片级断点续传 / Cookie 登录态透传 / 全局限速

**当前实现程度**：

- ✅ 轻嗅探（HTTP + 正则）、重嗅探（rod 无头 Chromium + JS 钩子拦 fetch/XHR）
- ✅ **MSE 劫持 / blob 回源**：代理 `SourceBuffer.appendBuffer` 复制播放器实际收到的数据，
  按 `addSourceBuffer` 的 mimeType 分轨落盘（`-mse`）
- ✅ HLS(m3u8) / DASH(mpd) / 直链、AES-128 解密、ffmpeg 混流、断点续传
- ✅ **直链多线程 Range 分块**（≥2MB 且服务端支持 Range 时自动启用，带 ETag/Last-Modified 变更检测）
- ❌ CDP 网络域嗅探（尚未做，目前只有 JS 注入 + Resource Timing，Worker/iframe 内请求仍会漏）
- ❌ HLS `#EXT-X-MAP`（fMP4 init 段）、直播、多音轨/字幕轨

完整差距分析与复刻路线图见 [`docs/QUARK-PARITY.md`](docs/QUARK-PARITY.md)。

## CLI

命令行壳复用同一套核心，不依赖 Wails 与前端构建。

```bash
go build -o build/bin/videodl ./cmd/videodl
```

```bash
videodl -probe https://example.com/page            # 轻嗅探，只列候选
videodl -probe -deep https://example.com/page      # 重嗅探（无头浏览器）
videodl -variants https://cdn/a/master.m3u8        # 列清晰度档位
videodl https://cdn/a/master.m3u8                  # 直链直接下
videodl -mse https://example.com/page              # MSE 劫持（blob 回源）
videodl -pick 2 https://example.com/page           # 下第 2 个候选
videodl -pick all https://example.com/page         # 批量下全部候选
videodl -json -probe https://example.com/page      # 机器可读，供扩展消费
videodl -j 32 -speed 2048 -o E:/videos <url>       # 32 并发 / 限速 2MB/s
```

常用参数：`-o` 输出目录、`-F` 文件名、`-j` 分片并发数、`-speed` 限速(KB/s)、
`-referer` / `-cookie` / `-ua` 回源凭据、`-json` JSON 输出、`-quiet` 静默、
`-mse-dur` MSE 采集窗口秒数、`-deep-wait` 渲染等待秒数。

`Ctrl+C` 会保留已下分片；**重跑同一条命令即断点续传**（续传目录按媒体 URL 哈希固定）。

## GUI（Wails + Svelte）

```bash
wails dev      # 开发模式
wails build    # 打包，产物在 build/bin/
```

> 注意：根包 `main.go` 用 `//go:embed all:frontend/dist`，**必须先构建前端**
> 才能 `go build .`。只改后端时请构建 `./cmd/videodl` 或 `./internal/...`，不受此限。

## Testing

```bash
go test ./internal/...                 # 本地桩，秒级
go vet ./cmd/... ./internal/...
VIDEODL_E2E=1 go test ./internal/downloader/ -run 'TestE2E(Public|Dash)' -v
```

## 开发注意

- 仓库文件的换行符是 CRLF（`core.autocrlf=true` 检出所致）。**不要对全仓库跑
  `gofmt -w`**，那会把所有文件重写成 LF 并产生巨大的无关 diff。只格式化自己新写的文件。

- **rod 的 `page.Eval` 只接受「函数」，不接受表达式或 IIFE。**
  它内部会把入参包成 `function(){ return (js).apply(this, arguments) }`，
  传 `1+1` / `JSON.stringify(...)` / `(function(){...})()` 都会抛
  `TypeError: xxx.apply is not a function`；参数要单独传：
  `page.Eval("(x) => x*2", 21)`。
  这个坑曾导致 `collectJS`、`memScanJS`、存活探针 `Eval("1")` 全部静默失效
  （重嗅探、浏览嗅探实际不可用），且因为错误被 `_` 吞掉而毫无提示。
  回归守卫见 `internal/sniffer/regression_test.go`。
  例外：`EvalOnNewDocument` 直接注入源码，那里的钩子脚本**应该**是 IIFE。

- `gson.JSON.String()` 等价于 `Sprintf("%v", 已解析的值)`，对 JSON 字符串返回
  **去引号后的内容**。取值请用 `res.Value.Str()`，不要再 `json.Unmarshal` 一遍。

- `SourceBuffer.mimeType` 在部分 Chrome 构建下是 `undefined`（实测本机如此），
  且 `SourceBuffer.prototype` 上没有该属性。MSE 轨名必须从 `addSourceBuffer(mime)`
  的入参取，用 `WeakMap` 绑到实例上。
