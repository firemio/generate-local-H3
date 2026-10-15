package hls

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firemio/generate-local-H3/internal/index"
)

// Golden from aitv.hls._log_on_air with time.time patched to 1758040000.5 (Python json.dump,
// ensure_ascii=False). Python puts a space after ':' and ',', Go does not; keys, values and order must match.
const pyNowGolden = `[{"t": 1758040000.5, "program": "夕方ニュース", "caption": "今日の<d>ヘッドライン</d> & 天気", "section": "headline", "format": "news", "seconds": 6.583333333333333, "created": "2026-09-10T18:24:22", "id": "20260910-182422-72da74"}]`

func TestEncodeOnAirParity(t *testing.T) {
	got, err := encodeOnAir([]OnAir{{
		T: 1758040000.5, Program: "夕方ニュース", Caption: "今日の<d>ヘッドライン</d> & 天気", Section: "headline",
		Format: "news", Seconds: 6.583333333333333, Created: "2026-09-10T18:24:22", ID: "20260910-182422-72da74",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Same document.
	var want, have []map[string]any
	if err := json.Unmarshal([]byte(pyNowGolden), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if !reflect.DeepEqual(want, have) {
		t.Errorf("now.json differs\n got %s\nwant %s", got, pyNowGolden)
	}
	// Same key order, no HTML escaping, no trailing newline (json.dump writes none).
	keys := regexp.MustCompile(`"([a-z]+)":`).FindAllStringSubmatch(string(got), -1)
	var order []string
	for _, k := range keys {
		order = append(order, k[1])
	}
	wantOrder := []string{"t", "program", "caption", "section", "format", "seconds", "created", "id"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("key order %v, want %v", order, wantOrder)
	}
	if !strings.Contains(string(got), "<d>ヘッドライン</d> & 天気") {
		t.Errorf("HTML/Unicode escaped: %s", got)
	}
	if strings.HasSuffix(string(got), "\n") {
		t.Errorf("trailing newline in %q", got)
	}
}

func TestOnAirRing(t *testing.T) {
	dir := t.TempDir()
	now := filepath.Join(dir, "now.json")
	var ring []OnAir
	before := float64(time.Now().UnixMicro()) / 1e6
	for i := 0; i < 45; i++ {
		ring = logOnAir(now, ring, index.Item{ID: fmt.Sprintf("id%d", i), Program: "p", Caption: "c", Seconds: float64(i)})
	}
	// Python: 40 entries, id5 .. id44 (both in memory and in the file).
	if len(ring) != 40 || ring[0].ID != "id5" || ring[39].ID != "id44" {
		t.Fatalf("ring: len %d first %q last %q", len(ring), ring[0].ID, ring[len(ring)-1].ID)
	}
	data, err := os.ReadFile(now)
	if err != nil {
		t.Fatal(err)
	}
	var file []OnAir
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	if !reflect.DeepEqual(file, ring) {
		t.Errorf("now.json (%d entries) differs from the in-memory ring", len(file))
	}
	for i, e := range file {
		if e.T < before || e.T > float64(time.Now().UnixMicro())/1e6 {
			t.Errorf("entry %d: t=%v not a current wall-clock time", i, e.T)
		}
		if i > 0 && e.T < file[i-1].T {
			t.Errorf("entry %d: t went backwards", i)
		}
	}
	if _, err := os.Stat(now + ".tmp"); err == nil {
		t.Error("now.json.tmp left behind")
	}
	// Errors are ignored: an unwritable path still returns the updated ring.
	ring = logOnAir(filepath.Join(dir, "missing", "now.json"), ring, index.Item{ID: "id45"})
	if len(ring) != 40 || ring[39].ID != "id45" {
		t.Errorf("ring not updated on write failure: %d %q", len(ring), ring[len(ring)-1].ID)
	}
}

// logBuf captures log lines from the Broadcast goroutine.
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

func (l *logBuf) count(substr string) int {
	n := 0
	for _, s := range l.all() {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

// testClips returns a clip directory and two items to air: the first two playable entries of the real
// aitv/library/index.json, or two synthetic h264/aac clips rendered with ffmpeg when the library is empty
// (or AITV_TEST_SYNTH=1 forces them).
func testClips(t *testing.T) (string, []index.Item) {
	t.Helper()
	lib := filepath.Join("..", "..", "aitv", "library")
	if data, err := os.ReadFile(filepath.Join(lib, "index.json")); err == nil && os.Getenv("AITV_TEST_SYNTH") == "" {
		var items []index.Item
		if json.Unmarshal(data, &items) == nil {
			var picked []index.Item
			for _, it := range items {
				if it.File == "" {
					continue
				}
				if _, err := os.Stat(filepath.Join(lib, it.File)); err != nil {
					continue
				}
				picked = append(picked, it)
				if len(picked) == 2 {
					abs, err := filepath.Abs(lib)
					if err != nil {
						t.Fatal(err)
					}
					return abs, picked
				}
			}
		}
	}
	// No library: render two 8 s clips shaped like the library's (h264 High with B-frames, 2 s GOP, AAC
	// stereo, ~1.2 Mbps). They must be longer than 5 s: the encoder's mpegts probe (analyzeduration)
	// swallows the first ~5 s of its input before -re pacing starts.
	dir := t.TempDir()
	var items []index.Item
	for i, name := range []string{"news", "cm"} {
		file := fmt.Sprintf("%s-synth-%d.mp4", name, i)
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=size=640x360:rate=24",
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-t", "8", "-vf", "noise=alls=20:allf=t",
			"-c:v", "libx264", "-preset", "veryfast", "-g", "48", "-bf", "2",
			"-b:v", "1200k", "-maxrate", "1200k", "-bufsize", "600k", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-ac", "2", "-shortest",
			filepath.Join(dir, file))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("no library clips and ffmpeg could not render a test clip: %v: %s", err, out)
		}
		items = append(items, index.Item{ID: fmt.Sprintf("synth-%d", i), File: file, Format: name,
			Program: "Synthetic " + name, Caption: "caption " + name, Seconds: 8, Created: "2026-01-01T00:00:00"})
	}
	return dir, items
}

// playlistContent sums the EXTINF durations of a playlist.
func playlistContent(pl string) float64 {
	total := 0.0
	for _, line := range strings.Split(pl, "\n") {
		if strings.HasPrefix(line, "#EXTINF:") {
			var d float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, "#EXTINF:"), "%f", &d); err == nil {
				total += d
			}
		}
	}
	return total
}

// runBroadcast runs Broadcast with a ctx deadline of d and returns its error, the log and every ffmpeg spawned.
func runBroadcast(t *testing.T, opt Options, d time.Duration) (error, *logBuf, []*child) {
	t.Helper()
	lg := &logBuf{}
	var mu sync.Mutex
	var kids []*child
	opt.Log = lg.f
	userSpawn := opt.onSpawn
	opt.onSpawn = func(c *child) {
		mu.Lock()
		kids = append(kids, c)
		mu.Unlock()
		if userSpawn != nil {
			userSpawn(c)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- Broadcast(ctx, opt) }()
	var err error
	select {
	case err = <-errc:
	case <-time.After(d + 2*stopWait + 5*time.Second):
		t.Fatalf("Broadcast did not return after cancellation; log:\n%s", strings.Join(lg.all(), "\n"))
	}
	mu.Lock()
	defer mu.Unlock()
	return err, lg, kids
}

// checkReaped asserts that every ffmpeg was waited for (Wait returned, so the process is gone).
func checkReaped(t *testing.T, kids []*child) {
	t.Helper()
	for i, c := range kids {
		if !c.exited() {
			t.Errorf("child %d (pid %d, %v) was not waited for", i, c.cmd.Process.Pid, c.cmd.Args[len(c.cmd.Args)-1])
			continue
		}
		if c.cmd.ProcessState == nil || !c.cmd.ProcessState.Exited() {
			t.Errorf("child %d (pid %d): Wait returned but the process has not exited", i, c.cmd.Process.Pid)
		}
	}
}

func encoders(kids []*child) (n int) {
	for _, c := range kids {
		if c.stdin != nil {
			n++
		}
	}
	return n
}

func TestBroadcast(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	lib, clips := testClips(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale.ts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	served := false
	opt := Options{Dir: dir, Lib: lib, SegmentSeconds: DefaultSegmentSeconds, nextBlock: func() []index.Item {
		if served {
			return nil
		}
		served = true
		return append([]index.Item(nil), clips...)
	}}
	start := time.Now()
	err, lg, kids := runBroadcast(t, opt, 12*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Broadcast: %v\n%s", err, strings.Join(lg.all(), "\n"))
	}
	if elapsed > 12*time.Second+stopWait+2*time.Second {
		t.Errorf("Broadcast took %v to return after a 12 s deadline", elapsed)
	}
	t.Logf("log:\n%s", strings.Join(lg.all(), "\n"))

	// The directory was emptied at start.
	if _, err := os.Stat(filepath.Join(dir, "stale.ts")); err == nil {
		t.Error("stale file in the HLS dir was not removed at start")
	}
	// live.m3u8 with segments that exist.
	m3u8, err := os.ReadFile(filepath.Join(dir, "live.m3u8"))
	if err != nil {
		t.Fatalf("live.m3u8: %v", err)
	}
	pl := string(m3u8)
	if !strings.HasPrefix(pl, "#EXTM3U") {
		t.Errorf("live.m3u8 does not start with #EXTM3U:\n%s", pl)
	}
	if !strings.Contains(pl, "#EXT-X-PROGRAM-DATE-TIME:") {
		t.Errorf("live.m3u8 has no program-date-time:\n%s", pl)
	}
	if strings.Contains(pl, "#EXT-X-ENDLIST") {
		t.Errorf("live.m3u8 has an ENDLIST (omit_endlist):\n%s", pl)
	}
	segs := 0
	for _, line := range strings.Split(pl, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "seg") && strings.HasSuffix(line, ".ts") {
			segs++
			if _, err := os.Stat(filepath.Join(dir, line)); err != nil {
				t.Errorf("segment %s listed but missing", line)
			}
		}
	}
	if segs < 2 {
		t.Errorf("only %d segments after ~12 s:\n%s", segs, pl)
	}
	// -re pacing: after 12 s of wall clock the stream holds ~12.5 s of content (0.5 s initial burst, the
	// partial segment flushed at stop), never the ~13-16 s of the two clips, and it did not stall either.
	content := playlistContent(pl)
	t.Logf("%d segments, %.1f s of content in live.m3u8 after a 12 s run", segs, content)
	if content < 8 || content > 14.5 {
		t.Errorf("playlist holds %.1f s of content after a 12 s run (want ~12.5 s):\n%s", content, pl)
	}
	// now.json has the two clips, in order, with sane times.
	data, err := os.ReadFile(filepath.Join(dir, "now.json"))
	if err != nil {
		t.Fatalf("now.json: %v", err)
	}
	var onAir []OnAir
	if err := json.Unmarshal(data, &onAir); err != nil {
		t.Fatalf("now.json: %v: %s", err, data)
	}
	if len(onAir) != 2 {
		t.Fatalf("now.json has %d entries, want 2: %s", len(onAir), data)
	}
	for i, e := range onAir {
		c := clips[i]
		if e.ID != c.ID || e.Program != c.Program || e.Caption != c.Caption || e.Format != c.Format ||
			e.Section != c.Section || e.Seconds != c.Seconds || e.Created != c.Created {
			t.Errorf("now.json[%d] = %+v, want fields of %+v", i, e, c)
		}
		if e.T < float64(start.UnixMicro())/1e6 || e.T > float64(time.Now().UnixMicro())/1e6 {
			t.Errorf("now.json[%d].t = %v outside the test's wall-clock window", i, e.T)
		}
	}
	// The feed is back-pressured by the encoder (a clip is written only as fast as ffmpeg takes it), so the
	// second clip cannot start before the encoder's probe window (~5 s) plus pacing of the rest of clip 1
	// has passed; only ~5 s are swallowed up front, so it starts well after the first.
	gap := onAir[1].T - onAir[0].T
	t.Logf("second clip went on air %.1f s after the first", gap)
	if gap < 1 {
		t.Errorf("second clip went on air only %.1f s after the first (the whole clip was buffered somewhere?)", gap)
	}
	// Log wording.
	lines := lg.all()
	if len(lines) == 0 || lines[0] != "[aitv] HLS broadcaster started -> aitv/hls/live.m3u8" {
		t.Errorf("first log line %q", lines)
	}
	wantBlock := "[aitv] block: " + clips[0].Format + " > " + clips[1].Format
	if lg.count(wantBlock) != 1 {
		t.Errorf("block line %q not logged once:\n%s", wantBlock, strings.Join(lines, "\n"))
	}
	for _, c := range clips {
		if lg.count("[aitv] >> "+c.Program+" / "+c.Caption) != 1 {
			t.Errorf("on-air line for %s not logged once", c.ID)
		}
	}
	if lg.count("restarting") != 0 {
		t.Errorf("encoder restarted during a healthy run:\n%s", strings.Join(lines, "\n"))
	}
	if lg.count("library empty") > 1 {
		t.Errorf("'library empty' warned more than once:\n%s", strings.Join(lines, "\n"))
	}
	// One encoder, one remux per clip, all reaped.
	if encoders(kids) != 1 || len(kids) != 3 {
		t.Errorf("spawned %d ffmpeg (%d encoders), want 3 (1 encoder + 2 remuxes)", len(kids), encoders(kids))
	}
	checkReaped(t, kids)
}

func TestBroadcastEncoderRestart(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	lib, clips := testClips(t)
	var once sync.Once
	opt := Options{Dir: t.TempDir(), Lib: lib, SegmentSeconds: DefaultSegmentSeconds,
		nextBlock: func() []index.Item { return append([]index.Item(nil), clips...) }, // forever
		onSpawn: func(c *child) {
			if c.stdin == nil {
				return
			}
			once.Do(func() { // the first encoder dies 1.5 s into the first clip
				go func() {
					time.Sleep(1500 * time.Millisecond)
					c.kill()
				}()
			})
		},
	}
	err, lg, kids := runBroadcast(t, opt, 10*time.Second)
	if err != nil {
		t.Fatalf("Broadcast: %v\n%s", err, strings.Join(lg.all(), "\n"))
	}
	t.Logf("log:\n%s", strings.Join(lg.all(), "\n"))
	// Either the write into the dead encoder failed (usual) or the exit was seen between clips.
	if lg.count("[aitv] encoder pipe broke; restarting encoder")+lg.count("[aitv] HLS encoder exited (") != 1 {
		t.Errorf("expected exactly one restart line:\n%s", strings.Join(lg.all(), "\n"))
	}
	if encoders(kids) != 2 {
		t.Errorf("%d encoders spawned, want 2", encoders(kids))
	}
	if lg.count("[aitv] >> ") < 2 {
		t.Errorf("the channel did not continue after the restart:\n%s", strings.Join(lg.all(), "\n"))
	}
	if _, err := os.Stat(filepath.Join(opt.Dir, "live.m3u8")); err != nil {
		t.Errorf("live.m3u8 after restart: %v", err)
	}
	checkReaped(t, kids)
}

func TestBroadcastCancelledWhileEmpty(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	opt := Options{Dir: t.TempDir(), Lib: t.TempDir(), SegmentSeconds: DefaultSegmentSeconds, nextBlock: func() []index.Item { return nil }}
	start := time.Now()
	err, lg, kids := runBroadcast(t, opt, 2*time.Second)
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if lg.count("[aitv] library empty, waiting for the producer...") != 1 {
		t.Errorf("empty warning not logged once:\n%s", strings.Join(lg.all(), "\n"))
	}
	if d := time.Since(start); d > 2*time.Second+stopWait+time.Second {
		t.Errorf("took %v to stop", d)
	}
	if len(kids) != 1 || encoders(kids) != 1 {
		t.Errorf("spawned %d ffmpeg, want just the encoder", len(kids))
	}
	checkReaped(t, kids)
}

// slowSink is an encoder stdin whose writes are paced like ffmpeg's -re input: every chunk blocks a while.
type slowSink struct {
	mu    sync.Mutex
	data  []byte
	pause time.Duration
}

func (s *slowSink) Write(p []byte) (int, error) {
	time.Sleep(s.pause)
	s.mu.Lock()
	s.data = append(s.data, p...)
	s.mu.Unlock()
	return len(p), nil
}

func (s *slowSink) Close() error { return nil }

// feed must hand the encoder every byte of the remuxed clip, however slowly the encoder takes them (the
// Python read remux.stdout to EOF before waiting on it): a remux reaped while a write is blocked must not
// drop the TS packets still buffered in the pipe — that lost the last audio frames of every clip.
func TestFeedDeliversWholeClip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	lib, clips := testClips(t)
	src := filepath.Join(lib, clips[0].File)
	want, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", src,
		"-c:v", "copy", "-bsf:v", "h264_mp4toannexb", "-c:a", "copy", "-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Fatalf("reference remux: %v", err)
	}
	if len(want) == 0 || len(want)%188 != 0 {
		t.Fatalf("reference remux gave %d bytes", len(want))
	}
	for _, pause := range []time.Duration{0, 10 * time.Millisecond, 50 * time.Millisecond} {
		sink := &slowSink{pause: pause}
		b := &broadcaster{ctx: context.Background(), log: (&logBuf{}).f, enc: &child{stdin: sink}}
		broken, err := b.feed(src)
		if err != nil || broken {
			t.Fatalf("pause %v: feed = %v, %v", pause, broken, err)
		}
		if !bytes.Equal(sink.data, want) {
			t.Errorf("pause %v: delivered %d of %d bytes (%d TS packets lost)", pause, len(sink.data), len(want), (len(want)-len(sink.data))/188)
		}
	}
}

// --segment is handed to ffmpeg verbatim (Python: str(segment_seconds)): 0 and -1 reach -hls_time as they
// are, the station's default is 2; the broadcaster never substitutes a value of its own.
func TestSegmentSecondsVerbatim(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	for _, seg := range []int{DefaultSegmentSeconds, 0, -1, 7} {
		opt := Options{Dir: t.TempDir(), Lib: t.TempDir(), SegmentSeconds: seg, nextBlock: func() []index.Item { return nil }}
		err, _, kids := runBroadcast(t, opt, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("Broadcast: %v", err)
		}
		if len(kids) == 0 {
			t.Fatalf("segment %d: no encoder spawned", seg)
		}
		args := kids[0].cmd.Args
		i := slices.Index(args, "-hls_time")
		if i < 0 || i+1 >= len(args) || args[i+1] != strconv.Itoa(seg) {
			t.Errorf("segment %d: encoder args %v", seg, args)
		}
		checkReaped(t, kids)
	}
}
