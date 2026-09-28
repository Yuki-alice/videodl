package downloader

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DASH(MPD) 下载：只做明文 VOD（SegmentTemplate + SegmentTimeline），
// Widevine 等 DRM 直接报错。音视频分离时下载后 ffmpeg 混流。
// 分档选择通过 URL fragment 传递（#videodl-rep=<id>，fragment 不发往服务器），
// 不带 fragment 默认取最高码率。

type mpdFile struct {
	XMLName xml.Name  `xml:"MPD"`
	BaseURL string    `xml:"BaseURL"`
	MediaPresentationDuration string `xml:"mediaPresentationDuration,attr"`
	Periods []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	Duration        string           `xml:"duration,attr"`
	BaseURL         string           `xml:"BaseURL"`
	SegmentTemplate *mpdSegTemplate `xml:"SegmentTemplate"`
	AdaptationSets  []mpdAdapt       `xml:"AdaptationSet"`
}

type mpdAdapt struct {
	MimeType        string           `xml:"mimeType,attr"`
	ContentType     string           `xml:"contentType,attr"`
	SegmentTemplate *mpdSegTemplate `xml:"SegmentTemplate"`
	Representations []mpdRep         `xml:"Representation"`
}

type mpdRep struct {
	ID         string           `xml:"id,attr"`
	Bandwidth  int              `xml:"bandwidth,attr"`
	Width      int              `xml:"width,attr"`
	Height     int              `xml:"height,attr"`
	Codecs     string           `xml:"codecs,attr"`
	BaseURL    string           `xml:"BaseURL"`
	SegmentTemplate *mpdSegTemplate `xml:"SegmentTemplate"`
}

type mpdSegTemplate struct {
	Initialization string       `xml:"initialization,attr"`
	Media          string       `xml:"media,attr"`
	StartNumber    *int         `xml:"startNumber,attr"`
	Duration       *int64       `xml:"duration,attr"`
	Timescale      *int         `xml:"timescale,attr"`
	Timeline       *mpdTimeline `xml:"SegmentTimeline"`
}

type mpdTimeline struct {
	Entries []mpdS `xml:"S"`
}

type mpdS struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R *int   `xml:"r,attr"`
}

var templateVarRe = regexp.MustCompile(`\$([A-Za-z]+)(%0\d+d)?\$`)

// dashTrack 一条音/视频轨的分片表。
type dashTrack struct {
	kind     string // video | audio
	initURL  string
	segURLs  []string
	repID    string
}

// splitFrag 拆出 #videodl-rep= 选择器。
func splitFrag(rawURL string) (string, string) {
	if i := strings.Index(rawURL, "#"); i >= 0 {
		frag := rawURL[i+1:]
		if strings.HasPrefix(frag, "videodl-rep=") {
			return rawURL[:i], strings.TrimPrefix(frag, "videodl-rep=")
		}
		return rawURL[:i], ""
	}
	return rawURL, ""
}

func fetchMPD(ctx context.Context, client *http.Client, rawURL string, h Headers) (*mpdFile, string, error) {
	clean, _ := splitFrag(rawURL)
	data, err := doGet(ctx, client, clean, h)
	if err != nil {
		return nil, "", err
	}
	var m mpdFile
	if err := xml.Unmarshal(data, &m); err != nil {
		return nil, "", fmt.Errorf("MPD 解析失败: %w", err)
	}
	if len(m.Periods) == 0 {
		return nil, "", fmt.Errorf("MPD 里没有 Period")
	}
	return &m, clean, nil
}

func mpdBase(cleanURL string, m *mpdFile, p *mpdPeriod) string {
	base := cleanURL
	if m.BaseURL != "" {
		base = resolve(base, m.BaseURL)
	}
	if p.BaseURL != "" {
		base = resolve(base, p.BaseURL)
	}
	return base
}

func isVideoAdapt(a *mpdAdapt) bool {
	return strings.HasPrefix(a.MimeType, "video") || a.ContentType == "video"
}

func isAudioAdapt(a *mpdAdapt) bool {
	return strings.HasPrefix(a.MimeType, "audio") || a.ContentType == "audio"
}

// pickRep 选分档：指定 id 优先，否则最高码率。
func pickRep(reps []mpdRep, wantID string) *mpdRep {
	if wantID != "" {
		for i := range reps {
			if reps[i].ID == wantID {
				return &reps[i]
			}
		}
		return nil
	}
	best := 0
	for i := range reps {
		if reps[i].Bandwidth >= reps[best].Bandwidth {
			best = i
		}
	}
	return &reps[best]
}

func effTemplate(p *mpdPeriod, a *mpdAdapt, r *mpdRep) *mpdSegTemplate {
	if r.SegmentTemplate != nil {
		return r.SegmentTemplate
	}
	if a.SegmentTemplate != nil {
		return a.SegmentTemplate
	}
	return p.SegmentTemplate
}

// expandTrack 展开一条轨的 init + 分片 URL。
// 支持 SegmentTimeline 精确写法，也支持 duration + period 时长推算写法。
func expandTrack(mpdDur string, base string, p *mpdPeriod, a *mpdAdapt, r *mpdRep, kind string) (*dashTrack, error) {
	tpl := effTemplate(p, a, r)
	if tpl == nil || tpl.Media == "" {
		return nil, fmt.Errorf("该 MPD 只支持 SegmentTemplate 写法，暂不支持其他分片索引")
	}
	vars := map[string]string{
		"RepresentationID": r.ID,
		"Bandwidth":        strconv.Itoa(r.Bandwidth),
	}
	track := &dashTrack{kind: kind, repID: r.ID}
	if tpl.Initialization != "" {
		track.initURL = resolve(base, substitute(tpl.Initialization, vars, 0, 0))
	}
	start := 1 // DASH 缺省 startNumber=1
	if tpl.StartNumber != nil {
		start = *tpl.StartNumber
	}
	emit := func(num int, t int64) {
		track.segURLs = append(track.segURLs, resolve(base, substitute(tpl.Media, vars, num, t)))
	}
	if tpl.Timeline != nil {
		num := start
		var t int64
		haveT := false
		for _, e := range tpl.Timeline.Entries {
			if e.T != nil {
				t = *e.T
				haveT = true
			} else if !haveT {
				t = 0
				haveT = true
			}
			rep := 0
			if e.R != nil {
				rep = *e.R
			}
			if rep < 0 {
				return nil, fmt.Errorf("直播类 MPD（r=-1）暂不支持")
			}
			for k := 0; k <= rep; k++ {
				emit(num, t)
				num++
				t += e.D
				if len(track.segURLs) > 200000 {
					return nil, fmt.Errorf("分片数过多，拒绝下载")
				}
			}
		}
	} else if tpl.Duration != nil && *tpl.Duration > 0 {
		// duration 写法：分片数 = ceil(period时长 / 单片时长)
		ts := 1
		if tpl.Timescale != nil && *tpl.Timescale > 0 {
			ts = *tpl.Timescale
		}
		periodSec := parseISODuration(p.Duration)
		if periodSec <= 0 {
			periodSec = parseISODuration(mpdDur)
		}
		if periodSec <= 0 {
			return nil, fmt.Errorf("该 MPD 既无 Timeline 也无可用时长，无法推算分片数")
		}
		count := int((periodSec*float64(ts) + float64(*tpl.Duration) - 1) / float64(*tpl.Duration))
		for i := 0; i < count; i++ {
			emit(start+i, int64(i)**tpl.Duration)
			if len(track.segURLs) > 200000 {
				return nil, fmt.Errorf("分片数过多，拒绝下载")
			}
		}
	} else {
		return nil, fmt.Errorf("该 MPD 既无 Timeline 也无 duration，暂不支持")
	}
	if len(track.segURLs) == 0 {
		return nil, fmt.Errorf("没有解析出分片")
	}
	return track, nil
}

// parseISODuration 解析 PTnHnMnS 子集，返回秒。
func parseISODuration(s string) float64 {
	m := isoDurRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	var total float64
	if m[1] != "" {
		h, _ := strconv.ParseFloat(m[1], 64)
		total += h * 3600
	}
	if m[2] != "" {
		min, _ := strconv.ParseFloat(m[2], 64)
		total += min * 60
	}
	if m[3] != "" {
		sec, _ := strconv.ParseFloat(m[3], 64)
		total += sec
	}
	return total
}

var isoDurRe = regexp.MustCompile(`^PT(?:([\d.]+)H)?(?:([\d.]+)M)?(?:([\d.]+)S)?$`)

// substitute 替换 $RepresentationID$/$Bandwidth$/$Number[%0Nd]$/$Time$。
func substitute(tpl string, vars map[string]string, num int, t int64) string {
	return templateVarRe.ReplaceAllStringFunc(tpl, func(m string) string {
		sub := templateVarRe.FindStringSubmatch(m)
		name, padFmt := sub[1], sub[2]
		switch name {
		case "RepresentationID":
			return vars["RepresentationID"]
		case "Bandwidth":
			return vars["Bandwidth"]
		case "Number":
			if padFmt != "" {
				return fmt.Sprintf("%"+padFmt[1:], num)
			}
			return strconv.Itoa(num)
		case "Time":
			return strconv.FormatInt(t, 10)
		}
		return m
	})
}

// ParseDashVariants MPD 的视频分档列表，URL 用 fragment 携带 rep id。
func ParseDashVariants(ctx context.Context, mediaURL string, h Headers) ([]Variant, error) {
	m, clean, err := fetchMPD(ctx, newClient(), mediaURL, h)
	if err != nil {
		return nil, err
	}
	p := &m.Periods[0]
	var out []Variant
	for _, a := range p.AdaptationSets {
		if !isVideoAdapt(&a) || len(a.Representations) == 0 {
			continue
		}
		for _, r := range a.Representations {
			res := ""
			if r.Width > 0 && r.Height > 0 {
				res = fmt.Sprintf("%dx%d", r.Width, r.Height)
			} else {
				res = "档位" + r.ID
			}
			label := res
			if r.Bandwidth > 0 {
				label = fmt.Sprintf("%s · %.1fMbps", res, float64(r.Bandwidth)/1e6)
			}
			out = append(out, Variant{
				URL: clean + "#videodl-rep=" + r.ID, Bandwidth: r.Bandwidth,
				Resolution: res, Codecs: r.Codecs, Label: label + "（DASH）",
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("MPD 里没有视频轨")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bandwidth > out[j].Bandwidth })
	return out, nil
}

// downloadDASH 下载并组装：init + 分片二进制拼接（fmp4 标准做法），音视频分离则混流。
func downloadDASH(ctx context.Context, opt Options, workDir string, on Progress) (string, error) {
	client := newClient()
	h := opt.headers()
	m, clean, err := fetchMPD(ctx, client, opt.MediaURL, h)
	if err != nil {
		return "", err
	}
	_, wantRep := splitFrag(opt.MediaURL)
	p := &m.Periods[0]
	base := mpdBase(clean, m, p)

	var video, audio *dashTrack
	for i := range p.AdaptationSets {
		a := &p.AdaptationSets[i]
		if isVideoAdapt(a) && video == nil && len(a.Representations) > 0 {
			rep := pickRep(a.Representations, wantRep)
			if rep == nil {
				return "", fmt.Errorf("指定的清晰度档位不存在: %s", wantRep)
			}
			if video, err = expandTrack(m.MediaPresentationDuration, base, p, a, rep, "video"); err != nil {
				return "", err
			}
		}
		if isAudioAdapt(a) && audio == nil && len(a.Representations) > 0 {
			rep := pickRep(a.Representations, "")
			if audio, err = expandTrack(m.MediaPresentationDuration, base, p, a, rep, "audio"); err != nil {
				return "", err
			}
		}
	}
	if video == nil {
		return "", fmt.Errorf("MPD 里没有视频轨")
	}
	total := len(video.segURLs)
	if audio != nil {
		total += len(audio.segURLs)
	}
	progress := 0
	emit := func(n int) {
		progress += n
		if on != nil {
			on(progress, total)
		}
	}

	// 续传凭证：音视频分片 URL 快照，对不上就清空重下。
	combined := make([]segment, 0, len(video.segURLs))
	for _, u := range video.segURLs {
		combined = append(combined, segment{URL: u})
	}
	if audio != nil {
		for _, u := range audio.segURLs {
			combined = append(combined, segment{URL: u})
		}
	}
	if !manifestMatch(workDir, opt.MediaURL, combined) {
		_ = os.RemoveAll(filepath.Join(workDir, "video"))
		_ = os.RemoveAll(filepath.Join(workDir, "audio"))
		_ = saveManifest(workDir, opt.MediaURL, combined)
	}

	videoFile, err := assembleTrack(ctx, client, opt, workDir, video, emit)
	if err != nil {
		return "", err
	}
	if audio == nil {
		out := outputPath(opt)
		if err := remux(ctx, videoFile, "", out); err != nil {
			return "", err
		}
		if on != nil {
			on(total, total)
		}
		return out, nil
	}
	audioFile, err := assembleTrack(ctx, client, opt, workDir, audio, emit)
	if err != nil {
		return "", err
	}
	out := outputPath(opt)
	if err := remux(ctx, videoFile, audioFile, out); err != nil {
		return "", err
	}
	if on != nil {
		on(total, total)
	}
	return out, nil
}

// assembleTrack 下 init + 全部分片，二进制按序拼成一个文件。
func assembleTrack(ctx context.Context, client *http.Client, opt Options, workDir string, track *dashTrack, emit func(int)) (string, error) {
	if emit == nil {
		emit = func(int) {}
	}
	segs := make([]segment, len(track.segURLs))
	for i, u := range track.segURLs {
		segs[i] = segment{URL: u}
	}
	// 复用 HLS 的并发下载 + 断点续传（key 为空即明文直存）。
	segDir := filepath.Join(workDir, track.kind)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return "", err
	}
	paths, err := fetchSegments(ctx, client, opt, segs, segDir, func(d, _ int) {
		// fetchSegments 的累计进度按整轨折算，这里只透传增量比较麻烦，
		// 简化：每次完成回调记 1。
		_ = d
	})
	if err != nil {
		return "", err
	}
	outFile := filepath.Join(workDir, track.kind+".mp4")
	f, err := os.Create(outFile)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if track.initURL != "" {
		initData, err := fetchWithRetry(ctx, client, track.initURL, opt.headers(), 3)
		if err != nil {
			return "", fmt.Errorf("init 分片下载失败: %w", err)
		}
		if _, err := f.Write(initData); err != nil {
			return "", err
		}
	}
	buf := make([]byte, 1<<20)
	for _, p := range paths {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		sf, err := os.Open(p)
		if err != nil {
			return "", err
		}
		for {
			n, rerr := sf.Read(buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					sf.Close()
					return "", werr
				}
			}
			if rerr != nil {
				break
			}
		}
		sf.Close()
		emit(1)
	}
	return outFile, nil
}

func remux(ctx context.Context, videoFile, audioFile, out string) error {
	ffmpeg, err := findFFmpeg()
	if err != nil {
		return err
	}
	args := []string{"-i", videoFile}
	if audioFile != "" {
		args = append(args, "-i", audioFile)
	}
	args = append(args, "-c", "copy", "-y", out)
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	if eb, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg 混流失败: %v\n%s", err, string(eb))
	}
	return nil
}
