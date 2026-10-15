package rtmp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/paths"
	"github.com/firemio/generate-local-H3/internal/winproc"
)

// source reads the live playlist, its segments and now.json, from a directory or over HTTP.
type source interface {
	playlist() (string, error)
	segment(uri string) ([]byte, error)
	// arrival is when the segment's last byte was written (a file's mtime), exact=true; over HTTP it is
	// unknown (zero, false) and the follower falls back to when the segment was first listed.
	arrival(uri string) (t time.Time, exact bool)
	// now returns the current now.json entries and whether they changed since the last call (a file is
	// re-read when its mtime or size changed; over HTTP the body is fetched every 2 s and compared).
	now() ([]OnAir, bool, error)
	String() string
}

// newSource resolves Options.Source: an http(s) URL of live.m3u8, a path of live.m3u8, a directory
// holding it, or "" for paths.HLS(). nowFile overrides where now.json is read from (always a file).
func newSource(spec, nowFile string) (source, error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://") {
		u, err := url.Parse(spec)
		if err != nil {
			return nil, fmt.Errorf("rtmp: source %q: %w", spec, err)
		}
		if !strings.HasSuffix(u.Path, ".m3u8") {
			u = u.JoinPath("live.m3u8")
		}
		s := &httpSource{playlistURL: u, client: &http.Client{Timeout: 10 * time.Second}}
		s.nowURL = u.ResolveReference(&url.URL{Path: "now.json"})
		if nowFile != "" {
			s.nowPath = nowFile
		}
		return s, nil
	}
	var dir, file string
	switch {
	case spec == "":
		dir = paths.HLS()
	case strings.HasSuffix(strings.ToLower(spec), ".m3u8"):
		file = spec
	default:
		if st, err := os.Stat(spec); err == nil && !st.IsDir() {
			file = spec
		} else {
			dir = spec
		}
	}
	if file == "" {
		file = filepath.Join(dir, "live.m3u8")
	} else {
		dir = filepath.Dir(file)
	}
	if nowFile == "" {
		nowFile = filepath.Join(dir, "now.json")
	}
	return &fileSource{dir: dir, file: file, nowPath: nowFile}, nil
}

// readFile is os.ReadFile through openShared, in one read. The broadcaster's directory is shared with
// its writer: hlsenc deletes old segments (FILE_SHARE_DELETE lets that succeed while one is being read)
// and renames live.m3u8.tmp over live.m3u8, as the broadcaster renames now.json.tmp over now.json — and
// on Windows a MoveFileEx replace fails while the target is open, whatever the sharing mode (the writer
// would have to use POSIX rename semantics). So the open is kept as short as one read, and both files
// are only read after their size or mtime changed (fileSource.playlist, now): right after a rename,
// never during the next one.
func readFile(path string) ([]byte, error) {
	f, err := openShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size := int64(0)
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	var buf bytes.Buffer
	buf.Grow(int(size) + 512)
	_, err = buf.ReadFrom(f)
	return buf.Bytes(), err
}

// fileSource reads the broadcaster's directory directly.
type fileSource struct {
	dir, file, nowPath string
	nowMod             time.Time
	nowSize            int64
	entries            []OnAir
	plMod              time.Time
	plSize             int64
	plText             string
}

func (s *fileSource) String() string { return s.file }

// playlist re-reads live.m3u8 only when its mtime or size changed (see readFile); a stat is not an open.
func (s *fileSource) playlist() (string, error) {
	st, err := os.Stat(s.file)
	if err != nil {
		return "", err
	}
	if st.ModTime().Equal(s.plMod) && st.Size() == s.plSize && s.plText != "" {
		return s.plText, nil
	}
	b, err := readFile(s.file)
	if err != nil {
		return "", err
	}
	s.plMod, s.plSize, s.plText = st.ModTime(), st.Size(), string(b)
	return s.plText, nil
}

func (s *fileSource) path(uri string) string {
	if filepath.IsAbs(uri) {
		return uri
	}
	return filepath.Join(s.dir, filepath.FromSlash(uri))
}

func (s *fileSource) segment(uri string) ([]byte, error) {
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		return httpGet(&http.Client{Timeout: 10 * time.Second}, uri)
	}
	return readFile(s.path(uri))
}

// arrival is the segment file's mtime: hlsenc closes a segment (its last bytes are flushed) the moment
// the next segment's first keyframe comes through the -re paced encoder, and lists it right after, so
// mtime - EXTINF is when the encoder took the segment's first frame from the broadcaster.
func (s *fileSource) arrival(uri string) (time.Time, bool) {
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		return time.Time{}, false
	}
	st, err := os.Stat(s.path(uri))
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

func (s *fileSource) now() ([]OnAir, bool, error) {
	st, err := os.Stat(s.nowPath)
	if err != nil {
		return s.entries, false, err
	}
	if st.ModTime().Equal(s.nowMod) && st.Size() == s.nowSize {
		return s.entries, false, nil
	}
	entries, err := readNow(s.nowPath)
	if err != nil {
		return s.entries, false, err // torn read during the broadcaster's replace: retried next poll
	}
	s.nowMod, s.nowSize, s.entries = st.ModTime(), st.Size(), entries
	return s.entries, true, nil
}

// readNow parses a now.json file (the broadcaster's compact JSON array).
func readNow(path string) ([]OnAir, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return parseNow(data)
}

func parseNow(data []byte) ([]OnAir, error) {
	var entries []OnAir
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// httpSource follows the playlist through the station's web server (or any HTTP server).
type httpSource struct {
	playlistURL *url.URL
	nowURL      *url.URL
	nowPath     string // NowFile override: read as a file instead
	client      *http.Client
	nowFetched  time.Time
	nowBody     []byte
	nowMod      time.Time
	nowSize     int64
	entries     []OnAir
}

func (s *httpSource) String() string { return s.playlistURL.String() }

func (s *httpSource) playlist() (string, error) {
	b, err := httpGet(s.client, s.playlistURL.String())
	return string(b), err
}

func (s *httpSource) segment(uri string) ([]byte, error) {
	ref, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	return httpGet(s.client, s.playlistURL.ResolveReference(ref).String())
}

func (s *httpSource) arrival(string) (time.Time, bool) { return time.Time{}, false }

func (s *httpSource) now() ([]OnAir, bool, error) {
	if s.nowPath != "" {
		st, err := os.Stat(s.nowPath)
		if err != nil {
			return s.entries, false, err
		}
		if st.ModTime().Equal(s.nowMod) && st.Size() == s.nowSize {
			return s.entries, false, nil
		}
		entries, err := readNow(s.nowPath)
		if err != nil {
			return s.entries, false, err
		}
		s.nowMod, s.nowSize, s.entries = st.ModTime(), st.Size(), entries
		return s.entries, true, nil
	}
	if time.Since(s.nowFetched) < nowPollHTTP {
		return s.entries, false, nil
	}
	s.nowFetched = time.Now()
	body, err := httpGet(s.client, s.nowURL.String())
	if err != nil {
		return s.entries, false, err
	}
	if bytes.Equal(body, s.nowBody) {
		return s.entries, false, nil
	}
	entries, err := parseNow(body)
	if err != nil {
		return s.entries, false, err
	}
	s.nowBody, s.entries = body, entries
	return s.entries, true, nil
}

func httpGet(c *http.Client, u string) ([]byte, error) {
	resp, err := c.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// follower polls the playlist and feeds new segments to ffmpeg's stdin, keeping the overlay in step.
// Every ffmpeg launch starts it afresh at the live edge (run).
//
// Timing. now.json's t is the broadcaster's wall clock when it started feeding a clip to its encoder;
// the segments' EXT-X-PROGRAM-DATE-TIME is not that clock: ffmpeg's hls muxer takes the encoder's start
// time once and adds media durations, so it drifts from the wall clock (the RTC's rate error, every
// w32time step, the initial read-rate burst — 9.7 s on the station after a day). The follower therefore
// never compares t with a PROGRAM-DATE-TIME. What it knows for sure is when a segment was finished: a
// file's mtime (exact: the encoder is -re paced, the segment is closed and listed when its last frame's
// time comes), or over HTTP when it was first listed (late by up to one poll; the best of the last few
// segments is kept). The feed time of a segment's first frame is that minus EXTINF (feedStart); with a
// PROGRAM-DATE-TIME the offset "date-time minus wall clock" is re-estimated from the newest segments
// and applied to older ones. Entries are then matched on feed time (deliver).
type follower struct {
	src     source
	overlay *overlay
	log     logx.Logger

	next      int     // next media sequence to deliver
	synced    bool    // next is valid; false until the first playlist after a launch (or a reset)
	entries   []OnAir // sorted by T
	shown     string  // key of the now.json entry the overlay currently shows
	lastWrite time.Time
	warnedSrc string // last source error logged (once per distinct message)
	warnedNow string

	seen    map[int]time.Time // media sequence → first listed (backdated by EXTINF at a (re)sync); nil = resync
	offsets []time.Duration   // PROGRAM-DATE-TIME minus wall clock, newest last (offsetSamples at most)
}

const (
	offsetSamples  = 8                      // arrival-based estimates kept (HTTP: the largest wins)
	boundaryLag    = 150 * time.Millisecond // a clip's first frame is fed ~0.1-0.4 s after its now.json t
	boundaryWindow = 700 * time.Millisecond // a keyframe this close to (t + lag) is that clip's first frame
	startLag       = 50 * time.Millisecond  // the least a clip's t precedes the feed of its first frame
	flushFrames    = 3                      // frames of a new clip fed before its text goes in (flushOffset)
)

// errStalled is returned when a segment write hung for stallTimeout and ffmpeg was killed.
var errStalled = errors.New("ffmpeg stopped reading its input; killed")

// run feeds w until ctx ends (nil), ffmpeg exits (exited closed → errExited), or a write fails (the error).
// kill is called when a write hangs for stallTimeout.
func (f *follower) run(ctx context.Context, w io.Writer, exited <-chan struct{}, kill func()) error {
	// A (re)launched ffmpeg starts a new timeline: resync to the live edge and write the overlay afresh.
	f.synced = false
	f.shown = ""
	f.overlay.setProgramme(nil)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		if err := f.poll(ctx, w, kill); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-exited:
			return errExited
		case <-tick.C:
		}
	}
}

var errExited = errors.New("ffmpeg exited")

// poll reads now.json and the playlist once and delivers every new segment.
func (f *follower) poll(ctx context.Context, w io.Writer, kill func()) error {
	if entries, changed, err := f.src.now(); err != nil {
		f.warnOnce(&f.warnedNow, "now.json", err)
	} else {
		f.warnedNow = ""
		if changed {
			f.entries = append([]OnAir(nil), entries...)
			sort.SliceStable(f.entries, func(i, j int) bool { return f.entries[i].T < f.entries[j].T })
		}
	}
	text, err := f.src.playlist()
	if err != nil {
		f.warnOnce(&f.warnedSrc, "playlist", err)
		return nil
	}
	segs := ParsePlaylist(text)
	if len(segs) == 0 {
		return nil // being rewritten, or the broadcaster has not started: retry
	}
	f.warnedSrc = ""
	last := segs[len(segs)-1].Seq
	if !f.synced || last < f.next-1 {
		if f.synced {
			f.log("[aitv] RTMP: playlist reset, resyncing")
		}
		f.next = segs[max(0, len(segs)-startBehind)].Seq
		f.synced = true
		f.seen, f.offsets = nil, nil // a restarted encoder has a new date-time epoch
	}
	f.observe(segs, time.Now())
	for _, s := range segs {
		if ctx.Err() != nil {
			return nil
		}
		if s.Seq < f.next {
			continue
		}
		if s.Seq > f.next {
			f.log("[aitv] RTMP: %d segment(s) left the window before they were sent; skipping ahead", s.Seq-f.next)
		}
		data, err := f.src.segment(s.URI)
		if err != nil {
			f.log("[aitv] RTMP: segment %s: %v (skipped)", s.URI, err)
			f.next = s.Seq + 1
			continue
		}
		if err := f.deliver(ctx, w, kill, s, data); err != nil {
			return err
		}
		f.next = s.Seq + 1
	}
	return nil
}

// observe records when each segment was first listed and refreshes the date-time offset from the
// newly listed ones. At a (re)sync the listed segments are backdated by the EXTINFs after them (they
// were listed one after the other, each when it was complete).
func (f *follower) observe(segs []Segment, now time.Time) {
	fresh := f.seen == nil
	if fresh {
		f.seen = make(map[int]time.Time, len(segs))
		t := now
		for i := len(segs) - 1; i >= 0; i-- {
			f.seen[segs[i].Seq] = t
			t = t.Add(-durationOf(segs[i]))
		}
	}
	listed := make(map[int]bool, len(segs))
	for _, s := range segs {
		listed[s.Seq] = true
		if _, ok := f.seen[s.Seq]; !ok {
			f.seen[s.Seq] = now
		} else if !fresh {
			continue
		}
		if s.Start.IsZero() {
			continue
		}
		arrived, exact := f.src.arrival(s.URI)
		if !exact {
			if fresh && s.Seq != segs[len(segs)-1].Seq {
				continue // backdated guesses: only the newest is worth keeping
			}
			arrived = f.seen[s.Seq]
		}
		est := s.Start.Add(durationOf(s)).Sub(arrived)
		if exact {
			f.offsets = []time.Duration{est} // the newest exact one is the truth
			continue
		}
		f.offsets = append(f.offsets, est)
		if len(f.offsets) > offsetSamples {
			f.offsets = f.offsets[1:]
		}
	}
	for seq := range f.seen {
		if !listed[seq] {
			delete(f.seen, seq)
		}
	}
}

func durationOf(s Segment) time.Duration { return time.Duration(s.Duration * float64(time.Second)) }

// offset is the current "PROGRAM-DATE-TIME minus wall clock" estimate (ok=false before any). A listing
// can only be seen late, which makes an estimate too small, so the largest recent one is the closest.
func (f *follower) offset() (time.Duration, bool) {
	if len(f.offsets) == 0 {
		return 0, false
	}
	best := f.offsets[0]
	for _, d := range f.offsets[1:] {
		if d > best {
			best = d
		}
	}
	return best, true
}

// feedStart is the wall-clock time the broadcaster's encoder took s's first frame (see follower).
func (f *follower) feedStart(s Segment) time.Time {
	if t, exact := f.src.arrival(s.URI); exact {
		return t.Add(-durationOf(s))
	}
	if off, ok := f.offset(); ok && !s.Start.IsZero() {
		return s.Start.Add(-off)
	}
	if t, ok := f.seen[s.Seq]; ok {
		return t.Add(-durationOf(s))
	}
	return time.Now().Add(-durationOf(s))
}

// part is one clip of a segment: its first frame's byte offset (Off, 0 for the clip the segment starts
// with), the entry to show for it (idx, -1 = none) and where the write may pause for the text change
// (Flush, see flushOffset).
type part struct {
	Off, Flush int
	idx        int
}

// parts plans how s is written: the entry on air at its first frame, then one more part per clip
// boundary inside it. The broadcaster's muxer cuts a segment at the first keyframe after each 2 s of
// media, and clips are 2 s GOPs, so a clip regularly starts part-way through a segment: that first
// frame is a keyframe, and the now.json entry whose t is nearest to its feed time (less the feed lag)
// belongs to it. The segment's own first frame shows the entry fed last before it (onAirIndex): a clip
// whose t falls inside the segment starts inside it or after it, never at its first frame, however
// close (a 0.58 s segment holding the old clip's last GOP used to flip 14 frames early).
func (f *follower) parts(s Segment, data []byte, start time.Time) []part {
	pes := tsVideo(data)
	first := onAirIndex(f.entries, start)
	var cuts []part
	claimed := map[int]bool{}
	prev := first - 1
	for i, p := range pes {
		if i == 0 || !p.Key {
			continue
		}
		e := boundaryIndex(f.entries, start.Add(time.Duration(p.PTS*float64(time.Second))))
		if e < 0 || e <= prev {
			continue
		}
		cuts = append(cuts, part{Off: p.Off, Flush: flushOffset(pes, i, len(data)), idx: e})
		claimed[e] = true
		prev = e
	}
	for first >= 0 && claimed[first] {
		first--
	}
	return append([]part{{Off: 0, Flush: flushOffset(pes, 0, len(data)), idx: first}}, cuts...)
}

// deliver writes s to ffmpeg with the overlay changed at each clip boundary (parts): for every part
// whose entry is not the one shown, the bytes up to its Flush go first, then (after the settle) the
// text, then the rest.
func (f *follower) deliver(ctx context.Context, w io.Writer, kill func(), s Segment, data []byte) error {
	pos := 0
	for _, p := range f.parts(s, data, f.feedStart(s)) {
		var e *OnAir
		if p.idx >= 0 {
			e = &f.entries[p.idx]
		}
		if onAirKey(e) == f.shown {
			continue
		}
		if end := max(pos, p.Flush); end > pos {
			if err := f.write(w, kill, data[pos:end]); err != nil {
				return err
			}
			pos = end
		}
		if err := f.sync(ctx, e); err != nil {
			return err
		}
	}
	if pos < len(data) {
		return f.write(w, kill, data[pos:])
	}
	return nil
}

// write sends b to ffmpeg, killing it (errStalled) when the write hangs for stallTimeout.
func (f *follower) write(w io.Writer, kill func(), b []byte) error {
	timer := time.AfterFunc(stallTimeout, kill)
	_, werr := w.Write(b)
	stalled := !timer.Stop()
	f.lastWrite = time.Now()
	if werr != nil {
		if stalled {
			return errStalled
		}
		return fmt.Errorf("write to ffmpeg: %w", werr)
	}
	return nil
}

// sync puts the overlay on e (nil clears it) before the next bytes are written. When that means a change,
// it first lets ffmpeg finish the frames it already has (settleWait after the last write), so the new
// title does not land on the tail of the old clip.
func (f *follower) sync(ctx context.Context, e *OnAir) error {
	key := onAirKey(e)
	if key == f.shown {
		return nil
	}
	if wait := settleWait - time.Since(f.lastWrite); wait > 0 && !f.lastWrite.IsZero() {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	f.overlay.setProgramme(e)
	f.shown = key
	return nil
}

// onAirKey identifies an entry for follower.shown ("" for nil).
func onAirKey(e *OnAir) string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%.6f/%s", e.T, e.ID)
}

// onAirIndex is the entry on air at feed time t, as an index into T-sorted entries (-1 when nothing is
// on air yet): the greatest T at least startLag before t — a clip's now.json line is written before its
// first frame is fed, so an entry written after t cannot be on screen at t.
func onAirIndex(entries []OnAir, t time.Time) int {
	limit := unixSeconds(t.Add(-startLag))
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].T <= limit {
			return i
		}
	}
	return -1
}

// boundaryIndex is the entry whose t is nearest to feed - boundaryLag, if within boundaryWindow (-1 when
// none): the clip whose first frame was fed at feed.
func boundaryIndex(entries []OnAir, feed time.Time) int {
	target := unixSeconds(feed.Add(-boundaryLag))
	best, bestD := -1, boundaryWindow.Seconds()
	for i := range entries {
		if d := math.Abs(entries[i].T - target); d <= bestD {
			best, bestD = i, d
		}
	}
	return best
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

func (f *follower) warnOnce(slot *string, what string, err error) {
	msg := err.Error()
	if *slot == msg {
		return
	}
	*slot = msg
	f.log("[aitv] RTMP: %s: %v", what, err)
}

// runFFmpeg launches ffmpeg once, runs the follower against its stdin and returns why it ended: ffmpeg's
// exit status (with the follower's reason when it had one), or exec.ErrNotFound when there is no ffmpeg.
// ctx cancellation closes stdin (ffmpeg drains and exits) and kills it after stopWait if it lingers.
func runFFmpeg(ctx context.Context, args []string, rawURL string, f *follower) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	mask := &maskWriter{w: os.Stderr, url: rawURL, masked: Masked(rawURL), secret: secretOf(rawURL)}
	cmd.Stderr = mask
	cmd.Cancel = stdin.Close
	cmd.WaitDelay = stopWait
	winproc.NoWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	var killOnce sync.Once
	kill := func() { killOnce.Do(func() { _ = cmd.Process.Kill() }) }
	reason := f.run(ctx, stdin, done, kill)
	_ = stdin.Close()
	select {
	case <-done:
	case <-time.After(stopWait):
		kill()
		<-done
	}
	mask.flush()
	status := "exited"
	if cmd.ProcessState != nil {
		status = cmd.ProcessState.String()
	}
	if reason == nil || errors.Is(reason, errExited) {
		return errors.New(status)
	}
	return fmt.Errorf("%v; %s", reason, status)
}

// secretOf is the stream key (the URL's last path element); short ones are not masked on their own (a
// one-letter key would blank every such letter in ffmpeg's messages) — the whole URL always is.
func secretOf(rawURL string) string {
	key := rawURL[strings.LastIndexByte(rawURL, '/')+1:]
	if i := strings.IndexByte(key, '?'); i >= 0 {
		key = key[:i]
	}
	if len(key) < 8 {
		return ""
	}
	return key
}

// maskWriter forwards ffmpeg's stderr line by line with the ingest URL and the stream key masked.
type maskWriter struct {
	w      io.Writer
	secret string
	masked string
	mu     sync.Mutex
	buf    []byte
	url    string
}

func (m *maskWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buf = append(m.buf, p...)
	for {
		i := bytes.IndexByte(m.buf, '\n')
		if i < 0 {
			break
		}
		m.emit(m.buf[:i+1])
		m.buf = m.buf[i+1:]
	}
	return len(p), nil
}

func (m *maskWriter) emit(line []byte) {
	s := string(line)
	if m.url != "" {
		s = strings.ReplaceAll(s, m.url, m.masked)
	}
	if m.secret != "" {
		s = strings.ReplaceAll(s, m.secret, "****")
	}
	_, _ = io.WriteString(m.w, s)
}

// flush writes a last partial line (ffmpeg's messages end with a newline; this covers a kill mid-line).
func (m *maskWriter) flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.buf) > 0 {
		m.emit(append(m.buf, '\n'))
		m.buf = nil
	}
}
