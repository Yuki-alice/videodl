package sniffer

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// WatchUpdate 一次轮询的增量结果：只有上次之后新发现的候选。
type WatchUpdate struct {
	Title      string      `json:"title"`
	Candidates []Candidate `json:"candidates"`
	Alive      bool        `json:"alive"`
}

type watchSession struct {
	pageURL string
	browser *rod.Browser
	page    *rod.Page
	seen    map[string]bool
}

var (
	watchMu sync.Mutex
	watches = map[string]*watchSession{}
)

// StartWatch 打开一个受控的有头浏览器窗口（独立资料目录，需在里面重新登录），
// 用户在里面正常逛站，前端轮询 PollWatch 即实现“边逛边发现”，对齐夸克的自动嗅探提示。
func StartWatch(startURL string) (string, error) {
	bin, err := findBrowser()
	if err != nil {
		return "", err
	}
	launchURL, err := launcher.New().Bin(bin).Headless(false).
		Set("autoplay-policy", "no-user-gesture-required").
		Set("mute-audio").
		Launch()
	if err != nil {
		return "", fmt.Errorf("浏览器启动失败: %w", err)
	}
	browser := rod.New().ControlURL(launchURL)
	if err := browser.Connect(); err != nil {
		return "", fmt.Errorf("连接浏览器失败: %w", err)
	}
	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		_ = browser.Close()
		return "", fmt.Errorf("建新标签页失败: %w", err)
	}
	page = page.Timeout(60 * time.Second)
	if _, err := page.EvalOnNewDocument(hookJS); err != nil {
		_ = page.Close()
		_ = browser.Close()
		return "", fmt.Errorf("注入钩子失败: %w", err)
	}
	if startURL != "" {
		if err := page.Navigate(startURL); err != nil {
			_ = page.Close()
			_ = browser.Close()
			return "", fmt.Errorf("打开页面失败: %w", err)
		}
		_ = page.WaitLoad()
	}
	id := fmt.Sprintf("watch-%d", time.Now().UnixNano())
	watchMu.Lock()
	watches[id] = &watchSession{pageURL: startURL, browser: browser, page: page, seen: map[string]bool{}}
	watchMu.Unlock()
	return id, nil
}

// PollWatch 采集钩子 + Resource Timing + video 标签 + HTML/JS内存 四路的新发现。
func PollWatch(id string) (WatchUpdate, error) {
	watchMu.Lock()
	s, ok := watches[id]
	watchMu.Unlock()
	if !ok {
		return WatchUpdate{}, fmt.Errorf("嗅探会话不存在")
	}
	// 用户关了窗口就结束会话。
	if _, err := s.page.Eval(pageAliveJS); err != nil {
		StopWatch(id)
		return WatchUpdate{Alive: false}, fmt.Errorf("浏览器窗口已关闭")
	}
	curURL := s.pageURL
	if info, err := s.page.Info(); err == nil && info.URL != "" {
		curURL = info.URL
	}
	cookie := pageCookies(s.page, curURL)
	title, _ := evalString(s.page, pageTitleJS)

	raw, err := evalString(s.page, collectJS)
	if err != nil {
		return WatchUpdate{Alive: true, Title: title}, nil // 偶发失败不断开
	}
	var got struct {
		Hooked    []string `json:"hooked"`
		Resources []string `json:"resources"`
		Videos    []string `json:"videos"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		return WatchUpdate{Alive: true, Title: title}, nil
	}
	mem, _ := evalStrList(s.page, memScanJS)

	var fresh []Candidate
	add := func(u, kind, label string) {
		key := kind + "\x00" + u
		if u == "" || s.seen[key] {
			return
		}
		s.seen[key] = true
		fresh = append(fresh, Candidate{URL: u, Kind: kind, Label: label, Referer: curURL, UserAgent: defaultUA, Cookie: cookie})
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
		if u == "" || startsWithBlob(u) {
			continue
		}
		if k, l, ok := classifyDeep(u); ok {
			add(u, k, l+"（video标签）")
		}
	}
	for _, u := range mem {
		if k, l, ok := classifyDeep(u); ok && k != "blob" {
			add(u, k, l+"（页面脚本）")
		}
	}
	return WatchUpdate{Title: title, Candidates: fresh, Alive: true}, nil
}

// StopWatch 关闭受控浏览器并释放会话。
func StopWatch(id string) {
	watchMu.Lock()
	s, ok := watches[id]
	if ok {
		delete(watches, id)
	}
	watchMu.Unlock()
	if !ok {
		return
	}
	_ = s.page.Close()
	_ = s.browser.Close()
}

func startsWithBlob(u string) bool { return len(u) >= 5 && u[:5] == "blob:" }

func evalStrList(page *rod.Page, js string, args ...interface{}) ([]string, error) {
	res, err := page.Eval(js, args...)
	if err != nil {
		return nil, err
	}
	// JS 返回的是「装着数组的 JSON 字符串」，用 Str() 取出内容后再解成切片。
	var out []string
	if err := json.Unmarshal([]byte(res.Value.Str()), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// memScanJS 扫 HTML 内联脚本 + window 上形如 *url/*video/*play 的变量，
// 抓网都抓不到的地址经常藏在这里。必须是函数形式（见 evalString 注释）。
const memScanJS = `() => {
  var out = [];
  var pat = /https?:\/\/[^\s"'<>\\]+\.(m3u8|mpd|mp4|webm|flv|mov)([^\s"'<>]*)?/gi;
  function pushAll(s){
    if (typeof s !== 'string') return;
    var m; pat.lastIndex = 0;
    while ((m = pat.exec(s)) && out.length < 100) {
      if (out.indexOf(m[0]) < 0) out.push(m[0]);
    }
  }
  try {
    pushAll(document.documentElement.innerHTML || '');
  } catch(e){}
  try {
    var keys = Object.keys(window);
    for (var i = 0; i < keys.length && out.length < 100; i++) {
      var k = keys[i];
      if (!/url|video|play|src|stream|media|file|hls|dash/i.test(k)) continue;
      try { pushAll(window[k]); } catch(e){}
    }
  } catch(e){}
  return JSON.stringify(out);
}`

// pageAliveJS 存活探针，同样是函数形式；表达式 `1` 会让 rod 抛 TypeError。
const pageAliveJS = `() => 1`

// pageTitleJS 取当前页面标题。
const pageTitleJS = `() => document.title || ''`
