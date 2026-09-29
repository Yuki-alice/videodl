package sniffer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// DeepProbe 重嗅探：起无头 Chromium 内核（优先本机 Edge/Chrome），
// 用三路抓取：JS hook 拦截 fetch/XHR + 监听 video 标签动态插入 +
// performance Resource Timing 回扫，连原生 <video src> 的请求也跑不掉。
// blob: 链接会一并返回（kind=blob），它的真实回源地址通常就在同页的 fetch/m3u8 项里。
func DeepProbe(pageURL string, waitSec int) ([]Candidate, error) {
	if waitSec <= 0 {
		waitSec = 12
	}
	if waitSec > 60 {
		waitSec = 60
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

	// 在 document 生成前注入，保证最早的 XHR/fetch 也被 hook 到。
	if _, err := page.EvalOnNewDocument(hookJS); err != nil {
		return nil, fmt.Errorf("注入钩子失败: %w", err)
	}
	if err := page.Navigate(pageURL); err != nil {
		return nil, fmt.Errorf("打开页面失败: %w", err)
	}
	_ = page.WaitLoad()
	time.Sleep(time.Duration(waitSec) * time.Second)
	// 静音自动播放，逼播放器发出媒体请求。必须是函数形式，否则 rod 会抛错被静默吞掉。
	_, _ = page.Eval(`() => [...document.querySelectorAll('video')].forEach(v=>{try{v.muted=true;v.play().catch(()=>{});}catch(e){}})`)
	time.Sleep(3 * time.Second)

	raw, err := evalString(page, collectJS)
	if err != nil {
		return nil, fmt.Errorf("读取嗅探结果失败: %w", err)
	}
	var got struct {
		Hooked    []string `json:"hooked"`
		Resources []string `json:"resources"`
		Videos    []string `json:"videos"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return nil, fmt.Errorf("解析嗅探结果失败: %w", err)
	}

	seen := map[string]bool{}
	var out []Candidate
	cookie := pageCookies(page, pageURL) // 登录态透传：需要 Cookie 鉴权的站才能回源下载
	add := func(u, kind, label string) {
		if u == "" || seen[kind+"\x00"+u] {
			return
		}
		seen[kind+"\x00"+u] = true
		out = append(out, Candidate{URL: u, Kind: kind, Label: label, Referer: pageURL, UserAgent: defaultUA, Cookie: cookie})
	}
	mem, _ := evalStrList(page, memScanJS)
	for _, u := range mem {
		if k, l, ok := classifyDeep(u); ok && k != "blob" {
			add(u, k, l+"（页面脚本）")
		}
	}
	for _, u := range got.Hooked {
		if k, l, ok := classifyDeep(u); ok {
			add(u, k, l)
		}
	}
	for _, u := range got.Resources {
		if k, l, ok := classifyDeep(u); ok && k != "blob" {
			add(u, k, l+"（resource）")
		}
	}
	for _, u := range got.Videos {
		if u == "" || strings.HasPrefix(u, "blob:") {
			continue
		}
		if k, l, ok := classifyDeep(u); ok {
			add(u, k, l+"（video标签）")
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out, nil
}

func findBrowser() (string, error) {
	if p := os.Getenv("VIDEODL_BROWSER"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("VIDEODL_BROWSER 指向的文件不存在: %s", p)
	}
	cands := []string{
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/chromium",
		"/usr/bin/google-chrome",
		"C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe",
		"C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe",
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
		"C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("没找到 Chromium 内核浏览器，请安装 Edge/Chrome 或设 VIDEODL_BROWSER 环境变量指向浏览器可执行文件")
}

// evalString 执行 JS 并把返回的字符串解出来。
//
// 两个坑：
//  1. rod 的 Eval 会把入参包成 `function(){ return (js).apply(this, arguments) }`，
//     所以 js 必须是**函数形式**（`() => ...` / `function(){}`），不能是表达式或 IIFE；
//     需要入参时通过 args 传入。
//  2. gson.JSON.String() 等价于 Sprintf("%v", 已解析的值)，对 JSON 字符串会得到
//     **去引号后的内容**。所以这里直接用 Str() 取值，不要再 json.Unmarshal 一遍，
//     否则返回对象时会报 "cannot unmarshal object into Go value of type string"。
func evalString(page *rod.Page, js string, args ...interface{}) (string, error) {
	res, err := page.Eval(js, args...)
	if err != nil {
		return "", err
	}
	return res.Value.Str(), nil
}

// pageCookies 读取页面 Cookie（含 HttpOnly，document.cookie 拿不到的也能透传），
// 拼成 Cookie 请求头供回源下载使用。失败返回空串，不阻断嗅探。
func pageCookies(page *rod.Page, pageURL string) string {
	cookies, err := page.Cookies([]string{pageURL})
	if err != nil || len(cookies) == 0 {
		return ""
	}
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

func classifyDeep(raw string) (kind, label string, ok bool) {
	if strings.HasPrefix(raw, "blob:") {
		return "blob", "blob 链接（真实回源看同页 m3u8/fetch 项）", true
	}
	l := strings.ToLower(strings.Split(raw, "?")[0])
	switch {
	case strings.HasSuffix(l, ".m3u8"):
		return "m3u8-media", "重嗅探 HLS", true
	case strings.HasSuffix(l, ".mpd"):
		return "mpd", "重嗅探 DASH", true
	case strings.HasSuffix(l, ".mp4") || strings.HasSuffix(l, ".webm") ||
		strings.HasSuffix(l, ".mov") || strings.HasSuffix(l, ".flv"):
		return "mp4", "重嗅探直链", true
	}
	// .ts/.m4s 纯分片噪音太大，直接丢弃（定位靠上层 m3u8）。
	return "", "", false
}

const hookJS = `
window.__sniffed = [];
(function(){
  var pat = /m3u8|\.mpd|\.mp4|\.webm|\.flv|\.mov|blob:/i;
  function push(u){ try{ if(typeof u==='string' && pat.test(u) && window.__sniffed.indexOf(u)<0){ window.__sniffed.push(u); } }catch(e){} }
  var _fetch = window.fetch;
  window.fetch = function(u, o){ try{ push(typeof u==='string'?u:(u&&u.url)); }catch(e){} return _fetch.apply(this, arguments); };
  var _open = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function(m, u){ try{ push(u); }catch(e){} return _open.apply(this, arguments); };
  if (URL.createObjectURL) {
    var _create = URL.createObjectURL.bind(URL);
    URL.createObjectURL = function(o){ try{ var u=_create(o); push(u); return u; }catch(e){ return _create(o); } };
  }
  function scan(root){ try{ var els=(root&&root.querySelectorAll)?root.querySelectorAll('video,source'):[]; for(var i=0;i<els.length;i++){ push(els[i].src||els[i].currentSrc); } }catch(e){} }
  function arm(){
    try{
      new MutationObserver(function(muts){
        muts.forEach(function(m){ m.addedNodes.forEach(function(n){ if(n&&n.nodeType===1){ try{push(n.src);}catch(e){} scan(n); } }); });
      }).observe(document.documentElement,{childList:true,subtree:true});
    }catch(e){}
    scan(document);
  }
  if (document.readyState==='loading'){ document.addEventListener('DOMContentLoaded',arm); } else { arm(); }
})();
`

// collectJS 必须是函数形式：rod 会对它调用 .apply(this, arguments)。
const collectJS = `() => JSON.stringify({
  hooked: window.__sniffed || [],
  resources: performance.getEntriesByType('resource').map(function(r){return r.name;}),
  videos: Array.prototype.map.call(document.querySelectorAll('video,source'), function(e){return e.src||e.currentSrc||'';})
})`
