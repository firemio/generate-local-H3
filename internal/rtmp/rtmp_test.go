package rtmp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A playlist as the broadcaster writes it (ffmpeg hls muxer, program_date_time, +0900), with a few tags
// the follower must skip.
const livePlaylist = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:46524
#EXT-X-DISCONTINUITY-SEQUENCE:0
#EXTINF:2.000000,
#EXT-X-PROGRAM-DATE-TIME:2026-09-18T12:27:25.141+0900
seg046524.ts
#EXTINF:1.416667,
#EXT-X-PROGRAM-DATE-TIME:2026-09-18T12:27:27.141+0900
seg046525.ts
#EXT-X-DISCONTINUITY
#EXTINF:2.000000,
#EXT-X-PROGRAM-DATE-TIME:2026-09-18T12:27:28.557+0900
seg046526.ts
#EXT-X-SOMETHING-NEW:1
#EXTINF:2,
seg046527.ts
`

func TestParsePlaylist(t *testing.T) {
	segs := ParsePlaylist(livePlaylist)
	if len(segs) != 4 {
		t.Fatalf("got %d segments: %+v", len(segs), segs)
	}
	jst := time.FixedZone("JST", 9*3600)
	want := []Segment{
		{Seq: 46524, URI: "seg046524.ts", Duration: 2, Start: time.Date(2026, 9, 18, 12, 27, 25, 141e6, jst)},
		{Seq: 46525, URI: "seg046525.ts", Duration: 1.416667, Start: time.Date(2026, 9, 18, 12, 27, 27, 141e6, jst)},
		{Seq: 46526, URI: "seg046526.ts", Duration: 2, Start: time.Date(2026, 9, 18, 12, 27, 28, 557e6, jst)},
		{Seq: 46527, URI: "seg046527.ts", Duration: 2},
	}
	for i, w := range want {
		g := segs[i]
		if g.Seq != w.Seq || g.URI != w.URI || g.Duration != w.Duration || !g.Start.Equal(w.Start) {
			t.Errorf("segment %d: got %+v want %+v", i, g, w)
		}
	}
	if !segs[3].Start.IsZero() {
		t.Errorf("segment without PROGRAM-DATE-TIME must have a zero Start, got %v", segs[3].Start)
	}
	// The wall-clock instant is right regardless of the zone spelling.
	if got := segs[0].Start.UTC().Format(time.RFC3339Nano); got != "2026-09-18T03:27:25.141Z" {
		t.Errorf("start in UTC = %s", got)
	}
	if segs := ParsePlaylist(strings.ReplaceAll(livePlaylist, "+0900", "+09:00")); !segs[0].Start.Equal(want[0].Start) {
		t.Errorf("RFC 3339 zone: %v", segs[0].Start)
	}
	if segs := ParsePlaylist("#EXTM3U\n#EXT-X-VERSION:3\n"); len(segs) != 0 {
		t.Errorf("empty playlist gave %+v", segs)
	}
	// No MEDIA-SEQUENCE: numbering starts at 0.
	if segs := ParsePlaylist("#EXTM3U\n#EXTINF:2.0,\na.ts\n#EXTINF:2.0,\nb.ts\n"); len(segs) != 2 || segs[1].Seq != 1 {
		t.Errorf("no media sequence: %+v", segs)
	}
}

func TestOnAirAt(t *testing.T) {
	entries := []OnAir{{T: 100, ID: "a"}, {T: 110, ID: "b"}, {T: 120, ID: "c"}}
	at := func(sec float64) time.Time { return time.UnixMicro(int64(sec * 1e6)) }
	cases := []struct {
		t    float64
		want string
	}{
		{99.9, ""}, // nothing on air yet
		{100, "a"}, // exactly on the line
		{109.9, "a"},
		{110, "b"},
		{115, "b"},
		{119.9, "b"},
		{120, "c"},
		{1e6, "c"},
	}
	for _, c := range cases {
		got := OnAirAt(entries, at(c.t))
		id := ""
		if got != nil {
			id = got.ID
		}
		if id != c.want {
			t.Errorf("OnAirAt(%v) = %q, want %q", c.t, id, c.want)
		}
	}
	if OnAirAt(nil, at(100)) != nil {
		t.Error("no entries must give nil")
	}
	// Unsorted input: still the greatest T that qualifies.
	if got := OnAirAt([]OnAir{{T: 120, ID: "c"}, {T: 100, ID: "a"}, {T: 110, ID: "b"}}, at(115)); got == nil || got.ID != "b" {
		t.Errorf("unsorted: %+v", got)
	}
}

// testdata/index.json is 40 entries of the real library: an old invented-news clip (no sources), the
// tail of a news programme, an ident, the tail of a weather programme, a commercial, the tail of a
// variety programme, an ident, the tail of a music show, a commercial, the head of a nature documentary,
// an ident, and 19 of the 39 clips of a cooking programme still in production.
func TestTickerText(t *testing.T) {
	got, err := TickerText(filepath.Join("testdata", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := "【3分クッキング】彩りたっぷり！簡単グリーンサラダ🥗（制作中 19/39）" +
		"　　◆　　【自然ドキュメンタリー】星影の詩：砂漠の夜に目覚める生命の記憶" +
		"　　◆　　【ミュージックステージ】ネオン・リゾナンス ～三つのリズムの交差点～" +
		"　　◆　　【バラエティー】輝く日常の発見！現代日本のトリセリアル" +
		"　　◆　　【お天気】9月17日(木) お天気" +
		"　　◆　　【ニュース】9月17日(木) 夕方のニュース"
	if got != want {
		t.Errorf("ticker\n got %s\nwant %s", got, want)
	}
	if _, err := TickerText(filepath.Join("testdata", "missing.json")); err == nil {
		t.Error("a missing index must be an error")
	}
	empty := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(empty, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := TickerText(empty); err != nil || got != "ローカルAIが24時間つくり続ける放送局" {
		t.Errorf("empty index: %q, %v", got, err)
	}
}

func TestArgs(t *testing.T) {
	font := `C:\Windows\Fonts\YuGothB.ttc`
	dir := `C:\Users\u\AppData\Local\Temp\aitv-rtmp-1`
	if runtime.GOOS != "windows" {
		font, dir = "/usr/share/fonts/YuGothB.ttc", "/tmp/aitv-rtmp-1"
	}
	url := "rtmp://a.rtmp.youtube.com/live2/abcd-1234"
	has := func(args []string, seq ...string) bool {
		for i := 0; i+len(seq) <= len(args); i++ {
			if slices.Equal(args[i:i+len(seq)], seq) {
				return true
			}
		}
		return false
	}
	find := func(args []string, flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}

	t.Run("landscape", func(t *testing.T) {
		args := Args(Options{URL: url, Font: font}, dir)
		for _, seq := range [][]string{
			{"-fflags", "+genpts"}, {"-threads", "1", "-f", "mpegts", "-i", "pipe:0"},
			{"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "high", "-pix_fmt", "yuv420p"},
			{"-g", "48", "-keyint_min", "48", "-sc_threshold", "0"},
			{"-b:v", "2500k", "-maxrate", "2500k", "-bufsize", "5000k"},
			{"-c:a", "copy"}, {"-f", "flv", "-flvflags", "no_duration_filesize", url},
		} {
			if !has(args, seq...) {
				t.Errorf("missing %q in %q", seq, args)
			}
		}
		if args[len(args)-1] != url {
			t.Errorf("the URL must be last: %q", args)
		}
		if slices.Contains(args, "-filter_complex") || slices.Contains(args, "-r") {
			t.Errorf("landscape must not scale or force a rate: %q", args)
		}
		vf := find(args, "-vf")
		if vf == "" {
			t.Fatalf("no -vf in %q", args)
		}
		if runtime.GOOS == "windows" {
			for _, want := range []string{
				`fontfile='C\:/Windows/Fonts/YuGothB.ttc'`,
				`textfile='C\:/Users/u/AppData/Local/Temp/aitv-rtmp-1/title.txt':reload=1:expansion=none`,
				`textfile='C\:/Users/u/AppData/Local/Temp/aitv-rtmp-1/sub.txt':reload=1`,
				`textfile='C\:/Users/u/AppData/Local/Temp/aitv-rtmp-1/ticker.txt':reload=1`,
			} {
				if !strings.Contains(vf, want) {
					t.Errorf("-vf lacks %s:\n%s", want, vf)
				}
			}
			if strings.Contains(vf, `\`+`Users`) || strings.Contains(vf, "C:/") {
				t.Errorf("unescaped Windows path in %s", vf)
			}
		}
		for _, want := range []string{
			"drawtext=", "text='EVO-X2 TV'", "boxcolor=black@0.75", "fontsize=20",
			`text='LIVE %{localtime\:%H\\\:%M}'`, "fontsize=16", "text='●'", "fontcolor=0xee3333",
			"fontsize=28", "shadowcolor=black@0.6:shadowx=2:shadowy=2", "fontcolor=white@0.85:fontsize=15",
			"drawbox=x=0:y=ih-40:w=iw:h=40:color=0xb3001b:t=fill",
			// the ticker: a fixed 300 s period (27000 px at 90 px/s); copy 0, then copies parked off-screen
			// beyond floor(P/tw) of them
			`fontsize=22:x=-mod(t*90\,27000):y=h-40+(40-th)/2`,
			`fontsize=22:x=if(lt(1\,max(1\,min(8\,floor(27000/tw)))+1)\,1*27000/max(1\,min(8\,floor(27000/tw)))-mod(t*90\,27000)\,-tw-w):y=h-40+(40-th)/2`,
			`fontsize=22:x=if(lt(8\,max(1\,min(8\,floor(27000/tw)))+1)\,8*27000/max(1\,min(8\,floor(27000/tw)))-mod(t*90\,27000)\,-tw-w):y=h-40+(40-th)/2`,
		} {
			if !strings.Contains(vf, want) {
				t.Errorf("-vf lacks %s:\n%s", want, vf)
			}
		}
		if strings.Count(vf, "drawtext=") != 5+tickerCopies+1 || strings.Count(vf, "drawbox=") != 1 {
			t.Errorf("expected %d drawtext + 1 drawbox:\n%s", 5+tickerCopies+1, vf)
		}
		if strings.Contains(vf, "w+tw") {
			t.Errorf("the ticker's period must not depend on the text width:\n%s", vf)
		}
		if strings.Contains(vf, "expansion=none:fontcolor=white:fontsize=16") {
			t.Errorf("the clock needs expansion:\n%s", vf)
		}
	})

	t.Run("landscape rate override", func(t *testing.T) {
		args := Args(Options{URL: url, Font: font, FPS: 30, VideoKbps: 3000}, dir)
		if !has(args, "-r", "30") || !has(args, "-g", "60", "-keyint_min", "60") || !has(args, "-b:v", "3000k", "-maxrate", "3000k", "-bufsize", "6000k") {
			t.Errorf("%q", args)
		}
	})

	t.Run("portrait", func(t *testing.T) {
		args := Args(Options{URL: url, Font: font, Portrait: true}, dir)
		fc := find(args, "-filter_complex")
		if fc == "" {
			t.Fatalf("no -filter_complex in %q", args)
		}
		for _, want := range []string{
			"[0:v]split[bg][fg];",
			"[bg]scale=1080:1920:force_original_aspect_ratio=increase,crop=1080:1920,gblur=sigma=30[bgb];",
			"[fg]scale=1080:-2[fgs];",
			"[bgb][fgs]overlay=(W-w)/2:(H-h)/2,drawtext=",
			"fontsize=45", "fontsize=36", "fontsize=63", "fontsize=34",
			"drawbox=x=0:y=ih-90:w=iw:h=90:color=0xb3001b:t=fill",
			`fontsize=50:x=-mod(t*203\,60900):y=h-90+(90-th)/2`,
			`-mod(t*203\,60900)\,-tw-w):y=h-90+(90-th)/2[v]`,
		} {
			if !strings.Contains(fc, want) {
				t.Errorf("-filter_complex lacks %s:\n%s", want, fc)
			}
		}
		if !has(args, "-map", "[v]", "-map", "0:a") || !has(args, "-r", "30") || !has(args, "-g", "60", "-keyint_min", "60") ||
			!has(args, "-b:v", "4500k", "-maxrate", "4500k", "-bufsize", "9000k") || !has(args, "-c:a", "copy") {
			t.Errorf("%q", args)
		}
		if slices.Contains(args, "-vf") {
			t.Errorf("portrait uses -filter_complex only: %q", args)
		}
	})

	t.Run("no overlay", func(t *testing.T) {
		args := Args(Options{URL: url, NoOverlay: true}, dir)
		if slices.Contains(args, "-vf") || slices.Contains(args, "-filter_complex") {
			t.Errorf("no filters expected: %q", args)
		}
		if !has(args, "-c:v", "libx264") || !has(args, "-c:a", "copy") {
			t.Errorf("still an encode: %q", args)
		}
		args = Args(Options{URL: url, NoOverlay: true, Portrait: true}, dir)
		fc := find(args, "-filter_complex")
		if !strings.HasSuffix(fc, "overlay=(W-w)/2:(H-h)/2[v]") || strings.Contains(fc, "drawtext") {
			t.Errorf("portrait without overlay: %s", fc)
		}
	})
}

func TestFilterArg(t *testing.T) {
	cases := map[string]string{
		"EVO-X2 TV":        "'EVO-X2 TV'",
		"a:b":              `'a\:b'`,
		"it's":             `'it'\\\''s'`,
		"C:/x/y":           `'C\:/x/y'`,
		"comma, semi; [x]": "'comma, semi; [x]'",
	}
	for in, want := range cases {
		if got := filterArg(in); got != want {
			t.Errorf("filterArg(%q) = %s, want %s", in, got, want)
		}
	}
	if runtime.GOOS == "windows" {
		if got := filterPath(`D:\a b\c.ttf`); got != `'D\:/a b/c.ttf'` {
			t.Errorf("filterPath = %s", got)
		}
	}
}

func TestMasked(t *testing.T) {
	cases := map[string]string{
		"rtmp://a.rtmp.youtube.com/live2/abcd-efgh-1234": "rtmp://a.rtmp.youtube.com/live2/****",
		"rtmps://live.twitch.tv/app/live_1?bw=1":         "rtmps://live.twitch.tv/app/****",
		"rtmp://host/app/":                               "rtmp://host/app/****",
		"nokey":                                          "****",
		"":                                               "****",
	}
	for in, want := range cases {
		if got := Masked(in); got != want {
			t.Errorf("Masked(%q) = %q, want %q", in, got, want)
		}
	}
	// ffmpeg's own messages are masked too (the URL, and a key of 8+ characters on its own).
	var out strings.Builder
	m := &maskWriter{w: &out, url: "rtmp://h/app/secretkey1", masked: "rtmp://h/app/****", secret: "secretkey1"}
	_, _ = m.Write([]byte("Error opening output rtmp://h/app/secretkey1: refused\npartial secretkey1"))
	m.flush()
	if got := out.String(); strings.Contains(got, "secretkey1") || !strings.Contains(got, "rtmp://h/app/****: refused") || !strings.HasSuffix(got, "partial ****\n") {
		t.Errorf("masked stderr: %q", got)
	}
	if secretOf("rtmp://127.0.0.1:19350/live/t") != "" || secretOf("rtmp://h/a/abcdefgh?x=1") != "abcdefgh" {
		t.Error("secretOf")
	}
}

func TestSubLineAndWrap(t *testing.T) {
	e := &OnAir{Program: "竹の囁き", Section: "光の粒子", Caption: "黄金の光が舞う竹林", Created: "2026-09-17T09:36:12"}
	if got := subLine(e); got != "光の粒子 · 黄金の光が舞う竹林 · 制作 9/17 09:36" {
		t.Errorf("subLine = %q", got)
	}
	if got := subLine(&OnAir{Caption: "cap", Created: "bad"}); got != "cap" {
		t.Errorf("subLine without section/created = %q", got)
	}
	if got := subLine(&OnAir{Section: "s", Created: "2026-12-05T00:07:00"}); got != "s · 制作 12/5 00:07" {
		t.Errorf("subLine = %q", got)
	}
	if got := stripEmoji("彩りたっぷり！簡単グリーンサラダ🥗 ☔☀ 🌃ネオン❤️"); got != "彩りたっぷり！簡単グリーンサラダ ☀ ネオン❤" {
		t.Errorf("stripEmoji = %q", got)
	}
	if got := wrap("短い", 10, 2); got != "短い" {
		t.Errorf("wrap short = %q", got)
	}
	if got := wrap("竹の囁き：光と影が織りなす深緑の物語", 12, 2); got != "竹の囁き：\n光と影が織りなす深緑の物語" {
		t.Errorf("wrap at punctuation = %q", got)
	}
	if got := wrap("光の粒子 · 黄金の光が舞う竹林 · 制作 9/17 09:36", 24, 2); got != "光の粒子 · 黄金の光が舞う竹林\n制作 9/17 09:36" {
		t.Errorf("wrap at separator = %q", got)
	}
	if got := wrap("あいうえおかきくけこさしすせそ", 6, 2); got != "あいうえおか\nきくけこさしすせそ" {
		t.Errorf("hard wrap = %q", got)
	}
	long := "週末と今後の天候変化 · 20日は降水確率が大幅に高まる点にお気をつけください · 制作 9/18 00:48"
	if got := wrap(long, 28, 2); got != "週末と今後の天候変化\n20日は降水確率が大幅に高まる点にお気をつけください · 制作 9/18 00:48" {
		t.Errorf("two lines = %q", got)
	}
	if got := wrap(long, 28, 3); got != "週末と今後の天候変化\n20日は降水確率が大幅に高まる点にお気をつけください\n制作 9/18 00:48" {
		t.Errorf("three lines = %q", got)
	}
	if got := wrap(long, 28, 1); got != long {
		t.Errorf("one line = %q", got)
	}
}

// Sources: a directory, a playlist path, an http URL, a bare http directory.
func TestNewSource(t *testing.T) {
	dir := t.TempDir()
	s, err := newSource(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	fs := s.(*fileSource)
	if fs.file != filepath.Join(dir, "live.m3u8") || fs.nowPath != filepath.Join(dir, "now.json") {
		t.Errorf("dir source: %+v", fs)
	}
	s, _ = newSource(filepath.Join(dir, "live.m3u8"), filepath.Join(dir, "other.json"))
	fs = s.(*fileSource)
	if fs.dir != dir || fs.nowPath != filepath.Join(dir, "other.json") {
		t.Errorf("file source: %+v", fs)
	}
	s, _ = newSource("http://127.0.0.1:8800/hls/live.m3u8", "")
	hs := s.(*httpSource)
	if hs.nowURL.String() != "http://127.0.0.1:8800/hls/now.json" {
		t.Errorf("now URL = %s", hs.nowURL)
	}
	s, _ = newSource("http://127.0.0.1:8800/hls/", "")
	if s.String() != "http://127.0.0.1:8800/hls/live.m3u8" {
		t.Errorf("http dir source = %s", s)
	}
}

// logBuf collects log lines from Push.
type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) f(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logBuf) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestPush runs the whole thing against a local RTMP listener: a synthetic two-segment HLS source (test
// pattern + sine) with a hand-written now.json, Push for ~8 s, then the received stream is probed.
func TestPush(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	if _, err := resolveFont(""); err != nil {
		t.Skip(err)
	}
	if err := checkFilters("drawtext", "gblur"); err != nil {
		t.Skip(err)
	}
	for _, portrait := range []bool{false, true} {
		name := "landscape"
		if portrait {
			name = "portrait"
		}
		t.Run(name, func(t *testing.T) { pushOnce(t, portrait) })
	}
}

func pushOnce(t *testing.T, portrait bool) {
	root := t.TempDir()
	src := filepath.Join(root, "hls")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// The overlay's temp dir must land under root so its removal can be checked.
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(v, tmp)
	}

	// 4 s of test pattern + tone, keyframes every 2 s, as a two-segment HLS with programme date-time.
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=832x480:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0",
		"-c:a", "aac", "-ac", "2", "-b:a", "96k",
		"-f", "hls", "-hls_time", "2", "-hls_list_size", "0", "-hls_flags", "program_date_time",
		"-hls_segment_filename", filepath.Join(src, "seg%06d.ts"), filepath.Join(src, "live.m3u8"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("source generation: %v\n%s", err, out)
	}
	text, err := os.ReadFile(filepath.Join(src, "live.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	segs := ParsePlaylist(string(text))
	if len(segs) < 2 || segs[0].Start.IsZero() {
		t.Fatalf("unexpected synthetic playlist: %+v\n%s", segs, text)
	}
	// The broadcaster's encoder is -re paced, so a segment file is finished (mtime) when its last frame's
	// time comes; the follower dates segments by that. The offline generation above wrote both files at
	// once, so stamp them as the live encoder would have.
	for _, s := range segs {
		end := s.Start.Add(time.Duration(s.Duration * float64(time.Second)))
		if err := os.Chtimes(filepath.Join(src, s.URI), end, end); err != nil {
			t.Fatal(err)
		}
	}
	// now.json: the first clip on air from the first segment, a second one from the second-to-last.
	sec := func(tm time.Time) float64 { return float64(tm.UnixMicro()) / 1e6 }
	onAir := []OnAir{
		{T: sec(segs[0].Start) - 0.3, Program: "テストパターン：第一部", Caption: "カラーバー", Section: "オープニング", Format: "nature", Seconds: 2, Created: "2026-09-18T10:00:00", ID: "t-00"},
		{T: sec(segs[len(segs)-1].Start) - 0.3, Program: "テストパターン：第二部🥗", Caption: "サイン波", Section: "エンディング", Format: "nature", Seconds: 2, Created: "2026-09-18T10:05:00", ID: "t-01"},
	}
	nowData, _ := json.Marshal(onAir)
	if err := os.WriteFile(filepath.Join(src, "now.json"), nowData, 0o644); err != nil {
		t.Fatal(err)
	}

	// The local ingest: one connection, stream copy into out.flv, at most 8 s of it.
	const url = "rtmp://127.0.0.1:19350/live/t"
	outFile := filepath.Join(root, "out.flv")
	lctx, lcancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer lcancel()
	listener := exec.CommandContext(lctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-listen", "1", "-i", url, "-t", "8", "-c", "copy", outFile)
	var lout strings.Builder
	listener.Stdout, listener.Stderr = &lout, &lout
	if err := listener.Start(); err != nil {
		t.Fatal(err)
	}
	listenerDone := make(chan error, 1)
	go func() { listenerDone <- listener.Wait() }()
	time.Sleep(1500 * time.Millisecond)

	var lb logBuf
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// Midway, the overlay must show the second clip (its title.txt lives in the per-run temp dir).
	titles := make(chan string, 1)
	go func() {
		time.Sleep(5 * time.Second)
		matches, _ := filepath.Glob(filepath.Join(tmp, "aitv-rtmp-*", titleFile))
		title := ""
		for _, m := range matches {
			b, _ := os.ReadFile(m)
			title += string(b)
		}
		titles <- title
	}()
	err = Push(ctx, Options{URL: url, Source: src, IndexFile: filepath.Join("testdata", "index.json"),
		Portrait: portrait, RestartWait: time.Second, Log: lb.f})
	for _, line := range lb.all() {
		t.Log(line)
	}
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if title := <-titles; strings.TrimSpace(title) != "テストパターン：第二部" {
		t.Errorf("title.txt mid-run = %q, want the second clip (emoji stripped)", title)
	}
	select {
	case err := <-listenerDone:
		if err != nil {
			t.Logf("listener: %v\n%s", err, lout.String())
		}
	case <-time.After(15 * time.Second):
		_ = listener.Process.Kill()
		<-listenerDone
		t.Logf("listener killed\n%s", lout.String())
	}
	if lines := lb.all(); len(lines) == 0 || !strings.HasPrefix(lines[0], "[aitv] RTMP push -> rtmp://127.0.0.1:19350/live/**** (") {
		t.Errorf("start line: %q", lines)
	}
	for _, line := range lb.all() {
		if strings.Contains(line, "/live/t") {
			t.Errorf("stream key in a log line: %s", line)
		}
	}

	// The received stream: h264 video of the right size + aac audio.
	probe := exec.Command("ffprobe", "-v", "error", "-show_streams", "-of", "json", outFile)
	pout, err := probe.Output()
	if err != nil {
		t.Fatalf("ffprobe out.flv: %v (listener output:\n%s)", err, lout.String())
	}
	var doc struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(pout, &doc); err != nil {
		t.Fatalf("%v: %s", err, pout)
	}
	wantW, wantH := 832, 480
	if portrait {
		wantW, wantH = 1080, 1920
	}
	video, audio := false, false
	for _, s := range doc.Streams {
		switch s.CodecType {
		case "video":
			video = true
			if s.CodecName != "h264" || s.Width != wantW || s.Height != wantH {
				t.Errorf("video stream %+v, want h264 %dx%d", s, wantW, wantH)
			}
		case "audio":
			audio = true
			if s.CodecName != "aac" {
				t.Errorf("audio stream %+v, want aac", s)
			}
		}
	}
	if !video || !audio {
		t.Errorf("out.flv streams: %s", pout)
	}
	// The temp dir with the text files is gone.
	if left, _ := filepath.Glob(filepath.Join(tmp, "aitv-rtmp-*")); len(left) != 0 {
		t.Errorf("temp dir left behind: %v", left)
	}
}
