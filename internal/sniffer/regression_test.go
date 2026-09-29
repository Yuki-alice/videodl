package sniffer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// rod 的 Eval 会把入参包成 `function(){ return (js).apply(this, arguments) }`，
// 所以凡是交给 page.Eval 的 JS 都必须是**函数形式**。写成表达式或 IIFE 会抛
// "xxx.apply is not a function" —— 更糟的是很多调用点的错误被 `_` 吞掉，
// 表现为「功能静默失效」而非报错。这个用例守住这类退化。
func TestEvalScriptsAreFunctionForm(t *testing.T) {
	cases := map[string]string{
		"collectJS":   collectJS,
		"memScanJS":   memScanJS,
		"drainFnJS":   drainFnJS,
		"pageAliveJS": pageAliveJS,
		"pageTitleJS": pageTitleJS,
	}
	for name, js := range cases {
		t.Run(name, func(t *testing.T) {
			s := strings.TrimSpace(js)
			if s == "" {
				t.Fatal("脚本为空")
			}
			if !strings.HasPrefix(s, "(") && !strings.HasPrefix(s, "function") && !strings.HasPrefix(s, "async") {
				t.Fatalf("不是函数形式，rod 会抛 .apply not a function: %.60q", s)
			}
			if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")()") {
				t.Fatalf("是自执行 IIFE，应改成函数定义: %.60q", s)
			}
		})
	}
}

// mseHookJS 走的是 EvalOnNewDocument（直接注入源码，不做 .apply 包装），
// 所以它**应该**是 IIFE。两者契约不同，分开断言以免误改。
func TestMSEHookIsIIFE(t *testing.T) {
	s := strings.TrimSpace(mseHookJS)
	if !strings.HasPrefix(s, "(function") {
		t.Fatalf("mseHookJS 供 EvalOnNewDocument 使用，应为 IIFE: %.40q", s)
	}
}

// newLocalMediaServer 起一个本地站点：页面内嵌 <video> 指向 m3u8。
// 用它可以在不联外网的前提下验证嗅探链路真的能跑通。
func newLocalMediaServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/media/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=640x360\n/lo.m3u8\n")
	})
	mux.HandleFunc("/page.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><body><video src="/media/master.m3u8" muted></video></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("VIDEODL_E2E") == "" {
		t.Skip("set VIDEODL_E2E=1 to run")
	}
	if _, err := findBrowser(); err != nil {
		t.Skipf("本机没有可用 Chromium 内核浏览器: %v", err)
	}
}

// 端到端重嗅探：能跑完 collectJS 并认出页面里的 m3u8。
// 如果 collectJS 又被写成非函数形式，这里会失败（而 DeepProbe 以前是直接报
// 「读取嗅探结果失败」）。
func TestDeepProbeFindsM3U8OnLocalPage(t *testing.T) {
	requireE2E(t)
	srv := newLocalMediaServer(t)

	cands, err := DeepProbe(srv.URL+"/page.html", 3)
	if err != nil {
		t.Fatalf("DeepProbe: %v", err)
	}
	for _, c := range cands {
		t.Logf("候选: kind=%s url=%s", c.Kind, c.URL)
	}
	for _, c := range cands {
		if strings.Contains(c.URL, "/media/master.m3u8") {
			return
		}
	}
	t.Fatalf("未从本地页面嗅到 m3u8（collectJS 可能又写成了非函数形式）: 共 %d 个候选", len(cands))
}

// 端到端浏览嗅探：PollWatch 不应因存活探针写错而立刻报「浏览器窗口已关闭」。
// 修复前 `page.Eval("1")` 必然抛错，导致整个浏览嗅探不可用。
func TestWatchSessionStaysAlive(t *testing.T) {
	requireE2E(t)
	srv := newLocalMediaServer(t)

	id, err := StartWatch(srv.URL + "/page.html")
	if err != nil {
		t.Fatalf("StartWatch: %v", err)
	}
	defer StopWatch(id)

	time.Sleep(2 * time.Second)
	u, err := PollWatch(id)
	if err != nil {
		t.Fatalf("PollWatch 报错（存活探针或 collectJS 写错会让会话秒断）: %v", err)
	}
	if !u.Alive {
		t.Fatal("会话应保持存活")
	}
	for _, c := range u.Candidates {
		if strings.Contains(c.URL, "/media/master.m3u8") {
			return
		}
	}
	t.Fatalf("浏览嗅探未发现页面内 m3u8: 标题=%q 候选数=%d", u.Title, len(u.Candidates))
}
