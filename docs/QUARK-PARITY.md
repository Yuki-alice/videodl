# 复刻夸克浏览器下载功能 —— 调研 · 差距分析 · 路线图

> 目标：把 videodl 的能力拉到"对齐夸克浏览器下载体验"。
> 结论先行：**夸克的能力约 70% 是客户端可复刻的，30% 依赖服务端（CDN/P2P/网盘离线），纯客户端项目做不到，不要在这上面浪费精力。**

---

## 一、夸克的"下载"到底是什么：五层拆解

夸克不是一个"下载器"，它是把下载能力**缝进浏览器产品流程**里。按层拆开：

| 层 | 夸克的做法 | 用户感知 |
|---|---|---|
| **1. 触发层** | 页面加载时后台持续扫描媒体请求；命中后地址栏右侧弹出「视频已找到」提示条 / 蓝色下载图标；部分版本支持长按页面空白 1.5s 唤起嗅探菜单 | 「它自己发现了」 |
| **2. 识别层** | 三层捕获：网络请求嗅探 → 播放器 API 劫持（MSE） → 录制兜底 | 覆盖率高、不用懂技术 |
| **3. 选择层** | 点提示条进入「资源分析」页，按类型（视频/音频/图片）分组，列出**多档清晰度 + 格式**（MP4/TS/m3u8），标注分辨率/大小 | 「能挑 1080P」 |
| **4. 下载层** | 极速下载模式 = 多线程 + HTTP/HTTPS 加速 + 断点续传优化；智能限速（省电/省流量时压制并发）；P2P 加速；任务被系统冻结时回落系统 DownloadManager | 「下得快、断了能续」 |
| **5. 管理层** | 下载中心：任务列表（文件名/进度/大小/状态）、暂停、恢复、单条删除、清空列表、改下载目录；后台下载；`quark://cache` 可直接看缓存文件 | 「像个正经下载器」 |

### 关键的产品细节（这些才是"体验"）

1. **要求视频真实播放 ≥3 秒才触发嗅探** —— 本质是"等媒体请求真的发出来"，不是玄学。
2. **提示条是非侵入式的**，不弹窗、不打断浏览。
3. **清晰度默认选中最高档**，不用用户决策。
4. **暂停/恢复是断点级的**，不是重新开始。
5. **下载全程在后台**，可以继续逛网页。

### 夸克里**无法**在客户端复刻的部分（必须明确划线）

| 能力 | 为什么复刻不了 |
|---|---|
| P2P 加速 | 依赖夸克自建 P2P 网络 + 其他客户端做种，客户端单机无法形成网络 |
| CDN 就近调度 / 多域名择优 | 依赖服务端 `X-RateLimit` / CDN 节点信息 |
| 云离线下载、跨端续传 | 夸克网盘的服务端能力 |
| 系统级后台保活 | 依赖 Android 系统 DownloadManager / 电池白名单 |

> **这四项要主动放弃。** 盯住"嗅探覆盖率 + 下载引擎 + 下载中心交互"，就已经能做出 90% 的体感对齐。

---

## 二、技术实现原理：三层捕获架构（可复刻的核心）

行业里把这件事做到最透的开源参照是 **猫抓（Cat-Catch）**，它的架构可以直接搬。核心思想是**分级设防：能匹配就匹配，匹配不到就劫持 API，劫持不到就录制。**

```
第 1 层：网络请求嗅探
        手段：webRequest / CDP Network 域，监听所有 XHR/Fetch/媒体请求
        产出：直链、m3u8、mpd
        ↓ 漏掉的（URL 里啥也看不出来）
第 2 层：播放器 API 劫持（MSE / blob 回源）
        手段：代理 MediaSource.prototype.addSourceBuffer、SourceBuffer.appendBuffer、
              URL.createObjectURL
        产出：直接复制喂给播放器的二进制流
        ↓ 漏掉的（拿不到数据）
第 3 层：MediaRecorder 录制
        手段：录屏式兜底
        产出：总能落盘，但牺牲画质/耗时
```

### 「blob 回源」的真相 —— 这是全篇最重要的一段

`blob:` 地址**不是一个网络地址**，它是 `URL.createObjectURL(obj)` 在浏览器内存里造的句柄。用 MSE 的播放器实际是：

```js
const ms = new MediaSource()
video.src = URL.createObjectURL(ms)        // ← 你嗅到的是这个 blob:，毫无用处
const sb = ms.addSourceBuffer('video/mp4') // ← 真正装数据的地方
sb.appendBuffer(segmentBytes)              // ← 数据是从这儿进去的
```

所以"回源"的正确解法**不是去找一个 URL**，而是：

**方案 A（推荐）：拦截 `appendBuffer`，把二进制复制一份送出来**

```js
// 注入脚本（在 document start 之前）
const _append = SourceBuffer.prototype.appendBuffer;
SourceBuffer.prototype.appendBuffer = function (buf) {
  buf.arrayBuffer().then(ab => {
    const u8 = new Uint8Array(ab);
    let s = '';
    for (let i = 0; i < u8.length; i += 0x8000)          // 分块避免爆栈
      s += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000));
    sendToGo(btoa(s));                                    // Go 侧绑定函数
  });
  return _append.apply(this, arguments);
};
```

Go 侧把 base64 解出来按到达顺序拼成完整媒体流。**这一步能救回"能播但嗅不到 URL"的一大类站点**——也是 videodl 目前最大的缺口。

**方案 B：代理 `URL.createObjectURL`**，拿到 Blob 对象后调它的 `arrayBuffer()` 读出来。比 A 简单，但只能覆盖"整个文件一次性做成 Blob"的站，流式的（MSE）覆盖不到。

**方案 C：CDP `Fetch.enable` + `Fetch.getResponseBody`**，在网络层直接取响应体，不用碰页面 JS。对 `m3u8/mpd/JSON` 这类小文本最干净，对分片视频体量太大不划算。

> **生产上的组合**：A 主干 + C 兜文本接口 + B 兜简单站点；第 3 层录制留作最后手段（本期不做）。

### 还要堵的洞：Worker 里的请求

很多播放器把取流逻辑扔进 WebWorker，主线程的 `window.fetch` 钩子是**看不见**的。所以注入脚本必须同时挂：

```js
const _W = self.Worker;
self.Worker = function (...args) { hookInside(args[0]); return new _W(...args); };
```

以及在 worker 内部也要能执行钩子（用 `Worker.prototype.postMessage` 之外，更稳的是走 CDP 层监听，因为 CDP 能看到所有 target 的请求）。

---

## 三、一个绕不开的架构约束：Wails 做不了原生网络拦截

**这是本项目最关键的架构判断，必须先说清楚。**

- Wails v2/v3 **没有对外暴露** WebView2 的 `AddWebResourceRequestedFilter` / `WebResourceRequested`，也没有 CDP 通道。社区 issue [wails#4685](https://github.com/wailsapp/wails/issues/4685) 正在要这个能力，**尚未实现**。macOS 的 `WKURLSchemeHandler`、Linux 的 `WebKitURISchemeRequest` 同理不可用。
- 也就是说：**想让 Wails 自己的窗口去嗅探，做不到。** 只能靠 JS 注入（覆盖面窄）或本地代理（重、要装证书）。

**本项目现在的解法是对的**：用 `go-rod` 另起一个真 Chromium（装好的 Edge/Chrome），走完整 CDP。
> `internal/sniffer/deep.go` 的 `DeepProbe` 用无头浏览器、`internal/sniffer/watch.go` 的 `StartWatch` 用有头窗口——这个"外挂浏览器"路线必须保持，不要试图改回 Wails 窗口。

**但现在的实现只用了 JS 注入，没有用 CDP 的网络域**，白白浪费了 rod 的能力。这是下一阶段最该补的：

```go
// 现在的做法：只注入 JS（会漏 worker / iframe / service worker）
page.EvalOnNewDocument(hookJS)

// 应该补上的做法：CDP 网络域全量监听
_ = proto.NetworkEnable{}.Call(page)
go page.EachEvent(
    func(e *proto.NetworkRequestWillBeSent)  { /* 看 URL + ResourceType */ },
    func(e *proto.NetworkResponseReceived)   { /* 看 MIME 类型，比扩展名可靠得多 */ },
)()

// 拦截响应体（拿接口里藏在 JSON 中的播放地址）
_ = proto.FetchEnable{Patterns: []*proto.FetchRequestPattern{
    {URLPattern: "*", RequestStage: proto.FetchRequestStageResponse},
}}.Call(page)
```

**为什么必须上 CDP**：现在只按**扩展名**判断（`.m3u8/.mpd/.mp4`），而真实世界的 CDN 地址大量是**无扩展名**的，靠 MIME 才能认出来：

| MIME | 含义 |
|---|---|
| `application/vnd.apple.mpegurl` / `application/x-mpegURL` | HLS 清单 |
| `application/dash+xml` | DASH 清单 |
| `video/mp4` / `video/mp2t` / `video/webm` | 媒体片段 |
| `application/octet-stream` + 响应体开头是 `#EXTM3U` | 伪装过的清单 |

---

## 四、差距分析：夸克 vs videodl 现状

### 4.1 代码地图（现状）

| 模块 | 文件 | 能力 |
|---|---|---|
| 轻嗅探 | `internal/sniffer/sniffer.go` | 正则扫 HTML + `<video>` 标签 + 直链后缀判断 |
| 重嗅探 | `internal/sniffer/deep.go` | 无头 Chromium + JS 钩子（fetch/XHR.open/createObjectURL）+ MutationObserver + Resource Timing |
| 浏览嗅探 | `internal/sniffer/watch.go` | 有头窗口 + 3s 轮询增量 + `memScanJS` 扫内存变量 |
| HLS | `playlist.go` / `downloader.go` | master 选最高档、AES-128 CBC 解密、并发分片、ffmpeg concat |
| DASH | `dash.go` | SegmentTemplate + Timeline / duration 推算、音视频分离混流 |
| 直链 | `downloader.go` | 单连接 + `.part` Range 续传 |
| 续传 | `tasks.go` | manifest 快照比对（分片数 + 首尾 URL） |
| 限速 | `throttle.go` | 全局简易令牌桶 |
| 任务 | `app.go` | 后台 goroutine、暂停/继续/删除、事件推送、持久化 |
| UI | `frontend/src/` | Sniffer / TaskList / SettingsPanel / Player 四个组件 |

### 4.2 逐项差距表

**A. 嗅探层**

| # | 夸克 / 应有能力 | videodl 现状 | 严重度 |
|---|---|---|---|
| A1 | 拦截 MSE 喂给播放器的数据（blob 回源） | ❌ 只把 `blob:` 当线索展示，注释写"真实回源看同页 m3u8/fetch 项" | **P0** |
| A2 | CDP 网络域全量监听（含 worker/iframe/SW） | ❌ 只用 JS 注入，Worker 内请求全漏 | **P0** |
| A3 | 按 MIME 类型识别，而非扩展名 | ❌ 只按 `.m3u8/.mpd/.mp4` 后缀 | **P0** |
| A4 | 智能等待"播放器就绪"再采集 | ⚠️ 固定 `time.Sleep(12s)` + 静音 autoplay，无轮询/无滚动触发 | P1 |
| A5 | 自动预解析 master 拿清晰度 | ⚠️ 要用户手动点"解析清晰度" | P1 |
| A6 | 识别 `ts/m4s/aac/vtt/mp3` 等更多类型 | ❌ `classifyDeep` 明确丢弃 `.ts/.m4s` | P1 |
| A7 | 接口 JSON 里藏的播放地址 | ⚠️ 只有 `memScanJS` 粗扫 window 变量 | P1 |
| A8 | 非侵入式"发现 N 个"提示 | ⚠️ 用 toast，体验接近；浏览嗅探有累计提示 | ✅ 基本达标 |
| A9 | 最后兜底录制 | ❌ 无 | P3 |

**B. 下载层**

| # | 夸克 / 应有能力 | videodl 现状 | 严重度 |
|---|---|---|---|
| B1 | 直链**多线程 Range 分块**下载 | ❌ **单连接**顺序 `Read/Write`，长视频慢数倍 | **P0** |
| B2 | HLS `#EXT-X-MAP`（fMP4 init 段） | ❌ **完全没解析**，fMP4 型 HLS 下出来缺 init 段、无法播放 | **P0** |
| B3 | 分片续传的完整性保证 | ❌ **真 bug**：只判 `Size()>0`，半个分片会被当成已完成 → 成片花屏 | **P0** |
| B4 | `#EXT-X-BYTERANGE` 分片 | ❌ 无 | P1 |
| B5 | 直播（HLS 无 `ENDLIST` / DASH `r=-1`） | ❌ HLS 下完当前清单就收、DASH 直接报错 | P1 |
| B6 | 多音轨 / 字幕轨 | ❌ DASH 只取第一条 audio；HLS `EXT-X-MEDIA` 不解析 | P1 |
| B7 | HTTP 连接池复用 | ⚠️ 每个任务 `newClient()`，无 Transport 调优 | P1 |
| B8 | 限速精度与多任务隔离 | ⚠️ 手写令牌桶、全局共享、精度差；建议换 `golang.org/x/time/rate` | P1 |
| B9 | 代理支持（HTTP/SOCKS5/系统代理） | ❌ 无。本机刚被死代理坑过，这个需求很实在 | P1 |
| B10 | 分片失败按状态码分级重试 | ⚠️ 固定 3 次指数退避，429/5xx 没有区别对待 | P2 |
| B11 | 断点续传覆盖率 | ⚠️ 直链支持；DASH 靠 manifest 比对（清单一变就全清） | P2 |
| B12 | 文件名模板（标题/清晰度/日期） | ❌ 随机时间戳命名 | P2 |
| B13 | P2P / CDN 多域名择优 | ❌ 服务端能力，**放弃** | — |

**C. 管理层 / UI**

| # | 夸克 / 应有能力 | videodl 现状 | 严重度 |
|---|---|---|---|
| C1 | 全局任务并发上限（队列） | ❌ `StartDownload` 直接起 goroutine，10 个任务就是 10×16 = 160 并发连接 | **P0** |
| C2 | 全部暂停 / 全部继续 | ❌ 无 | P1 |
| C3 | 边下边播 | ❌ 无（`previewUrl` store 已定义但没接上） | P2 |
| C4 | 完成后一键播放 | ⚠️ 只有"定位文件" | P2 |
| C5 | 设置项：代理 / UA / 并发任务数 / 文件名模板 / 默认清晰度 | ⚠️ 只有并发分片数 / 目录 / 限速 | P1 |
| C6 | 剪贴板自动识别链接 | ❌ 无 | P2 |
| C7 | 下载历史搜索 / 筛选 | ❌ 无 | P3 |
| C8 | 暂停/恢复/删除/改目录/清空 | ✅ 基本齐全 | ✅ |

**D. 工程**

| # | 问题 | 位置 |
|---|---|---|
| D1 | `uaDesktop` 硬编码 macOS UA，Windows 上跑会被站点识破 | `app.go:21` |
| D2 | `findFFmpeg` 兜底只写了 brew 路径，Windows 靠 PATH | `downloader.go:457` |
| D3 | `tasks.go` 注释"直链暂不支持 Range 续传"与实际实现不一致（已支持），误导 | `tasks.go:20` |
| D4 | Watch 会话 map 无超时/僵尸清理 | `watch.go:28` |
| D5 | 单测已有 5 个文件，但缺 `t.Parallel()` / `goleak` / build tag 隔离（刚装的 `golang-testing` skill 正好治这个） | `internal/downloader/*_test.go` |

---

## 五、复刻路线图

按"投入产出比"排序，**P0 全是能直接决定成败的**。

### 第一期 P0：把嗅探覆盖率和下载引擎的地基打对

> **进度（2026-09-28）**：① MSE 劫持、③ 直链多线程 Range 已完成并有测试覆盖；
> 其余 4 项待做。

| 任务 | 落点 | 要点 | 状态 |
|---|---|---|---|
| **① MSE 劫持 / blob 回源** | `internal/sniffer/mse.go` + 注入脚本 | `appendBuffer` 覆盖 → base64 → 队列按预算拉取；按 `addSourceBuffer` 的 mimeType 分轨拼接；总量上限保护 | ✅ 已完成（`-mse`） |
| **② CDP 网络域替换 JS 钩子主干** | `deep.go` / `watch.go` | `Network.enable` + `Network.responseReceived` 按 **MIME** 判定；同时对每个 target（worker/iframe）都 `Network.enable` | ⬜ 待做 |
| **③ 直链多线程 Range 分块** | `internal/downloader/multirange.go` | HEAD 拿 `Content-Length`+`Accept-Ranges` → 按 N 等分 → 每块独立 goroutine + `Range` → 预分配文件 + `WriteAt`；不支持 Range 时自动回落单连接 | ✅ 已完成（≥2MB 自动启用 + 变更检测） |
| **④ HLS `#EXT-X-MAP`** | `playlist.go` | 解析 `EXT-X-MAP:URI=` → init 段先下载 → concat 时排在分片最前；顺带支持 `BYTERANGE` | ⬜ 待做 |
| **⑤ 分片续传原子化** | `downloader.go:fetchSegments` | 写 `.tmp` → `os.Rename` 落定；manifest 里记每个分片字节数；启动时校验不符则重下 | ⬜ 待做 |
| **⑥ 全局任务调度器** | `app.go` 新增 `queue.go` | 最大同时下载任务数（可配，默认 3）+ FIFO 队列；分片并发按"全局剩余额度"分配而不是每任务固定 16 | ⬜ 待做 |

> ①③ 的实现细节与踩坑记录见代码注释与 `README.md` 的「开发注意」。
> 顺带修掉了一组**既有严重 bug**：rod 的 `page.Eval` 只接受函数形式，
> 而原有 `collectJS` / `memScanJS` / 存活探针 `Eval("1")` / 两处自动播放脚本
> 全是表达式或 IIFE，导致**重嗅探与浏览嗅探在生产中不可用**（错误被 `_` 吞掉，无任何提示）。

### 第二期 P1：把"选择"和"稳定"做到位

- 清晰度自动预解析（嗅到 master 立刻出档位列表，无需点击）
- 直播支持：HLS 检测 `EXT-X-ENDLIST` 缺失 → 轮询刷新清单持续追分片；DASH `r=-1` → 按 wall-clock 追
- 多音轨 / 字幕轨：HLS `EXT-X-MEDIA`、DASH 全部 `audio` AdaptationSet 并列可选
- 代理：设置项支持 `http://` / `socks5://` 与"跟随系统代理"，并配 `NO_PROXY` 排除本地
- HTTP 客户端单例化 + Transport 调优：`MaxIdleConnsPerHost=64`、`ForceAttemptHTTP2=true`、`DisableCompression`（视频已是压缩格式）
- 限速换 `golang.org/x/time/rate`：每任务一个 `Limiter`，可各自限速
- 设置项扩展：UA / 并发任务数 / 文件名模板 / 默认清晰度 / 失败重试次数
- UI：全部暂停/继续、任务搜索、失败原因可读化

### 第三期 P2：体验层

- **边下边播**：Go 侧起本地 `http://127.0.0.1:<随机端口>` 的 HTTP 服务（支持 `Range`），把已下分片按序对外提供一个虚拟 `.mp4`/`.m3u8`；前端 `<video>` 直接播。`previewUrl` store 已经在等这个了。
- 文件名模板：`{title}-{resolution}-{date}`，title 从嗅探阶段顺手带回来
- 剪贴板监听：前台时检测到链接自动填进输入框
- 完成后一键播放 + 托盘通知点击直达

### 第四期 P3：兜底与打磨

- MediaRecorder 录制兜底（拿不到数据时的最后手段）
- 下载历史持久化 + 搜索
- 打包：`wails build` 出 Windows/macOS 双端；校验 ffmpeg 缺失时的友好引导

---

## 六、几个容易踩的坑（提前记下）

1. **别指望 Wails 窗口能嗅探** —— 见第三节。外挂浏览器是唯一可行且正确的路线。
2. **`appendBuffer` 的数据是分片的**，一次几十 KB 到几 MB，必须**按到达顺序**拼接，且要注意 `SourceBuffer` 可能是多个（video/audio 分开）→ 按 `mimeType` 分桶。
3. **无头模式的 `<video>` 可能不加载** —— 现在靠 `mute-audio` + `autoplay-policy` 绕过。有的站还需要伪造 `navigator.webdriver=false`、`plugins.length`，否则直接不给你流。
4. **`Content-Length` 经常是 `-1`**（chunked 或动态生成）→ 多线程分块策略必须先 HEAD 验证，拿不到就回落。
5. **不要在 `pace()` 里持锁 sleep** —— 现在的实现在锁外 sleep 是对的，别改坏。
6. **manifest 失效就全清重下** 对直播/短时效 CDN 是灾难 —— 直播场景要改成"保留已下、按序号继续追"。
7. **合规红线**：只做公开、非 DRM 资源。遇到 `Widevine`/`SAMPLE-AES`/私有加密**明确报错退出**，这一点现有代码的方向是对的（`playlist.go:82` / `dash.go` 都有明确拒绝），继续保持。

---

## 七、一句话总结

> 夸克的体感来自 **"不用管，它自己发现，挑最好的，下得稳"**。
> 对应的技术动作只有三件事：**把嗅探做全（MSE 劫持 + CDP 网络域）、把下载做快做稳（多线程 Range + 原子分片 + init 段）、把并发管起来（全局调度器）**。
> 这三件之外的东西——P2P、CDN 择优、云离线——都是服务端能力，**放弃**。

---

*调研来源：夸克官方/教程站点的功能描述、猫抓 Cat-Catch 架构解析、N_m3u8DL-RE 能力矩阵、lux/annie 下载器实现、Wails issue #4685（WebView2 拦截能力缺失）、WebView2 CDP 官方文档。*
