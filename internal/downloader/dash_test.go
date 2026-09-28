package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

const dashTestMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT20S">
<Period>
<AdaptationSet mimeType="video/mp4" contentType="video">
<SegmentTemplate initialization="init-$RepresentationID$.mp4" media="seg-$RepresentationID$-$Number%03d$.m4s" startNumber="1">
<SegmentTimeline><S t="0" d="10" r="1"/></SegmentTimeline>
</SegmentTemplate>
<Representation id="v1" bandwidth="1000000" width="640" height="360"/>
<Representation id="v2" bandwidth="5000000" width="1280" height="720"/>
</AdaptationSet>
<AdaptationSet mimeType="audio/mp4" contentType="audio">
<SegmentTemplate initialization="ainit.mp4" media="a$Number$.m4s" startNumber="0">
<SegmentTimeline><S d="10" r="0"/></SegmentTimeline>
</SegmentTemplate>
<Representation id="a1" bandwidth="128000"/>
</AdaptationSet>
</Period>
</MPD>`

func dashTestServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/m.mpd", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, dashTestMPD) })
	mux.HandleFunc("/init-v2.mp4", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "INITV") })
	mux.HandleFunc("/seg-v2-001.m4s", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "V0") })
	mux.HandleFunc("/seg-v2-002.m4s", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "V1") })
	mux.HandleFunc("/ainit.mp4", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "INITA") })
	mux.HandleFunc("/a0.m4s", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "A0") })
	return httptest.NewServer(mux)
}

func TestDashParseVariants(t *testing.T) {
	srv := dashTestServer()
	defer srv.Close()
	vars, err := ParseDashVariants(context.Background(), srv.URL+"/m.mpd", Headers{UserAgent: "UT"})
	if err != nil || len(vars) != 2 {
		t.Fatalf("分档解析错误: %v %+v", err, vars)
	}
	if vars[0].Resolution != "1280x720" || vars[0].Bandwidth != 5000000 {
		t.Fatalf("默认应按码率降序，首档: %+v", vars[0])
	}
	clean, rep := splitFrag(vars[1].URL)
	if rep != "v1" || clean != srv.URL+"/m.mpd" {
		t.Fatalf("fragment 携带 rep 失败: %q %q", clean, rep)
	}
}

func TestDashExpandAssemble(t *testing.T) {
	srv := dashTestServer()
	defer srv.Close()
	ctx := context.Background()
	h := Headers{UserAgent: "UT"}
	m, clean, err := fetchMPD(ctx, newClient(), srv.URL+"/m.mpd", h)
	if err != nil {
		t.Fatal(err)
	}
	p := &m.Periods[0]
	base := mpdBase(clean, m, p)

	// 默认选最高码率 v2；Number %03d 补零 + Timeline r=1 展开 2 个分片
	rep := pickRep(p.AdaptationSets[0].Representations, "")
	if rep.ID != "v2" {
		t.Fatalf("默认分档应为 v2，实际 %s", rep.ID)
	}
	video, err := expandTrack("", base, p, &p.AdaptationSets[0], rep, "video")
	if err != nil {
		t.Fatal(err)
	}
	if video.initURL != srv.URL+"/init-v2.mp4" {
		t.Fatalf("init 地址不对: %s", video.initURL)
	}
	if len(video.segURLs) != 2 || video.segURLs[0] != srv.URL+"/seg-v2-001.m4s" || video.segURLs[1] != srv.URL+"/seg-v2-002.m4s" {
		t.Fatalf("分片展开不对: %v", video.segURLs)
	}
	// 指定低档
	repLow := pickRep(p.AdaptationSets[0].Representations, "v1")
	if repLow == nil || repLow.ID != "v1" {
		t.Fatal("指定 rep=v1 失败")
	}

	// 组装：init + 分片二进制拼接
	workDir := t.TempDir()
	opt := Options{Workers: 2}
	got, err := assembleTrack(ctx, newClient(), opt, workDir, video, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(got)
	if string(b) != "INITVV0V1" {
		t.Fatalf("组装内容不对: %q", b)
	}

	// 音频轨：startNumber=0，$Number$ 无补零
	audio, err := expandTrack("", base, p, &p.AdaptationSets[1], &p.AdaptationSets[1].Representations[0], "audio")
	if err != nil {
		t.Fatal(err)
	}
	if audio.initURL != srv.URL+"/ainit.mp4" || len(audio.segURLs) != 1 || audio.segURLs[0] != srv.URL+"/a0.m4s" {
		t.Fatalf("音频轨不对: %+v", audio)
	}
}
