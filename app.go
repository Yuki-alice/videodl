package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/gen2brain/beeep"
	"videodl/internal/downloader"
	"videodl/internal/sniffer"
)

const uaDesktop = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

// App struct
type App struct {
	ctx   context.Context
	mu    sync.Mutex
	tasks map[string]*taskEntry
}

type taskEntry struct {
	info   *TaskInfo
	cancel context.CancelFunc
}

// TaskInfo 下载任务：GUI 轮询 + 事件 task://progress 推送进度。
// 持久化到 ~/.videodl/tasks/<id>/task.json，重启后可继续。
type TaskInfo struct {
	ID       string `json:"id"`
	InputURL string `json:"inputUrl"`
	MediaURL string `json:"mediaUrl"`
	Referer  string `json:"referer"`
	Cookie   string `json:"cookie"`
	OutDir   string `json:"outDir"`
	FileName string `json:"fileName"`
	Workers  int    `json:"workers"`
	Status   string `json:"status"` // downloading | paused | done | failed
	Done     int    `json:"done"`
	Total    int    `json:"total"`
	Output   string `json:"output"`
	Error    string `json:"error"`
}

// DlConfig 下载设置，持久化到 ~/.videodl/config.json。
type DlConfig struct {
	Workers       int    `json:"workers"`
	OutDir        string `json:"outDir"`
	SpeedLimitKBs int64  `json:"speedLimitKBs"` // 0 = 不限速
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{tasks: map[string]*taskEntry{}}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.loadPersistedTasks()
	downloader.SetSpeedLimit(a.GetConfig().SpeedLimitKBs << 10)
}

// Probe 轻嗅探：输入页面URL/视频URL -> 候选直链列表。
func (a *App) Probe(pageURL string) ([]sniffer.Candidate, error) {
	cands, err := sniffer.Probe(pageURL)
	if err != nil {
		return nil, err
	}
	if cands == nil {
		return []sniffer.Candidate{}, nil
	}
	return cands, nil
}

// DeepProbe 重嗅探：无头浏览器渲染页面，拦截 XHR/fetch + blob 回源。
// waitSec 等待播放器触发请求的时间，一般 10~15 秒。
func (a *App) DeepProbe(pageURL string, waitSec int) ([]sniffer.Candidate, error) {
	cands, err := sniffer.DeepProbe(pageURL, waitSec)
	if err != nil {
		return nil, err
	}
	if cands == nil {
		return []sniffer.Candidate{}, nil
	}
	return cands, nil
}

// GetVariants 解析清晰度档位：m3u8 走 HLS，mpd 走 DASH；其他返回单档默认。
func (a *App) GetVariants(mediaURL, referer, cookie string) ([]downloader.Variant, error) {
	h := downloader.Headers{Referer: referer, UserAgent: uaDesktop, Cookie: cookie}
	if strings.Contains(strings.ToLower(mediaURL), ".mpd") {
		return downloader.ParseDashVariants(context.Background(), mediaURL, h)
	}
	return downloader.ParseVariants(context.Background(), mediaURL, referer, cookie, uaDesktop)
}

// GetConfig 读取下载设置（带默认值）。
func (a *App) GetConfig() DlConfig {
	cfg := DlConfig{Workers: 16, OutDir: defaultOutDir()}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	var saved DlConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		return cfg
	}
	if saved.Workers >= 1 && saved.Workers <= 64 {
		cfg.Workers = saved.Workers
	}
	if saved.OutDir != "" {
		cfg.OutDir = saved.OutDir
	}
	if saved.SpeedLimitKBs >= 0 {
		cfg.SpeedLimitKBs = saved.SpeedLimitKBs
	}
	return cfg
}

// SetConfig 保存下载设置。
func (a *App) SetConfig(cfg DlConfig) error {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.Workers > 64 {
		cfg.Workers = 64
	}
	if cfg.OutDir == "" {
		cfg.OutDir = defaultOutDir()
	}
	if cfg.SpeedLimitKBs < 0 {
		cfg.SpeedLimitKBs = 0
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return fmt.Errorf("下载目录不可用: %w", err)
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath(), data, 0o644); err != nil {
		return err
	}
	downloader.SetSpeedLimit(cfg.SpeedLimitKBs << 10)
	return nil
}

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "videodl-config.json"
	}
	dir := filepath.Join(home, ".videodl")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "config.json")
}

// CheckFFmpeg 检查 ffmpeg 是否可用。
func (a *App) CheckFFmpeg() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// StartDownload 创建任务并后台执行：并发下分片 -> AES-128解密 -> ffmpeg合并。
// 分片持久化在任务目录，中断后可 PauseTask/ResumeTask 续传。
func (a *App) StartDownload(inputURL, mediaURL, referer, cookie, outDir string) (string, error) {
	if mediaURL == "" {
		return "", fmt.Errorf("mediaURL 为空")
	}
	if outDir == "" {
		outDir = defaultOutDir()
	}
	id := fmt.Sprintf("task-%d", time.Now().UnixNano())
	cfg := a.GetConfig()
	downloader.SetSpeedLimit(cfg.SpeedLimitKBs << 10)
	if outDir == "" {
		outDir = cfg.OutDir
	}
	info := &TaskInfo{ID: id, InputURL: inputURL, MediaURL: mediaURL, Referer: referer, Cookie: cookie, OutDir: outDir, Workers: cfg.Workers, Status: "downloading"}
	a.mu.Lock()
	a.tasks[id] = &taskEntry{info: info}
	a.mu.Unlock()
	a.persist(info)
	a.runTask(id)
	return id, nil
}

// PauseTask 暂停任务，已下分片保留在任务目录。
func (a *App) PauseTask(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.tasks[id]
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	if e.info.Status != "downloading" {
		return fmt.Errorf("任务不在下载中，无法暂停")
	}
	if e.cancel != nil {
		e.cancel()
	}
	// 最终状态由 runTask 收尾（canceled -> paused）；先行标记，避免竞态下重复暂停。
	e.info.Status = "pausing"
	return nil
}

// ResumeTask 从断点继续任务。
func (a *App) ResumeTask(id string) error {
	a.mu.Lock()
	e, ok := a.tasks[id]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("任务不存在: %s", id)
	}
	if e.info.Status == "downloading" || e.info.Status == "pausing" {
		a.mu.Unlock()
		return fmt.Errorf("任务正在下载中")
	}
	if e.info.Status == "done" {
		a.mu.Unlock()
		return fmt.Errorf("任务已完成")
	}
	e.info.Status = "downloading"
	e.info.Error = ""
	a.mu.Unlock()
	a.persist(e.info)
	a.runTask(id)
	return nil
}

func (a *App) runTask(id string) {
	a.mu.Lock()
	e, ok := a.tasks[id]
	a.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	e.cancel = cancel
	a.mu.Unlock()

	go func() {
		info := e.info
		opt := downloader.Options{
			MediaURL:  info.MediaURL,
			Referer:   info.Referer,
			Cookie:    info.Cookie,
			UserAgent: uaDesktop,
			OutDir:    info.OutDir,
			FileName:  info.FileName,
			Workers:   info.Workers,
		}
		out, err := downloader.DownloadTask(ctx, opt, taskStateDir(id), func(done, total int) {
			a.mu.Lock()
			info.Done, info.Total = done, total
			a.mu.Unlock()
			runtime.EventsEmit(a.ctx, "task://progress", map[string]any{"id": id, "done": done, "total": total})
		})
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			if ctx.Err() == context.Canceled {
				info.Status = "paused" // 用户暂停，保留分片供续传
				runtime.EventsEmit(a.ctx, "task://done", map[string]any{"id": id, "status": "paused"})
			} else {
				info.Status = "failed"
				info.Error = err.Error()
				runtime.EventsEmit(a.ctx, "task://done", map[string]any{"id": id, "status": "failed", "error": info.Error})
				_ = beeep.Notify("VideoDL 下载失败", shortName(info.MediaURL), "")
			}
			a.persist(info)
			return
		}
		info.Status = "done"
		info.Output = out
		runtime.EventsEmit(a.ctx, "task://done", map[string]any{"id": id, "status": "done", "output": out})
		_ = beeep.Notify("VideoDL 下载完成", out, "")
		a.persist(info)
	}()
}

// ListTasks 返回全部任务。
func (a *App) ListTasks() []*TaskInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*TaskInfo, 0, len(a.tasks))
	for _, e := range a.tasks {
		cp := *e.info
		out = append(out, &cp)
	}
	return out
}

// DeleteTask 删除任务（含分片目录）；deleteFiles=true 连成片一起删。
func (a *App) DeleteTask(id string, deleteFiles bool) error {
	a.mu.Lock()
	e, ok := a.tasks[id]
	if ok {
		if e.cancel != nil {
			e.cancel()
		}
		delete(a.tasks, id)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("任务不存在: %s", id)
	}
	_ = os.RemoveAll(taskStateDir(id))
	if deleteFiles && e.info.Output != "" {
		_ = os.Remove(e.info.Output)
	}
	return nil
}

// StartWatch 打开受控浏览器窗口，返回会话 id，前端轮询 PollWatch 实现边逛边发现。
func (a *App) StartWatch(startURL string) (string, error) {
	return sniffer.StartWatch(startURL)
}

// PollWatch 取回自上次以来新发现的候选。
func (a *App) PollWatch(id string) (sniffer.WatchUpdate, error) {
	u, err := sniffer.PollWatch(id)
	if err != nil {
		return sniffer.WatchUpdate{}, err
	}
	if u.Candidates == nil {
		u.Candidates = []sniffer.Candidate{}
	}
	return u, nil
}

// StopWatch 关闭受控浏览器。
func (a *App) StopWatch(id string) {
	sniffer.StopWatch(id)
}

func shortName(mediaURL string) string {
	if len(mediaURL) > 120 {
		return mediaURL[:120] + "…"
	}
	return mediaURL
}

// RevealOutput 在文件管理器中定位成片。
func (a *App) RevealOutput(path string) error {
	if path == "" {
		return fmt.Errorf("文件路径为空")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("文件不存在: %s", path)
	}
	var cmd *exec.Cmd
	switch stdruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	case "windows":
		cmd = exec.Command("explorer", "/select,", path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	return cmd.Start()
}

func tasksRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".videodl-tasks"
	}
	dir := filepath.Join(home, ".videodl", "tasks")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func taskStateDir(id string) string {
	dir := filepath.Join(tasksRoot(), id)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func (a *App) persist(info *TaskInfo) {
	data, _ := json.Marshal(info)
	_ = os.WriteFile(filepath.Join(taskStateDir(info.ID), "task.json"), data, 0o644)
}

// loadPersistedTasks 启动时恢复历史任务；上次未完成的标记为 paused，可一键续传。
func (a *App) loadPersistedTasks() {
	root := tasksRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, en := range entries {
		if !en.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, en.Name(), "task.json"))
		if err != nil {
			continue
		}
		var info TaskInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if info.Status == "downloading" || info.Status == "pausing" {
			info.Status = "paused"
		}
		a.tasks[info.ID] = &taskEntry{info: &info}
	}
}

func defaultOutDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	dir := filepath.Join(home, "Downloads", "videodl")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}
