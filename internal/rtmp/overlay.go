package rtmp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/programs"
)

// Overlay text files (in the per-run temp dir) read by drawtext with reload=1.
const (
	titleFile  = "title.txt"
	subFile    = "sub.txt"
	tickerFile = "ticker.txt"
)

// Colours of the web player (aitv/web/index.html): the red band, the LIVE dot.
const (
	bandColor = "0xb3001b"
	dotColor  = "0xee3333"
)

// defaultFonts are tried in order under %SystemRoot%\Fonts when Options.Font is empty: Yu Gothic Bold,
// Meiryo Bold, Noto Sans JP, MS Gothic (bitmap-strike font; drawtext gets ft_load_flags=no_bitmap so it
// works too).
var defaultFonts = []string{"YuGothB.ttc", "meiryob.ttc", "NotoSansJP-VF.ttf", "msgothic.ttc"}

// resolveFont returns font when given (it must exist), else the first default that exists.
func resolveFont(font string) (string, error) {
	if font != "" {
		if _, err := os.Stat(font); err != nil {
			return "", fmt.Errorf("rtmp: font %s: %w", font, err)
		}
		return font, nil
	}
	dir := filepath.Join(windowsDir(), "Fonts")
	for _, name := range defaultFonts {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("rtmp: no Japanese font found in %s (%s); pass --font", dir, strings.Join(defaultFonts, ", "))
}

func windowsDir() string {
	if d := os.Getenv("SystemRoot"); d != "" {
		return d
	}
	if d := os.Getenv("WINDIR"); d != "" {
		return d
	}
	return `C:\Windows`
}

// filterArg quotes a filter option value: single-quoted at the filtergraph level (so ',' ';' '[' ']' are
// literal there) with the colon and the backslash escaped for the option parser inside; a single quote is
// written as: close the quotes, a backslash-escaped quote, reopen. Windows paths go through filterPath.
func filterArg(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case ':':
			b.WriteString(`\:`)
		case '\'':
			b.WriteString(`'\\\''`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// filterPath is filterArg for a file path: forward slashes, drive colon escaped ("C\:/Windows/...").
func filterPath(p string) string {
	return filterArg(filepath.ToSlash(p))
}

// layout holds the pixel sizes of the graphics for one output size: the web player's look at 832x480
// (bug 20 px, clock 16, title 28, sub 15, band 40 with the ticker at 22, ~90 px/s) scaled with the short
// side of the canvas (×2.25 for 1080x1920).
type layout struct {
	w, h   int
	margin int // side margin (22 px at 480)
	top    int // top of the bug's text
	bug    int // station bug font size
	bugPad int // box padding around the bug
	clock  int // clock font size
	clockW int // width reserved for the clock (right-aligned inside it)
	dot    int // red dot font size
	title  int
	sub    int
	band   int // red band height
	ticker int
	speed  int // ticker px/s
	shadow int
	// subLines is how many lines the subline may take (space for them is reserved above the band): 2 in
	// landscape, 3 in portrait, where a section · caption · date line rarely fits in two.
	subLines int
}

func newLayout(opt Options) layout {
	w, h := opt.Width, opt.Height
	if !opt.Portrait || w <= 0 || h <= 0 {
		w, h = 832, 480
	}
	s := float64(min(w, h)) / 480
	px := func(v float64) int { return int(math.Round(v * s)) }
	l := layout{w: w, h: h, margin: px(22), top: px(18), bug: px(20), bugPad: px(6), clock: px(16),
		dot: px(14), title: px(28), sub: px(15), band: px(40), ticker: px(22), speed: px(90), shadow: max(1, px(2)),
		subLines: 2}
	l.clockW = int(math.Round(5.3 * float64(l.clock))) // "LIVE 09:31" is ~5.2 em in Yu Gothic
	if opt.Portrait {
		l.top = int(math.Round(60 * float64(w) / 1080)) // in the blurred top band, clear of the phone's status bar
		l.subLines = 3
	}
	return l
}

// titleRunes / subRunes are how many characters fit on one line (CJK glyphs are ~1 em wide).
func (l layout) titleRunes() int { return max(8, (l.w-2*l.margin)/l.title) }
func (l layout) subRunes() int   { return max(8, (l.w-2*l.margin)/l.sub) }

// subTop is where the subline starts: subLines sub lines (1.4 em each) plus a gap above the band.
func (l layout) subTop() int {
	return l.h - l.band - int(math.Round(0.8*float64(l.sub))) - l.subLines*l.subLineH()
}
func (l layout) subLineH() int { return int(math.Round(1.4 * float64(l.sub))) }

// overlayFilters is the drawbox/drawtext chain (comma-joined, no labels) for opt with the text files in dir.
func overlayFilters(opt Options, dir string) string {
	l := newLayout(opt)
	font := "fontfile=" + filterPath(opt.Font) + ":ft_load_flags=no_bitmap"
	text := func(name string) string {
		return "textfile=" + filterPath(filepath.Join(dir, name)) + ":reload=1:expansion=none"
	}
	shadow := fmt.Sprintf("shadowcolor=black@0.6:shadowx=%d:shadowy=%d", l.shadow, l.shadow)
	parts := []string{
		// station bug: white bold on a dark box
		"drawtext=" + font + ":text=" + filterArg(programs.STATION.Name) + ":expansion=none:fontcolor=white" +
			fmt.Sprintf(":fontsize=%d:x=%d:y=%d:box=1:boxcolor=black@0.75:boxborderw=%d", l.bug, l.margin, l.top, l.bugPad),
		// clock, right-aligned inside a fixed box at the right margin (so the dot can sit at a fixed x)
		"drawtext=" + font + `:text='LIVE %{localtime\:%H\\\:%M}':fontcolor=white` +
			fmt.Sprintf(":fontsize=%d:x=w-%d-%d:boxw=%d:text_align=R:y=%d", l.clock, l.margin, l.clockW, l.clockW, l.top+(l.bug-l.clock)/2),
		// the red dot left of the clock
		"drawtext=" + font + ":text='●':expansion=none:fontcolor=" + dotColor +
			fmt.Sprintf(":fontsize=%d:x=w-%d-%d-%d-tw:y=%d", l.dot, l.margin, l.clockW, l.bugPad, l.top+(l.bug-l.dot)/2+1),
		// title above the subline; both anchored to the subline's top so a two-line title grows upwards
		"drawtext=" + font + ":" + text(titleFile) + ":fontcolor=white" +
			fmt.Sprintf(":fontsize=%d:x=%d:y=%d-th:", l.title, l.margin, l.subTop()-l.bugPad) + shadow,
		"drawtext=" + font + ":" + text(subFile) + ":fontcolor=white@0.85" +
			fmt.Sprintf(":fontsize=%d:x=%d:y=%d:", l.sub, l.margin, l.subTop()) + shadow,
		// the red band with the ticker scrolling right-to-left
		fmt.Sprintf("drawbox=x=0:y=ih-%d:w=iw:h=%d:color=%s:t=fill", l.band, l.band, bandColor),
	}
	for j := 0; j <= tickerCopies; j++ {
		parts = append(parts, "drawtext="+font+":"+text(tickerFile)+":fontcolor=white"+
			fmt.Sprintf(":fontsize=%d:x=%s:y=h-%d+(%d-th)/2", l.ticker, tickerX(l, j), l.band, l.band))
	}
	return strings.Join(parts, ",")
}

// tickerX is the x expression of ticker copy j (0..tickerCopies).
//
// The usual scroll, x=w-mod(t*speed, w+tw), is periodic in the text width tw: when ticker.txt changes
// (every few minutes, the 制作中 counter), a new tw re-phases the modulo and the text jumps to an
// arbitrary place. Here the period P = tickerPeriod * speed is a constant, so the scroll is continuous
// across any text change (the letters swap in place at worst). One copy alone would leave the band
// empty for P - tw pixels, so the text is drawn m = floor(P/tw) times per period (at most
// tickerCopies), P/m apart: copy j sits at j*P/m - mod(t*speed, P). Copies 0..m are needed to cover
// the frame at every phase (P/m >= w holds for any text that fits the band); the rest are parked off
// the left edge. When the phase wraps, copy j takes over exactly where copy j+m was, so the seam is
// invisible; the gap between copies is P/m - tw plus the separator the text ends with.
func tickerX(l layout, j int) string {
	period := int(tickerPeriod.Seconds()) * l.speed
	if j == 0 {
		return fmt.Sprintf(`-mod(t*%d\,%d)`, l.speed, period)
	}
	m := fmt.Sprintf(`max(1\,min(%d\,floor(%d/tw)))`, tickerCopies, period)
	return fmt.Sprintf(`if(lt(%d\,%s+1)\,%d*%d/%s-mod(t*%d\,%d)\,-tw-w)`, j, m, j, period, m, l.speed, period)
}

// overlay owns the text files drawtext reloads every frame.
//
// The files are overwritten in place, never renamed over: on Windows a rename onto the file ffmpeg is
// about to open makes that open fail (access denied while the old file is delete-pending), and a failed
// reload aborts the filter and kills ffmpeg (measured: a few seconds of continuous rewrites did it). An
// in-place write is seen by ffmpeg either before or after (a torn read would show one odd frame at worst,
// an empty file is fine for drawtext), and it never fails because both sides open with share read/write.
// A shorter text is padded with spaces to the previous length so the file never has to shrink (a
// truncation while ffmpeg maps the file would also fail its reload).
type overlay struct {
	dir       string
	indexFile string
	layout    layout
	log       logx.Logger

	mu     sync.Mutex
	sizes  map[string]int
	ticker string
	warned string // last ticker error logged (once per distinct error)
}

func newOverlay(dir string, opt Options, log logx.Logger) (*overlay, error) {
	o := &overlay{dir: dir, indexFile: opt.IndexFile, layout: newLayout(opt), log: log, sizes: map[string]int{}}
	for _, name := range []string{titleFile, subFile, tickerFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(" "), 0o644); err != nil {
			return nil, err
		}
		o.sizes[name] = 1
	}
	return o, nil
}

// write replaces the file's text in place (see overlay). Empty text becomes one space.
func (o *overlay) write(name, text string) error {
	b := []byte(text)
	if len(b) == 0 {
		b = []byte(" ")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if old := o.sizes[name]; len(b) < old {
		b = append(b, bytes.Repeat([]byte(" "), old-len(b))...)
	}
	path := filepath.Join(o.dir, name)
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		var f *os.File
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
		if err == nil {
			_, err = f.Write(b)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
		if err == nil {
			o.sizes[name] = len(b)
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// setProgramme writes the title and the subline for e (nil clears both).
func (o *overlay) setProgramme(e *OnAir) {
	title, sub := "", ""
	if e != nil {
		title = wrap(stripEmoji(e.Program), o.layout.titleRunes(), 2)
		sub = wrap(stripEmoji(subLine(e)), o.layout.subRunes(), o.layout.subLines)
	}
	if err := o.write(titleFile, title); err != nil {
		o.log("[aitv] RTMP: title.txt: %v", err)
	}
	if err := o.write(subFile, sub); err != nil {
		o.log("[aitv] RTMP: sub.txt: %v", err)
	}
}

// subLine is "<section> · <caption> · 制作 M/D HH:MM" with the empty parts left out; Created is the
// library's local "2006-01-02T15:04:05" (an unreadable one is left out too).
func subLine(e *OnAir) string {
	var parts []string
	if s := strings.TrimSpace(e.Section); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(e.Caption); s != "" {
		parts = append(parts, s)
	}
	if t, err := time.Parse("2006-01-02T15:04:05", e.Created); err == nil {
		parts = append(parts, fmt.Sprintf("制作 %d/%d %02d:%02d", int(t.Month()), t.Day(), t.Hour(), t.Minute()))
	}
	return strings.Join(parts, " · ")
}

// tickerSeparator is what the ticker entries are joined with (the web player's); the streamed text ends
// with it too, so the copies drawn one after the other read as one continuous guide (tickerX).
const tickerSeparator = "　　◆　　"

// refreshTicker rebuilds the ticker from the index and rewrites ticker.txt only when the text changed.
func (o *overlay) refreshTicker() {
	text, err := TickerText(o.indexFile)
	if err != nil {
		if msg := err.Error(); msg != o.warned {
			o.warned = msg
			o.log("[aitv] RTMP: ticker: %v", err)
		}
		return
	}
	o.warned = ""
	if text == o.ticker {
		return
	}
	o.ticker = text
	if err := o.write(tickerFile, stripEmoji(text)+tickerSeparator); err != nil {
		o.log("[aitv] RTMP: ticker.txt: %v", err)
	}
}

// tickerLoop refreshes the ticker every 30 s until ctx ends.
func (o *overlay) tickerLoop(ctx context.Context) {
	t := time.NewTicker(tickerInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.refreshTicker()
		}
	}
}

// wrap breaks s into at most maxLines lines of about maxRunes: each line ends at a " · " separator when
// one falls inside it (the separator is dropped), else after a punctuation mark or a space, else at
// maxRunes; the last line may still overflow the frame.
func wrap(s string, maxRunes, maxLines int) string {
	if maxRunes <= 0 || maxLines <= 1 {
		return s
	}
	var lines []string
	rest := strings.TrimSpace(s)
	for len(lines) < maxLines-1 {
		line, tail := breakLine(rest, maxRunes)
		if tail == "" {
			break
		}
		lines = append(lines, line)
		rest = tail
	}
	return strings.Join(append(lines, rest), "\n")
}

// breakLine splits off the first line of s (tail "" when s fits on one line).
func breakLine(s string, maxRunes int) (line, tail string) {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s, ""
	}
	lo := max(1, maxRunes/3)
	cut, drop := -1, 0
	for i := maxRunes; i >= lo; i-- {
		if r[i-1] == '·' {
			cut, drop = i-1, 1
			break
		}
	}
	if cut < 0 {
		for i := maxRunes; i >= lo; i-- {
			if isBreakRune(r[i-1]) {
				cut = i
				break
			}
		}
	}
	if cut < 0 {
		cut = maxRunes
	}
	line = strings.TrimSpace(string(r[:cut]))
	tail = strings.TrimSpace(string(r[cut+drop:]))
	if line == "" || tail == "" {
		return s, ""
	}
	return line, tail
}

func isBreakRune(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("：:・、，,。！!？?～〜」』）)】", r)
}

// stripEmoji removes the characters a Japanese text font has no glyph for (they render as boxes): the
// emoji planes above the BMP, the BMP code points with emoji presentation, variation selectors, the
// zero-width joiner and the keycap mark.
func stripEmoji(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isEmoji(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func isEmoji(r rune) bool {
	if r >= 0x1F000 && r <= 0x1FAFF {
		return true
	}
	if r == 0x200D || r == 0x20E3 || (r >= 0xFE00 && r <= 0xFE0F) {
		return true
	}
	for _, rg := range emojiPresentationBMP {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// emojiPresentationBMP are the BMP ranges with Emoji_Presentation=Yes (Unicode emoji-data): ☔ ⚡ ✅ ❌ ...
// but not ☀ ★ ◆ ● ♪, which Japanese fonts do have.
var emojiPresentationBMP = [][2]rune{
	{0x231A, 0x231B}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3}, {0x25FD, 0x25FE},
	{0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4},
	{0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA}, {0x26FD, 0x26FD},
	{0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E},
	{0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	{0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55},
}

// checkFilters verifies with "ffmpeg -filters" that every named filter exists (drawtext needs a build with
// libfreetype). A missing ffmpeg is reported as such.
func checkFilters(names ...string) error {
	if len(names) == 0 {
		return nil
	}
	out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("rtmp: ffmpeg not found on PATH: %w", err)
		}
		return fmt.Errorf("rtmp: ffmpeg -filters: %w", err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			have[f[1]] = true
		}
	}
	for _, n := range names {
		if !have[n] {
			return fmt.Errorf("rtmp: this ffmpeg has no %s filter (a full build with libfreetype is needed)", n)
		}
	}
	return nil
}
