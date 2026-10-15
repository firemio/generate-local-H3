// Package rtmp pushes the channel to a streaming platform (YouTube Live, Twitch, TikTok LIVE, ...) with the
// station's on-screen graphics burned in.
//
// The station's own output is the local HLS window plus a web page that draws the graphics (station bug,
// LIVE clock, programme title, ticker) on top of the video. A platform only takes RTMP(S) and shows the
// pixels it gets, so the pusher re-encodes the picture with the same graphics drawn by ffmpeg's drawtext:
//
//	┌──────────────────────────────────────────┐
//	│ ▪ EVO-X2 TV                 ● LIVE 09:31 │  top-left station bug (white on a dark box), top-right red dot + clock
//	│                                          │
//	│                                          │
//	│ 竹の囁き：光と影が織りなす深緑の物語       │  bottom-left: programme title (bold)
//	│ 光の粒子 · 黄金の光が舞う竹林 · 制作 9/17 09:36 │  subline: section · caption · 制作 <created>
//	│▮【自然ドキュメンタリー】竹の囁き… ◆ 【お天気】… │  red band, ticker scrolling right-to-left (the programme guide)
//	└──────────────────────────────────────────┘
//
// Sync: the graphics must change exactly when the clip on screen changes. The broadcaster records what it
// put on air in now.json (wall-clock time t, programme, caption, section, created). So the pusher does not
// let ffmpeg read the playlist: it follows the playlist itself (follower), works out for every segment
// when the broadcaster fed its first frame (from the segment file's mtime, or when it was listed — not
// from EXT-X-PROGRAM-DATE-TIME, which is a media-duration clock that drifts seconds away from the wall
// clock now.json is written with), looks up the now.json entry on air then, writes the overlay text files,
// and only then pipes the segment's bytes into ffmpeg's stdin. drawtext re-reads the text files every
// frame (reload=1). Segment and clip boundaries do not always coincide: the hls muxer cuts at the first
// keyframe after each 2 s of media and clips are 2 s GOPs, so a clip often starts inside a segment. The
// follower finds that keyframe in the segment's TS packets and writes the segment in two parts with the
// text change in between (ts.go). ffmpeg's decoder runs single-threaded (-threads 1): frame threading
// would hold ~17 frames back and burn the new text into the tail of the old clip. The few frames the
// demuxer, the parser and the decoder's reorder buffer still hold only come out as the new clip's first
// frames go in, so those go in before the text (flushOffset): measured against the station's segments,
// the title, subline and date then flip on the very frame the picture cuts, in landscape and portrait.
//
// Landscape output keeps the source size (832x480, 24 fps) — upscaling adds nothing. Portrait (TikTok)
// puts the 16:9 picture over a blurred, zoomed copy of itself in a 9:16 canvas (1080x1920 by default, 30 fps),
// with the graphics laid out for the phone: bug and clock in the top band, title, subline and ticker in
// the bottom band.
//
// The ticker scrolls with a fixed period (300 s of travel) whatever its text: drawtext's scroll is a
// function of the text width, and a text change (the 制作中 counter moves every few minutes) would
// re-phase it into a visible jump. Copies of the text are drawn one period-fraction apart so the band is
// never empty for long (overlayFilters).
//
// The stream key is never logged (the URL is shown with its last path element masked) — but it is on
// ffmpeg's command line, which any local process can read (Task Manager, Win32_Process); ffmpeg takes an
// output URL no other way. ffmpeg is relaunched whenever it exits (network drop, platform hiccup) and the
// follower resyncs to the live edge, also when the local broadcaster restarted its encoder (the media
// sequence jumps backwards).
package rtmp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/paths"
	"github.com/firemio/generate-local-H3/internal/programs"
	"github.com/firemio/generate-local-H3/internal/rundown"
)

// Options configure Push.
type Options struct {
	// URL is the ingest URL with the stream key (rtmp://a.rtmp.youtube.com/live2/<key>, rtmps://...). It
	// is passed to ffmpeg as an argument: visible in the process list to every local user, like any
	// command line.
	URL string
	// Source is the live playlist: an http(s) URL, a file path of live.m3u8 or its directory
	// ("" = paths.HLS() — the broadcaster's directory, read directly). Segments are resolved relative to it.
	Source string
	// NowFile is the broadcaster's on-air log ("" = <playlist dir>/now.json, or /now.json next to the URL).
	NowFile string
	// IndexFile is the library index the ticker is built from ("" = paths.Index()).
	IndexFile string
	// Overlay draws the graphics (default true when the zero Options is not used; see Push).
	NoOverlay bool
	// Font is the TrueType/TTC file for the graphics ("" = the first that exists of
	// C:\Windows\Fonts\YuGothB.ttc, meiryob.ttc, NotoSansJP-VF.ttf, msgothic.ttc).
	Font string
	// Portrait re-encodes into a 9:16 canvas (Width x Height, default 1080x1920) instead of the source size.
	Portrait bool
	Width    int
	Height   int
	// FPS (0 = source rate in landscape, 30 in portrait) and VideoKbps (0 = 2500 landscape / 4500 portrait).
	FPS       int
	VideoKbps int
	// RestartWait is the pause before relaunching ffmpeg (0 = 5 s).
	RestartWait time.Duration
	Log         logx.Logger
}

// OnAir is one now.json entry (the hls package writes them; duplicated here to keep rtmp independent).
type OnAir struct {
	T       float64 `json:"t"`
	Program string  `json:"program"`
	Caption string  `json:"caption"`
	Section string  `json:"section"`
	Format  string  `json:"format"`
	Seconds float64 `json:"seconds"`
	Created string  `json:"created"`
	ID      string  `json:"id"`
}

// Segment is one entry of the live playlist.
type Segment struct {
	Seq      int       // media sequence number
	URI      string    // as written in the playlist
	Duration float64   // EXTINF seconds
	Start    time.Time // EXT-X-PROGRAM-DATE-TIME (zero when absent)
}

// Defaults and tunables.
const (
	sourceFPS       = 24   // the library is 832x480 at 24 fps; landscape keeps it and needs it for the GOP
	portraitFPS     = 30   // TikTok wants 30
	portraitWidth   = 1080 // default 9:16 canvas
	portraitHeight  = 1920
	landscapeKbps   = 2500
	portraitKbps    = 4500
	defaultRestart  = 5 * time.Second
	pollInterval    = 500 * time.Millisecond // playlist poll
	nowPollHTTP     = 2 * time.Second        // now.json over HTTP (a file is stat'ed every poll)
	tickerInterval  = 30 * time.Second       // ticker.txt refresh
	startBehind     = 2                      // segments behind the live edge at (re)sync
	settleWait      = 500 * time.Millisecond // between the last segment write and an overlay text change
	stallTimeout    = 30 * time.Second       // a segment write that takes longer means ffmpeg is stuck: kill it
	stopWait        = 5 * time.Second        // grace after closing ffmpeg's stdin before it is killed
	tickerProgrames = 8                      // programmes in the ticker (the web player's slice(0, 8))
	playlistItems   = 200                    // /playlist.json carries the last 200 airable clips
	tickerPeriod    = 300 * time.Second      // the ticker's scroll period (fixed: a text change never re-phases it)
	tickerCopies    = 8                      // copies of the ticker text per period at most (drawtext instances - 1)
)

// ParsePlaylist reads a live media playlist: EXT-X-MEDIA-SEQUENCE numbers the first segment, each EXTINF
// (with an optional preceding EXT-X-PROGRAM-DATE-TIME) is followed by its URI. Unknown tags are skipped.
func ParsePlaylist(text string) []Segment {
	var segs []Segment
	seq := 0
	var dur float64
	var start time.Time
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))); err == nil {
				seq = n
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			v := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			dur, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			start = parseDateTime(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")))
		case strings.HasPrefix(line, "#"):
			// other tags and comments
		default:
			segs = append(segs, Segment{Seq: seq, URI: line, Duration: dur, Start: start})
			seq++
			dur = 0
			start = time.Time{}
		}
	}
	return segs
}

// parseDateTime reads an EXT-X-PROGRAM-DATE-TIME value: ffmpeg writes "2026-09-18T12:27:25.141+0900",
// the spec RFC 3339 ("+09:00", "Z"); a value without a zone is taken as local time. Zero when unreadable.
func parseDateTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// OnAirAt returns the entry on air at feed time t: the one with the greatest T <= t (nil when none). A
// clip's t is written before its first frame is fed, so an entry never precedes its own picture.
func OnAirAt(entries []OnAir, t time.Time) *OnAir {
	limit := unixSeconds(t)
	var best *OnAir
	for i := range entries {
		e := &entries[i]
		if e.T <= limit && (best == nil || e.T >= best.T) {
			best = e
		}
	}
	return best
}

// TickerText builds the programme guide shown in the red band from the library index, the same way the web
// player does (aitv/web/index.html refresh()): one entry per programme (grouped by programme_id, else by
// title), newest first, at most 8, commercials and idents left out, "【<format title>】<programme title>",
// the newest one marked "（制作中 n/N）" when it has fewer clips than segments_total; entries joined by
// "　　◆　　". Format titles come from programs.Formats.
//
// The web player reads /playlist.json, which is rundown.Airable(index) trimmed to its last 200 clips, so the
// same filter and window apply here (invented news without sources never shows in the guide). With no
// programme at all the station tagline is returned, as the page shows it.
func TickerText(indexFile string) (string, error) {
	if indexFile == "" {
		indexFile = paths.Index()
	}
	data, err := os.ReadFile(indexFile)
	if err != nil {
		return "", err
	}
	items := rundown.Airable(index.Parse(data))
	if len(items) > playlistItems {
		items = items[len(items)-playlistItems:]
	}
	type prog struct {
		title, format, created string
		n, total               int
	}
	byKey := map[string]*prog{}
	var progs []*prog
	for _, it := range items {
		if _, ok := programs.Formats[it.Format]; !ok || isBreak(it.Format) {
			continue
		}
		key := it.ProgrammeID
		if key == "" {
			key = it.Program
		}
		p := byKey[key]
		if p == nil {
			p = &prog{title: it.Program, format: it.Format, created: it.Created, total: it.SegmentsTotal}
			byKey[key] = p
			progs = append(progs, p)
		}
		p.n++
		if it.Created > p.created {
			p.created = it.Created
		}
	}
	sort.SliceStable(progs, func(i, j int) bool { return progs[i].created > progs[j].created })
	if len(progs) > tickerProgrames {
		progs = progs[:tickerProgrames]
	}
	parts := make([]string, 0, len(progs))
	for k, p := range progs {
		s := "【" + programs.Formats[p.format].Title + "】" + p.title
		if k == 0 && p.total > 0 && p.n < p.total {
			s += fmt.Sprintf("（制作中 %d/%d）", p.n, p.total)
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return programs.STATION.Tagline, nil
	}
	return strings.Join(parts, tickerSeparator), nil
}

func isBreak(format string) bool {
	for _, b := range programs.BreakFormats {
		if b == format {
			return true
		}
	}
	return false
}

// withDefaults fills the zero fields of opt (size, rate, bitrate, restart pause, logger). The font is
// resolved separately (Push), as Args must stay usable without a font on disk.
func withDefaults(opt Options) Options {
	if opt.Portrait {
		if opt.Width <= 0 {
			opt.Width = portraitWidth
		}
		if opt.Height <= 0 {
			opt.Height = portraitHeight
		}
		if opt.FPS <= 0 {
			opt.FPS = portraitFPS
		}
		if opt.VideoKbps <= 0 {
			opt.VideoKbps = portraitKbps
		}
	} else if opt.VideoKbps <= 0 {
		opt.VideoKbps = landscapeKbps
	}
	if opt.RestartWait <= 0 {
		opt.RestartWait = defaultRestart
	}
	if opt.Log == nil {
		opt.Log = logx.Printf
	}
	return opt
}

// Args is the ffmpeg command line for opt (input is MPEG-TS on stdin; exported for tests and the log).
// Landscape: the source size, x264 veryfast, tune zerolatency, keyframe every 2 s, audio copied.
// Portrait: split → blurred zoomed background + centred picture → overlays. Overlay filters use drawbox
// and drawtext (fontfile=<Font>, textfile=<dir>/title.txt|sub.txt|ticker.txt with reload=1, the ticker
// as tickerCopies+1 drawtext instances one period-fraction apart, the clock with %{localtime:%H\:%M});
// Windows paths in filter arguments use forward slashes with ':' escaped as '\:'.
//
// The input is probed for 1 s only (-analyzeduration/-probesize): with the defaults ffmpeg would sit on
// the first 5 s of the live pipe before starting, and the first title change could land on the wrong
// frames. The decoder is single-threaded (-threads 1 before -i): frame threading keeps ~17 frames in
// flight until more input arrives, so a text change written between two segments would land on the last
// 0.7 s of the previous one (measured); decoding 832x480 on one thread costs nothing noticeable. In
// landscape the source rate (24 fps) is kept unless FPS is set; -g is 2 s at the output rate.
func Args(opt Options, textDir string) []string {
	opt = withDefaults(opt)
	fps := opt.FPS
	if fps <= 0 {
		fps = sourceFPS
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats",
		"-fflags", "+genpts", "-analyzeduration", "1000000", "-probesize", "1000000",
		"-threads", "1", "-f", "mpegts", "-i", "pipe:0"}
	var overlays string
	if !opt.NoOverlay {
		overlays = overlayFilters(opt, textDir)
	}
	switch {
	case opt.Portrait:
		w, h := strconv.Itoa(opt.Width), strconv.Itoa(opt.Height)
		fc := "[0:v]split[bg][fg];" +
			"[bg]scale=" + w + ":" + h + ":force_original_aspect_ratio=increase,crop=" + w + ":" + h + ",gblur=sigma=30[bgb];" +
			"[fg]scale=" + w + ":-2[fgs];" +
			"[bgb][fgs]overlay=(W-w)/2:(H-h)/2"
		if overlays != "" {
			fc += "," + overlays
		}
		fc += "[v]"
		args = append(args, "-filter_complex", fc, "-map", "[v]", "-map", "0:a")
	case overlays != "":
		args = append(args, "-vf", overlays)
	}
	if opt.Portrait || opt.FPS > 0 {
		args = append(args, "-r", strconv.Itoa(fps))
	}
	gop := strconv.Itoa(2 * fps)
	kbps := strconv.Itoa(opt.VideoKbps)
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-g", gop, "-keyint_min", gop, "-sc_threshold", "0",
		"-b:v", kbps+"k", "-maxrate", kbps+"k", "-bufsize", strconv.Itoa(2*opt.VideoKbps)+"k",
		"-c:a", "copy",
		"-f", "flv", "-flvflags", "no_duration_filesize", opt.URL)
	return args
}

// Push runs the follower + ffmpeg until ctx is cancelled, relaunching ffmpeg RestartWait after every exit.
// Log lines: "[aitv] RTMP push -> <masked url> (<mode>)" at start, "[aitv] RTMP push ended after Ns (err);
// reconnecting in Ns" on exit, "[aitv] RTMP: playlist reset, resyncing" on a media-sequence jump.
//
// Setup failures return an error (no URL, an unusable Source, no font, an ffmpeg without drawtext, no
// ffmpeg at all); everything after that is retried forever. The overlay text files live in a temporary
// directory (os.MkdirTemp) that is removed on return, after the ticker goroutine (refreshing it every
// 30 s) has stopped.
func Push(ctx context.Context, opt Options) error {
	opt = withDefaults(opt)
	if strings.TrimSpace(opt.URL) == "" {
		return errors.New("rtmp: the ingest URL is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	log := opt.Log
	src, err := newSource(opt.Source, opt.NowFile)
	if err != nil {
		return err
	}
	need := []string{}
	if !opt.NoOverlay {
		opt.Font, err = resolveFont(opt.Font)
		if err != nil {
			return err
		}
		need = append(need, "drawtext", "drawbox")
	}
	if opt.Portrait {
		need = append(need, "split", "scale", "crop", "gblur", "overlay")
	}
	if err := checkFilters(need...); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "aitv-rtmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ov, err := newOverlay(dir, opt, log)
	if err != nil {
		return err
	}
	tickerDone := make(chan struct{})
	if opt.NoOverlay {
		close(tickerDone)
	} else {
		ov.refreshTicker()
		go func() {
			defer close(tickerDone)
			ov.tickerLoop(ctx)
		}()
	}
	defer func() { // before RemoveAll: a write in flight would keep ticker.txt (and the dir) alive
		cancel()
		<-tickerDone
	}()

	mode := "landscape"
	if opt.Portrait {
		mode = fmt.Sprintf("portrait %dx%d", opt.Width, opt.Height)
	}
	if opt.NoOverlay {
		mode += ", no overlay"
	}
	log("[aitv] RTMP push -> %s (%s)", Masked(opt.URL), mode)
	args := Args(opt, dir)
	log("[aitv] RTMP: ffmpeg %s", strings.Join(args[:len(args)-1], " ")+" "+Masked(opt.URL))
	f := &follower{src: src, overlay: ov, log: log}
	for ctx.Err() == nil {
		started := time.Now()
		reason := runFFmpeg(ctx, args, opt.URL, f)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(reason, exec.ErrNotFound) {
			return reason
		}
		log("[aitv] RTMP push ended after %ds (%v); reconnecting in %ds",
			int(time.Since(started).Seconds()), reason, int(opt.RestartWait.Seconds()))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(opt.RestartWait):
		}
	}
	return nil
}

// Masked hides the stream key: everything after the last "/" becomes "****".
func Masked(url string) string {
	i := strings.LastIndexByte(url, '/')
	return url[:i+1] + "****"
}
