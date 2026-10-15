package producer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firemio/generate-local-H3/internal/comfy"
	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/llm"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/programs"
	"github.com/firemio/generate-local-H3/internal/script"
	"github.com/firemio/generate-local-H3/internal/sources"
)

// Short names for the seam signatures.
type (
	llmClientT = llm.Client
	materialT  = sources.Material
	logT       = logx.Logger
	randT      = rand.Rand
)

// Golden values computed with the Python producer on the fixtures under testdata/ (see the report:
// aitv.producer.Producer._resume_slot / _unfinished with index_store.INDEX and script.SCRIPTS patched).
const (
	fixturePID   = "20260916-201504-2e47cd"
	fixtureTitle = "竹の海：風と影が奏でる緑の交響曲"
)

// ---- helpers -------------------------------------------------------------------------------------

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logBuf) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *logBuf) has(sub string) bool {
	for _, s := range l.all() {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (l *logBuf) count(sub string) int {
	n := 0
	for _, s := range l.all() {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (l *logBuf) mustHave(t *testing.T, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !l.has(sub) {
			t.Errorf("log lacks %q; log:\n%s", sub, strings.Join(l.all(), "\n"))
		}
	}
}

// fakeGPU stands in for *comfy.Client.
type fakeGPU struct {
	mu         sync.Mutex
	alive      func(call int) bool
	run        func(call int, g comfy.Graph, o comfy.RunOptions) comfy.RunResult
	runErr     func(call int) error // non-nil error = what the Python client raised (Run's error return)
	aliveCalls int
	runCalls   int
	interrupts int
	frees      int
	lastOpt    comfy.RunOptions
	lastPrompt string // the cond node's prompt of the last graph run
}

func (f *fakeGPU) Alive() bool {
	f.mu.Lock()
	f.aliveCalls++
	n := f.aliveCalls
	fn := f.alive
	f.mu.Unlock()
	if fn == nil {
		return true
	}
	return fn(n)
}

func (f *fakeGPU) Run(ctx context.Context, g comfy.Graph, o comfy.RunOptions) (comfy.RunResult, error) {
	f.mu.Lock()
	f.runCalls++
	n := f.runCalls
	f.lastOpt = o
	if g.Has("cond") {
		f.lastPrompt, _ = g.Node("cond").Inputs.Get("prompt").(string)
	}
	fn, errFn := f.run, f.runErr
	f.mu.Unlock()
	if errFn != nil {
		if err := errFn(n); err != nil {
			return comfy.RunResult{}, err
		}
	}
	if ctx.Err() != nil {
		return comfy.RunResult{}, ctx.Err()
	}
	return fn(n, g, o), nil
}

func (f *fakeGPU) Interrupt() { f.mu.Lock(); f.interrupts++; f.mu.Unlock() }
func (f *fakeGPU) Free(bool, bool) {
	f.mu.Lock()
	f.frees++
	f.mu.Unlock()
}

type fakeLLM struct {
	mu    sync.Mutex
	alive func(call int) bool
	calls int
}

func (f *fakeLLM) Alive() bool {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.alive == nil {
		return true
	}
	return f.alive(n)
}

// memIndex is an in-memory index behind the loadIndex / appendIndex seams.
type memIndex struct {
	mu    sync.Mutex
	items []index.Item
	fail  error
}

func (m *memIndex) load() []index.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]index.Item(nil), m.items...)
}

func (m *memIndex) append(it index.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.items = append(m.items, it)
	return nil
}

func fixtureIndex(t *testing.T) []index.Item {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var items []index.Item
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 7 {
		t.Fatalf("fixture has %d items, want 7", len(items))
	}
	return items
}

// stubAll points every seam at test doubles and restores them afterwards. Timings are shortened.
func stubAll(t *testing.T) {
	t.Helper()
	saved := struct {
		loadIndex       func() []index.Item
		appendIndex     func(index.Item) error
		history         func(string, int) ([]programs.Episode, error)
		writeProgramme  func(*llmClientT, string, *materialT, float64, *int, logT, *randT) (*script.Programme, error)
		writeBreak      func(*llmClientT, string) (*script.Programme, error)
		materialFor     func(string, *randT) *materialT
		audioFilterArgs func(string) []string
		libDir          func() string
		scriptsDir      func() string
		rawDir          func() string
		now             func() time.Time
	}{loadIndex, appendIndex, history, writeProgramme, writeBreak, materialFor, audioFilterArgs, libDir, scriptsDir, rawDir, now}
	savedD := []time.Duration{comfyPoll, comfyRestartAfter, llmPoll, restartSettle, retrySleep, failureUnit, failureMax, normalizeTimeout}
	t.Cleanup(func() {
		loadIndex, appendIndex, history, writeProgramme, writeBreak = saved.loadIndex, saved.appendIndex, saved.history, saved.writeProgramme, saved.writeBreak
		materialFor, audioFilterArgs = saved.materialFor, saved.audioFilterArgs
		libDir, scriptsDir, rawDir, now = saved.libDir, saved.scriptsDir, saved.rawDir, saved.now
		comfyPoll, comfyRestartAfter, llmPoll, restartSettle = savedD[0], savedD[1], savedD[2], savedD[3]
		retrySleep, failureUnit, failureMax, normalizeTimeout = savedD[4], savedD[5], savedD[6], savedD[7]
	})
	loadIndex = func() []index.Item { return nil }
	appendIndex = func(index.Item) error { return nil }
	history = func(string, int) ([]programs.Episode, error) { return nil, nil }
	writeProgramme = func(*llmClientT, string, *materialT, float64, *int, logT, *randT) (*script.Programme, error) {
		return nil, errors.New("writeProgramme not stubbed")
	}
	writeBreak = func(*llmClientT, string) (*script.Programme, error) { return nil, errors.New("writeBreak not stubbed") }
	materialFor = func(string, *randT) *materialT { return nil }
	tmp := t.TempDir()
	lib, raw, scripts := filepath.Join(tmp, "library"), filepath.Join(tmp, "raw"), filepath.Join(tmp, "scripts")
	for _, d := range []string{lib, raw, scripts} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	libDir = func() string { return lib }
	rawDir = func() string { return raw }
	scriptsDir = func() string { return scripts }
	comfyPoll, comfyRestartAfter, llmPoll, restartSettle = time.Millisecond, 5*time.Millisecond, time.Millisecond, time.Millisecond
	retrySleep, failureUnit, failureMax = time.Millisecond, time.Millisecond, 5*time.Millisecond
}

// mustNew is New with nil clients.
func mustNew(t *testing.T, opt Options) *Producer {
	t.Helper()
	return New(nil, nil, opt)
}

// newTestProducer builds a producer around fakes (New with nil clients, then the fakes wired in).
func newTestProducer(t *testing.T, gpu *fakeGPU, wr *fakeLLM, opt Options) (*Producer, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	opt.Log = lb.logf
	seed := int64(7)
	opt.Seed = &seed
	p := mustNew(t, opt)
	p.gpu, p.comfyBase = gpu, "http://127.0.0.1:8188"
	p.writer, p.llmBase = wr, "http://127.0.0.1:11434"
	return p, lb
}

func haveFFmpeg(t *testing.T) {
	t.Helper()
	for _, exe := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(exe); err != nil {
			t.Skipf("%s not on PATH", exe)
		}
	}
}

// makeClip writes a 1 s 320x240 30 fps test clip with 44.1 kHz mono audio (nothing like a library clip).
func makeClip(t *testing.T, path string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100",
		"-t", "1", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-ac", "1", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg test clip: %v\n%s", err, out)
	}
}

type probe struct {
	Width, Height int
	FrameRate     string
	SampleRate    string
	Channels      int
	VCodec        string
	ACodec        string
}

func ffprobe(t *testing.T, path string) probe {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var doc struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			RFrameRate string `json:"r_frame_rate"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var pr probe
	for _, s := range doc.Streams {
		switch s.CodecType {
		case "video":
			pr.Width, pr.Height, pr.FrameRate, pr.VCodec = s.Width, s.Height, s.RFrameRate, s.CodecName
		case "audio":
			pr.SampleRate, pr.Channels, pr.ACodec = s.SampleRate, s.Channels, s.CodecName
		}
	}
	return pr
}

// renderingGPU is a fake whose Run "renders" by copying src into the raw dir (ComfyUI's output folder).
func renderingGPU(t *testing.T, src string, seconds float64) *fakeGPU {
	t.Helper()
	return &fakeGPU{run: func(call int, g comfy.Graph, o comfy.RunOptions) comfy.RunResult {
		out := filepath.Join(rawDir(), fmt.Sprintf("seg_%05d_.mp4", call))
		if err := copyFile(src, out); err != nil {
			t.Errorf("fake render: %v", err)
			return comfy.RunResult{Err: err.Error()}
		}
		return comfy.RunResult{PromptID: fmt.Sprint(call), OK: true, Seconds: seconds, LocalFiles: []string{out}}
	}}
}

func testProgramme(id, format, title string, slot *int, captions ...string) *script.Programme {
	prog := &script.Programme{ID: id, Format: format, Slot: slot, Title: title, Minutes: 1, Sources: []string{},
		Written: "2026-09-16T20:15:04"}
	plan := "plan"
	sec := script.Section{Name: "本編", Plan: &plan}
	for _, c := range captions {
		sec.Segments = append(sec.Segments, script.Segment{Caption: c, Seconds: 9, Prompt: "integrated_multimodal_description: " + c})
		sec.Seconds += 9
	}
	prog.Sections = []script.Section{sec}
	prog.SegmentsTotal = len(captions)
	prog.SecondsTotal = sec.Seconds
	return prog
}

func intp(i int) *int { return &i }

// ---- ResumeSlot / Unfinished --------------------------------------------------------------------------

func TestResumeSlot(t *testing.T) {
	stubAll(t)
	items := fixtureIndex(t)
	loadIndex = func() []index.Item { return items }
	cases := []struct {
		lineup []string
		want   int
	}{
		{nil, 5}, // programs.Lineup: newest slotted item is slot 4 nature -> 5
		{[]string{"nature", "cooking"}, 1},
		{[]string{"cooking", "nature"}, 0},
		{[]string{"weather", "nature", "cm"}, 2},
	}
	for _, c := range cases {
		p := mustNew(t, Options{Lineup: c.lineup, Log: (&logBuf{}).logf})
		if got := p.ResumeSlot(); got != c.want {
			t.Errorf("ResumeSlot(%v) = %d, want %d", c.lineup, got, c.want)
		}
		if p.slot != c.want {
			t.Errorf("New: slot = %d, want %d", p.slot, c.want)
		}
	}
	loadIndex = func() []index.Item { return nil }
	if got := mustNew(t, Options{Log: (&logBuf{}).logf}).ResumeSlot(); got != 0 {
		t.Errorf("empty index: ResumeSlot = %d, want 0", got)
	}
}

func TestUnfinished(t *testing.T) {
	stubAll(t)
	items := fixtureIndex(t)
	loadIndex = func() []index.Item { return items }
	scriptsDir = func() string { return filepath.Join("testdata", "scripts") }
	p := mustNew(t, Options{Log: (&logBuf{}).logf})

	prog := p.Unfinished()
	if prog == nil {
		t.Fatal("Unfinished = nil, want the nature programme")
	}
	if prog.ID != fixturePID || prog.Title != fixtureTitle || prog.Format != "nature" || prog.SegmentsTotal != 5 {
		t.Errorf("got id=%s title=%s format=%s total=%d", prog.ID, prog.Title, prog.Format, prog.SegmentsTotal)
	}
	if prog.Slot == nil || *prog.Slot != 4 {
		t.Errorf("slot = %v, want 4", prog.Slot)
	}
	if len(prog.Done) != 2 || !prog.Done[0] || !prog.Done[1] {
		t.Errorf("Done = %v, want {0,1}", prog.Done)
	}
	segs := prog.Segments()
	if len(segs) != 5 || segs[3].Section != "風の通り道" || segs[3].Segment.Caption != "clip 3" {
		t.Errorf("segments = %+v", segs)
	}

	// The newest programme is complete (all 5 indices present): nothing to resume, whatever came before.
	full := append([]index.Item(nil), items...)
	for i := 2; i < 5; i++ {
		it := items[len(items)-1]
		it.SegmentIndex = intp(i)
		full = append(full, it)
	}
	loadIndex = func() []index.Item { return full }
	if got := p.Unfinished(); got != nil {
		t.Errorf("complete programme: Unfinished = %v, want nil", got)
	}

	// A complete break as the newest entry decides too (the walk stops at the first programme_id).
	loadIndex = func() []index.Item { return items[:5] }
	if got := p.Unfinished(); got != nil {
		t.Errorf("newest is a complete cm: Unfinished = %v, want nil", got)
	}

	// segments_total 0 (unknown) -> nil.
	zero := append([]index.Item(nil), items...)
	zero[len(zero)-1].SegmentsTotal = 0
	loadIndex = func() []index.Item { return zero }
	if got := p.Unfinished(); got != nil {
		t.Errorf("segments_total 0: Unfinished = %v, want nil", got)
	}

	// The script file is missing -> nil.
	loadIndex = func() []index.Item { return items }
	scriptsDir = func() string { return t.TempDir() }
	if got := p.Unfinished(); got != nil {
		t.Errorf("missing script: Unfinished = %v, want nil", got)
	}

	// Only old-library entries -> nil.
	loadIndex = func() []index.Item { return items[:1] }
	if got := p.Unfinished(); got != nil {
		t.Errorf("old library: Unfinished = %v, want nil", got)
	}
}

// ---- Normalize / SweepRaw ---------------------------------------------------------------------------

func TestNormalize(t *testing.T) {
	haveFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "raw.mp4")
	makeClip(t, src)
	dst := filepath.Join(dir, "lib.mp4")
	if err := Normalize(src, dst, LibWidth, LibHeight); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	pr := ffprobe(t, dst)
	want := probe{Width: 832, Height: 480, FrameRate: "24/1", SampleRate: "48000", Channels: 2, VCodec: "h264", ACodec: "aac"}
	if pr != want {
		t.Errorf("normalised clip = %+v, want %+v", pr, want)
	}
	if err := Normalize(filepath.Join(dir, "missing.mp4"), filepath.Join(dir, "x.mp4"), LibWidth, LibHeight); err == nil {
		t.Error("Normalize of a missing file: no error")
	} else if !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("error should name ffmpeg: %v", err)
	}
}

func TestSweepRaw(t *testing.T) {
	stubAll(t)
	dir := rawDir()
	old := filepath.Join(dir, "seg_00001_.mp4")
	fresh := filepath.Join(dir, "seg_00002_.mp4")
	for _, f := range []string{old, fresh} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, past, past); err != nil {
		t.Fatal(err)
	}
	lb := &logBuf{}
	if n := SweepRaw(time.Hour, lb.logf); n != 1 {
		t.Errorf("SweepRaw = %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old file still there")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file removed")
	}
	if _, err := os.Stat(sub); err != nil {
		t.Error("directory removed")
	}
	if got := lb.all(); len(got) != 1 || got[0] != "[aitv] swept 1 raw render(s) from output/aitv" {
		t.Errorf("log = %q", got)
	}
	lb = &logBuf{}
	if n := SweepRaw(time.Hour, lb.logf); n != 0 || len(lb.all()) != 0 {
		t.Errorf("second sweep: n=%d log=%q", n, lb.all())
	}
	rawDir = func() string { return filepath.Join(dir, "does-not-exist") }
	if n := SweepRaw(0, nil); n != 0 {
		t.Errorf("missing dir: SweepRaw = %d", n)
	}
}

// ---- RenderSegment / RenderProgramme ------------------------------------------------------------

var uidRe = regexp.MustCompile(`^\d{8}-\d{6}-2e47cd-03$`)

func TestRenderSegment(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	t.Setenv("AITV_NO_YOMI", "1") // the prompt reaches H3 as written; TestRenderSegmentKana covers the conversion
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	gpu := renderingGPU(t, src, 346.84)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{Width: 640, Height: 352, Steps: 8, Turbo: true})

	prog := testProgramme(fixturePID, "nature", fixtureTitle, intp(4), "c0", "c1", "c2", "c3", "c4")
	prog.Sources = []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9", "s10"}
	seg := script.Segment{Caption: "風にゆれる竹", Seconds: 9, Prompt: "integrated_multimodal_description: <d>[Japanese] 竹。</d>"}
	before := time.Now()
	item, err := p.RenderSegment(context.Background(), prog, 3, "風の通り道", seg)
	if err != nil || item == nil {
		t.Fatalf("RenderSegment = %v, %v; log: %v", item, err, lb.all())
	}
	if !uidRe.MatchString(item.ID) {
		t.Errorf("id = %q", item.ID)
	}
	if item.File != "nature-"+item.ID+".mp4" {
		t.Errorf("file = %q", item.File)
	}
	if _, err := os.Stat(filepath.Join(libDir(), item.File)); err != nil {
		t.Errorf("library clip missing: %v", err)
	}
	if raw := gpu.lastOpt; raw.StallTimeout != 1200*time.Second || raw.Log == nil || raw.Timeout != 0 {
		t.Errorf("RunOptions = %+v", raw)
	}
	if entries, _ := os.ReadDir(rawDir()); len(entries) != 0 {
		t.Errorf("raw output not deleted: %d files", len(entries))
	}
	if item.Format != "nature" || item.Program != fixtureTitle || item.ProgrammeID != fixturePID || item.Section != "風の通り道" ||
		item.Caption != seg.Caption || item.Prompt != seg.Prompt || item.SegmentsTotal != 5 {
		t.Errorf("item = %+v", item)
	}
	if item.Slot == nil || *item.Slot != 4 || item.SegmentIndex == nil || *item.SegmentIndex != 3 {
		t.Errorf("slot/segment_index = %v/%v", item.Slot, item.SegmentIndex)
	}
	// 9 s -> round(216) = 216 frames -> +10 on the 17k+5 grid = 226 -> 9.4166 s (h3gen.graph.snap_frames).
	if want := 226.0 / 24.0; item.Seconds != want {
		t.Errorf("seconds = %v, want %v", item.Seconds, want)
	}
	if item.Seed < 1 || item.Seed >= 1<<31 {
		t.Errorf("seed = %d", item.Seed)
	}
	if item.RenderS != 346.8 {
		t.Errorf("render_s = %v, want 346.8", item.RenderS)
	}
	created, err := time.ParseInLocation(timeLayout, item.Created, time.Local)
	if err != nil || created.Before(before.Truncate(time.Second)) || created.After(time.Now()) {
		t.Errorf("created = %q (%v)", item.Created, err)
	}
	if len(item.Sources) != 8 || item.Sources[7] != "s8" {
		t.Errorf("sources = %v", item.Sources)
	}
	want := fmt.Sprintf("[aitv] + %s  (9.4s, rendered in 347s) [%s / 風の通り道 4/5: 風にゆれる竹]", item.File, fixtureTitle)
	if got := lb.all(); len(got) != 1 || got[0] != want {
		t.Errorf("log = %q\nwant %q", got, want)
	}

	// No sources -> the key stays absent.
	prog.Sources = nil
	if it, err := p.RenderSegment(context.Background(), prog, 0, "本編", seg); err != nil || it == nil || it.Sources != nil {
		t.Errorf("sources should be nil: %+v (%v)", it, err)
	}

	// A failed run: no item, one log line, nothing written.
	gpu.run = func(int, comfy.Graph, comfy.RunOptions) comfy.RunResult {
		return comfy.RunResult{Err: "SamplerCustomAdvanced: HIP error"}
	}
	lb.lines = nil
	if it, err := p.RenderSegment(context.Background(), prog, 0, "本編", seg); it != nil || err != nil {
		t.Errorf("failed render returned %+v, %v", it, err)
	}
	if got := lb.all(); len(got) != 1 || got[0] != "[aitv] render failed: SamplerCustomAdvanced: HIP error" {
		t.Errorf("log = %q", got)
	}
	gpu.run = func(int, comfy.Graph, comfy.RunOptions) comfy.RunResult { return comfy.RunResult{OK: true} } // ok but no files
	if it, err := p.RenderSegment(context.Background(), prog, 0, "本編", seg); it != nil || err != nil {
		t.Errorf("render without files returned %+v, %v", it, err)
	}

	// What the Python client raised (a refused websocket, a rejected graph) is returned as an error, not
	// logged as a failed render: aitv/producer.py render_segment had no try around comfy.run.
	lb.lines = nil
	gpu.runErr = func(int) error { return errors.New("websocket connect: dial tcp 127.0.0.1:1: connectex: refused") }
	it, err := p.RenderSegment(context.Background(), prog, 0, "本編", seg)
	if it != nil || err == nil || !strings.HasPrefix(err.Error(), "websocket connect: ") {
		t.Errorf("RenderSegment = %+v, %v; want the client error", it, err)
	}
	if got := lb.all(); len(got) != 0 {
		t.Errorf("a raised client error must not be logged as a render failure: %q", got)
	}
}

func TestRenderSegmentNormalizeFallback(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	gpu := renderingGPU(t, src, 10)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{})
	audioFilterArgs = func(string) []string { return []string{"-af", "nosuchfilter=1"} } // makes ffmpeg fail
	prog := testProgramme("20260916-201504-abcdef", "cm", "CM", nil, "c0")
	item, err := p.RenderSegment(context.Background(), prog, 0, "CM", prog.Sections[0].Segments[0])
	if err != nil || item == nil {
		t.Fatalf("RenderSegment = %v, %v; log: %v", item, err, lb.all())
	}
	lb.mustHave(t, "[aitv] ffmpeg normalize failed (ffmpeg: ", "); copying raw")
	dst := filepath.Join(libDir(), item.File)
	a, _ := os.ReadFile(src)
	b, err := os.ReadFile(dst)
	if err != nil || string(a) != string(b) {
		t.Errorf("library file is not the raw copy (%v)", err)
	}
	if entries, _ := os.ReadDir(rawDir()); len(entries) != 0 {
		t.Errorf("raw output not deleted after copy: %d files", len(entries))
	}
	if item.Slot != nil {
		t.Errorf("break slot = %v, want nil", *item.Slot)
	}
}

func TestRenderProgrammeEscalation(t *testing.T) {
	stubAll(t)
	restarts := 0
	gpu := &fakeGPU{run: func(int, comfy.Graph, comfy.RunOptions) comfy.RunResult { return comfy.RunResult{Err: "boom"} }}
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{RestartComfy: func() error { restarts++; return errors.New("no launcher") }})
	prog := testProgramme("20260916-201504-abcdef", "nature", "森", intp(4), "0", "1", "2", "3", "4", "5", "6", "7")
	made, failed, err := p.RenderProgramme(context.Background(), prog)
	if made != 0 || failed != 6 || err != nil {
		t.Errorf("made=%d failed=%d err=%v, want 0/6/nil", made, failed, err)
	}
	if gpu.runCalls != 6 || gpu.interrupts != 6 || gpu.frees != 6 {
		t.Errorf("runs=%d interrupts=%d frees=%d, want 6 each", gpu.runCalls, gpu.interrupts, gpu.frees)
	}
	if restarts != 3 { // failures 4, 5, 6
		t.Errorf("restarts = %d, want 3", restarts)
	}
	lb.mustHave(t,
		"[aitv] render failed: boom",
		"[aitv] 1 consecutive failure(s): interrupting and freeing models",
		"[aitv] 4 consecutive failure(s): interrupting and freeing models",
		"[aitv] restarting ComfyUI",
		"[aitv] restart failed: no launcher",
		"[aitv] giving up on 森 after 6 failed renders")
	if n := lb.count("[aitv] restarting ComfyUI"); n != 3 {
		t.Errorf("restarting logged %d times, want 3", n)
	}

	// Done indices are skipped; a failed index write keeps the clip and the count.
	haveFFmpeg(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	gpu2 := renderingGPU(t, src, 1)
	mem := &memIndex{fail: errors.New("disk full")}
	appendIndex = mem.append
	p2, lb2 := newTestProducer(t, gpu2, &fakeLLM{}, Options{})
	prog2 := testProgramme("20260916-201504-abcdef", "nature", "森", intp(4), "0", "1", "2")
	prog2.Done = map[int]bool{0: true, 2: true}
	made, failed, err = p2.RenderProgramme(context.Background(), prog2)
	if made != 1 || failed != 0 || err != nil || gpu2.runCalls != 1 {
		t.Errorf("made=%d failed=%d err=%v runs=%d, want 1/0/nil/1", made, failed, err, gpu2.runCalls)
	}
	lb2.mustHave(t, "[aitv] index write failed (disk full); clip kept on disk as nature-")

	// Cancellation stops the loop between clips.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gpu2.runCalls = 0
	if made, failed, err := p2.RenderProgramme(ctx, testProgramme("x", "nature", "森", nil, "0", "1")); made != 0 || failed != 0 || err != nil || gpu2.runCalls != 0 {
		t.Errorf("cancelled: made=%d failed=%d err=%v runs=%d", made, failed, err, gpu2.runCalls)
	}
}

// A client error (what h3gen/client.py raised: ws.connect, queue()) abandons the programme at once, with
// no recover_comfy escalation: no interrupt, no /free, no ComfyUI restart, no "render failed" line — the
// Python render_programme had no try around render_segment, so the exception went straight to run().
func TestRenderProgrammeClientErrorAbandons(t *testing.T) {
	stubAll(t)
	restarts := 0
	gpu := &fakeGPU{
		run: func(int, comfy.Graph, comfy.RunOptions) comfy.RunResult { return comfy.RunResult{OK: true} },
		runErr: func(int) error {
			return errors.New(`{"error": {"type": "invalid_prompt", "message": "Cannot execute because a node is missing the class_type property."}}`)
		},
	}
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{RestartComfy: func() error { restarts++; return nil }})
	prog := testProgramme("20260916-201504-abcdef", "nature", "森", intp(4), "0", "1", "2", "3", "4", "5", "6", "7")
	made, failed, err := p.RenderProgramme(context.Background(), prog)
	if made != 0 || failed != 0 || err == nil || !strings.Contains(err.Error(), "invalid_prompt") {
		t.Errorf("made=%d failed=%d err=%v, want 0/0/the queue error", made, failed, err)
	}
	if gpu.runCalls != 1 || gpu.interrupts != 0 || gpu.frees != 0 || restarts != 0 {
		t.Errorf("runs=%d interrupts=%d frees=%d restarts=%d, want 1/0/0/0", gpu.runCalls, gpu.interrupts, gpu.frees, restarts)
	}
	if lb.has("render failed") || lb.has("consecutive failure") || lb.has("giving up") {
		t.Errorf("escalation logged for a client error: %q", lb.all())
	}
}

// ---- health ---------------------------------------------------------------------------------------

func TestWaitForComfy(t *testing.T) {
	stubAll(t)
	restarts := 0
	gpu := &fakeGPU{alive: func(call int) bool { return call >= 12 }}
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{RestartComfy: func() error { restarts++; return nil }})
	p.WaitForComfy(context.Background())
	if restarts != 1 {
		t.Errorf("restarts = %d, want exactly 1", restarts)
	}
	want := []string{"[aitv] ComfyUI at http://127.0.0.1:8188 not reachable; waiting...", "[aitv] ComfyUI still down; relaunching it", "[aitv] ComfyUI is back"}
	if got := lb.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("log = %q\nwant %q", got, want)
	}

	// Already up: silent. No restart hook: only the two lines.
	gpu2 := &fakeGPU{alive: func(call int) bool { return call != 1 }}
	p2, lb2 := newTestProducer(t, gpu2, &fakeLLM{}, Options{})
	p2.WaitForComfy(context.Background())
	p2.WaitForComfy(context.Background())
	if got := lb2.all(); len(got) != 2 || got[1] != "[aitv] ComfyUI is back" {
		t.Errorf("log = %q", got)
	}

	// Down for good: returns on cancellation.
	gpu3 := &fakeGPU{alive: func(int) bool { return false }}
	p3, _ := newTestProducer(t, gpu3, &fakeLLM{}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { p3.WaitForComfy(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForComfy did not return after cancellation")
	}
}

func TestWaitForLLM(t *testing.T) {
	stubAll(t)
	wr := &fakeLLM{alive: func(call int) bool { return call >= 4 }}
	p, lb := newTestProducer(t, &fakeGPU{}, wr, Options{})
	p.WaitForLLM(context.Background())
	want := []string{"[aitv] writer LLM at http://127.0.0.1:11434 not answering; waiting...", "[aitv] writer LLM is back"}
	if got := lb.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("log = %q\nwant %q", got, want)
	}
	p.WaitForLLM(context.Background())
	if len(lb.all()) != 2 {
		t.Errorf("an answering LLM should log nothing: %q", lb.all())
	}
}

// ---- loop -----------------------------------------------------------------------------------------

type writerCall struct {
	fmtKey string
	slot   int
}

func TestRun(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)

	var mu sync.Mutex
	var calls []writerCall
	var breaks []string
	prefetchStarted := make(chan struct{})
	writeProgramme = func(_ *llmClientT, fmtKey string, m *materialT, minutes float64, slot *int, log logT, rng *randT) (*script.Programme, error) {
		mu.Lock()
		calls = append(calls, writerCall{fmtKey, *slot})
		n := len(calls)
		mu.Unlock()
		if minutes != 1 || rng == nil || log == nil || m != nil {
			t.Errorf("writeProgramme args: minutes=%v rng=%v log=%v material=%v", minutes, rng != nil, log != nil, m)
		}
		if *slot == 1 {
			close(prefetchStarted)
		}
		s := *slot
		return testProgramme(fmt.Sprintf("20260917-0000%02d-%06d", n, n), fmtKey, fmt.Sprintf("番組%d", n), &s, "a", "b"), nil
	}
	writeBreak = func(_ *llmClientT, fmtKey string) (*script.Programme, error) {
		mu.Lock()
		breaks = append(breaks, fmtKey)
		mu.Unlock()
		return testProgramme("20260917-000100-bbbbbb", fmtKey, "ブレイク "+fmtKey, nil, "x"), nil
	}
	mem := &memIndex{}
	loadIndex, appendIndex = mem.load, mem.append
	gpu := renderingGPU(t, src, 2)
	inner := gpu.run
	gpu.run = func(call int, g comfy.Graph, o comfy.RunOptions) comfy.RunResult {
		if call == 1 { // the next script must already be in the making while the first clip renders
			select {
			case <-prefetchStarted:
			case <-time.After(5 * time.Second):
				t.Error("slot 2 was not prefetched while slot 1 rendered")
			}
		}
		return inner(call, g, o)
	}
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{Minutes: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p.Run(ctx, intp(2), []string{"nature", "travel"})

	if want := []writerCall{{"nature", 0}, {"travel", 1}}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("writeProgramme calls = %v, want %v", calls, want)
	}
	if want := []string{"cm", "ident"}; fmt.Sprint(breaks) != fmt.Sprint(want) {
		t.Errorf("writeBreak calls = %v, want %v", breaks, want)
	}
	items := mem.load()
	if len(items) != 6 {
		t.Fatalf("index has %d items, want 6 (2+1+2+1)", len(items))
	}
	wantFmt := []string{"nature", "nature", "cm", "travel", "travel", "ident"}
	for i, it := range items {
		if it.Format != wantFmt[i] {
			t.Errorf("item %d format = %s, want %s", i, it.Format, wantFmt[i])
		}
		if _, err := os.Stat(filepath.Join(libDir(), it.File)); err != nil {
			t.Errorf("item %d: clip missing: %v", i, err)
		}
	}
	if items[0].Slot == nil || *items[0].Slot != 0 || items[3].Slot == nil || *items[3].Slot != 1 || items[2].Slot != nil {
		t.Errorf("slots = %v %v %v", items[0].Slot, items[3].Slot, items[2].Slot)
	}
	lb.mustHave(t,
		"[aitv] === slot 1/2: ",
		"[aitv] === slot 2/2: ",
		"[aitv] === done: 番組1 (2 clips, 0 failed)",
		"[aitv] === done: 番組2 (2 clips, 0 failed)",
		"[ブレイク cm / 本編 1/1: x]",
		"[ブレイク ident / 本編 1/1: x]")
	if lb.has("=== done: ブレイク") {
		t.Errorf("a break has no === done line: %q", lb.all())
	}
	for _, s := range lb.all() {
		if strings.HasPrefix(s, "[aitv] === slot ") && !strings.HasSuffix(s, " (1 min)") {
			t.Errorf("slot line should end with the minutes: %q", s)
		}
	}
	if p.slot != 0 {
		t.Errorf("slot after two of two = %d, want 0", p.slot)
	}
	if p.prefetch != nil {
		t.Error("a prefetch was started for a programme beyond count")
	}
}

func TestRunResume(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	mem := &memIndex{items: fixtureIndex(t)}
	loadIndex, appendIndex = mem.load, mem.append
	scriptsDir = func() string { return filepath.Join("testdata", "scripts") }
	var calls []writerCall
	writeProgramme = func(_ *llmClientT, fmtKey string, _ *materialT, _ float64, slot *int, _ logT, _ *randT) (*script.Programme, error) {
		calls = append(calls, writerCall{fmtKey, *slot})
		s := *slot
		return testProgramme("20260917-000001-000001", fmtKey, "次の番組", &s, "a"), nil
	}
	writeBreak = func(_ *llmClientT, fmtKey string) (*script.Programme, error) {
		return testProgramme("20260917-000100-bbbbbb", fmtKey, "ブレイク", nil, "x"), nil
	}
	gpu := renderingGPU(t, src, 1)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{Minutes: 1})
	if p.slot != 5 {
		t.Fatalf("resumed slot = %d, want 5", p.slot)
	}
	p.Run(context.Background(), intp(1), nil)

	lb.mustHave(t,
		"[aitv] === resuming: "+fixtureTitle+" (3 clips left)",
		"[aitv] === done: "+fixtureTitle+" (3 clips, 0 failed)",
		"[aitv] === slot 6/8: ",
		"[aitv] === done: 次の番組 (1 clips, 0 failed)")
	if want := []writerCall{{"cooking", 5}}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("writeProgramme calls = %v, want %v", calls, want)
	}
	items := mem.load()
	if len(items) != 7+3+1+1 {
		t.Fatalf("index has %d items, want 12", len(items))
	}
	for i, wantIdx := range []int{2, 3, 4} { // the resumed clips, in air order, under the old programme id
		it := items[7+i]
		if it.ProgrammeID != fixturePID || it.SegIndex() != wantIdx || it.Slot == nil || *it.Slot != 4 || it.SegmentsTotal != 5 {
			t.Errorf("resumed item %d = %+v", i, it)
		}
	}
	if items[7].Section != "夜明けの竹林" || items[8].Section != "風の通り道" {
		t.Errorf("sections = %q %q", items[7].Section, items[8].Section)
	}
	if p.slot != 6 {
		t.Errorf("slot after the run = %d, want 6", p.slot)
	}

	// With formats given there is no resume at all.
	lb.lines = nil
	mem.items = fixtureIndex(t)
	p.Run(context.Background(), intp(1), []string{"travel"})
	if lb.has("resuming") {
		t.Error("formats given: must not resume")
	}
	if len(mem.load()) != 7+1+1 {
		t.Errorf("index has %d items, want 9", len(mem.load()))
	}
}

func TestRunFailuresAndSkips(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	mem := &memIndex{}
	loadIndex, appendIndex = mem.load, mem.append

	// Two throw-away formats: one needs material (none is available), one has a minimum gap.
	programs.Formats["zz_mat"] = programs.Format{Key: "zz_mat", Title: "素材", Minutes: 1, NeedsMaterial: true}
	programs.Formats["zz_gap"] = programs.Format{Key: "zz_gap", Title: "間隔", Minutes: 1, MinGapHours: 5}
	t.Cleanup(func() { delete(programs.Formats, "zz_mat"); delete(programs.Formats, "zz_gap") })
	history = func(fmtKey string, n int) ([]programs.Episode, error) {
		if fmtKey != "zz_gap" || n != 1 {
			t.Errorf("history(%q, %d)", fmtKey, n)
		}
		return []programs.Episode{{Written: time.Now().Add(-90 * time.Minute).Format(timeLayout), Title: "前回"}}, nil
	}
	var calls []writerCall
	writeProgramme = func(_ *llmClientT, fmtKey string, _ *materialT, _ float64, slot *int, _ logT, _ *randT) (*script.Programme, error) {
		calls = append(calls, writerCall{fmtKey, *slot})
		switch len(calls) {
		case 1:
			return nil, errors.New("LLM did not return JSON: nope")
		case 2:
			panic("TODO")
		}
		s := *slot
		return testProgramme("20260917-000001-000001", fmtKey, "やっと", &s, "a"), nil
	}
	writeBreak = func(_ *llmClientT, fmtKey string) (*script.Programme, error) { return nil, errors.New("no break") }
	gpu := renderingGPU(t, src, 1)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{})

	// Lineup: error, panic, no material, too soon, success -> 3 attempts counted (skips count, failures do not).
	p.Run(context.Background(), intp(3), []string{"nature", "travel", "zz_mat", "zz_gap", "cooking"})
	if want := []writerCall{{"nature", 0}, {"travel", 1}, {"cooking", 4}}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("writeProgramme calls = %v, want %v", calls, want)
	}
	lb.mustHave(t,
		"[aitv] programme failed (LLM did not return JSON: nope); retrying in 60s",
		"[aitv] programme failed (panic: TODO); retrying in 60s",
		"[aitv] no real material for zz_mat (offline?); slot 3 skipped this round",
		"[aitv] last zz_gap was written 1.5 h ago (< 5 h): the material would be the same; slot 4 skipped this round",
		"[aitv] === slot 5/5: ",
		"[aitv] === done: やっと (1 clips, 0 failed)",
		"[aitv] cm script failed (no break); skipped")
	if got := lb.count("=== slot "); got != 5 {
		t.Errorf("slot lines = %d, want 5", got)
	}
	if len(mem.load()) != 1 {
		t.Errorf("index has %d items, want 1", len(mem.load()))
	}
	if p.slot != 0 {
		t.Errorf("slot = %d, want 0 (wrapped)", p.slot)
	}
	if p.breakToggle != 1 {
		t.Errorf("breakToggle = %d, want 1", p.breakToggle)
	}
}

func TestRunPrefetchFailureAndCancel(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	mem := &memIndex{}
	loadIndex, appendIndex = mem.load, mem.append
	var mu sync.Mutex
	var calls []writerCall
	writeProgramme = func(_ *llmClientT, fmtKey string, _ *materialT, _ float64, slot *int, _ logT, _ *randT) (*script.Programme, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, writerCall{fmtKey, *slot})
		if *slot == 1 {
			return nil, errors.New("outline without sections")
		}
		s := *slot
		return testProgramme(fmt.Sprintf("20260917-0000%02d-000001", len(calls)), fmtKey, "本編"+fmtKey, &s, "a"), nil
	}
	writeBreak = func(_ *llmClientT, fmtKey string) (*script.Programme, error) {
		return testProgramme("20260917-000100-bbbbbb", fmtKey, "ブレイク", nil, "x"), nil
	}
	gpu := renderingGPU(t, src, 1)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{})
	p.Run(context.Background(), intp(2), []string{"nature", "travel"})
	lb.mustHave(t, "[aitv] writing slot 2 failed (outline without sections)", "[aitv] === slot 2/2: ")
	if lb.has("programme failed") {
		t.Errorf("a prefetch failure is not a programme failure: %q", lb.all())
	}
	mu.Lock()
	if want := []writerCall{{"nature", 0}, {"travel", 1}}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("writeProgramme calls = %v, want %v", calls, want)
	}
	mu.Unlock()
	if len(mem.load()) != 2 { // nature clip + its cm; travel was skipped
		t.Errorf("index has %d items, want 2", len(mem.load()))
	}

	// A cancelled context ends a forever run promptly, without a "programme failed" line.
	lb.lines = nil
	ctx, cancel := context.WithCancel(context.Background())
	gpu.runErr = func(int) error { // the render is interrupted by Ctrl+C: comfy.Run returns ctx.Err()
		cancel()
		return context.Canceled
	}
	done := make(chan struct{})
	go func() { p.Run(ctx, nil, []string{"nature"}); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if lb.has("programme failed") {
		t.Errorf("cancellation logged as a failure: %q", lb.all())
	}
}

func TestProduceBreakAlternates(t *testing.T) {
	stubAll(t)
	var got []string
	writeBreak = func(_ *llmClientT, fmtKey string) (*script.Programme, error) {
		got = append(got, fmtKey)
		return nil, errors.New("x")
	}
	p, lb := newTestProducer(t, &fakeGPU{}, &fakeLLM{}, Options{})
	for i := 0; i < 3; i++ {
		if err := p.ProduceBreak(context.Background()); err != nil {
			t.Errorf("ProduceBreak: %v", err)
		}
	}
	if fmt.Sprint(got) != "[cm ident cm]" {
		t.Errorf("break formats = %v", got)
	}
	lb.mustHave(t, "[aitv] cm script failed (x); skipped", "[aitv] ident script failed (x); skipped")
}

// A client error during a programme (or its break) is one "programme failed" line, the slot advances and
// the loop sleeps 60 s (aitv/producer.py run(): the raise lands in the programme-level except); during the
// resume it is "resume failed (...); moving on". No escalation, no ComfyUI restart, no 60 s retry on top.
func TestRunClientErrorIsProgrammeFailure(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	mem := &memIndex{items: fixtureIndex(t)}
	loadIndex, appendIndex = mem.load, mem.append
	scriptsDir = func() string { return filepath.Join("testdata", "scripts") }
	var calls []writerCall
	writeProgramme = func(_ *llmClientT, fmtKey string, _ *materialT, _ float64, slot *int, _ logT, _ *randT) (*script.Programme, error) {
		calls = append(calls, writerCall{fmtKey, *slot})
		s := *slot
		return testProgramme(fmt.Sprintf("20260917-0000%02d-000001", len(calls)), fmtKey, "番組"+fmtKey, &s, "a", "b"), nil
	}
	writeBreak = func(_ *llmClientT, fmtKey string) (*script.Programme, error) {
		return testProgramme("20260917-000100-bbbbbb", fmtKey, "ブレイク", nil, "x"), nil
	}
	restarts := 0
	gpu := renderingGPU(t, src, 1)
	// call 1 (the resume) raises; call 2 renders; call 3 (second clip of the programme) raises;
	// calls 4-5 render (the next programme); call 6 (its break) raises.
	gpu.runErr = func(call int) error {
		if call == 1 || call == 3 || call == 6 {
			return errors.New("websocket connect: dial tcp 127.0.0.1:8188: connectex: refused")
		}
		return nil
	}
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{Minutes: 1, RestartComfy: func() error { restarts++; return nil }})
	t0 := time.Now()
	p.Run(context.Background(), intp(2), nil)
	if el := time.Since(t0); el > 30*time.Second {
		t.Errorf("Run took %v: the failure back-offs must not run for a client error", el)
	}
	lb.mustHave(t,
		"[aitv] === resuming: "+fixtureTitle+" (3 clips left)",
		"[aitv] resume failed (websocket connect: dial tcp 127.0.0.1:8188: connectex: refused); moving on",
		"[aitv] === slot 6/8: ",
		"[aitv] programme failed (websocket connect: dial tcp 127.0.0.1:8188: connectex: refused); retrying in 60s",
		"[aitv] === slot 7/8: ",
		"[aitv] === done: 番組travel (2 clips, 0 failed)")
	if lb.has("render failed") || lb.has("consecutive failure") || lb.has("=== done: "+fixtureTitle) || lb.has("=== done: 番組cooking") {
		t.Errorf("log: %q", lb.all())
	}
	if n := lb.count("programme failed"); n != 2 { // the programme, then its break
		t.Errorf("programme failed logged %d times, want 2: %q", n, lb.all())
	}
	if gpu.runCalls != 6 || gpu.interrupts != 0 || gpu.frees != 0 || restarts != 0 {
		t.Errorf("runs=%d interrupts=%d frees=%d restarts=%d, want 6/0/0/0", gpu.runCalls, gpu.interrupts, gpu.frees, restarts)
	}
	if want := []writerCall{{"cooking", 5}, {"travel", 6}}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("writeProgramme calls = %v, want %v", calls, want)
	}
	if p.slot != 7 {
		t.Errorf("slot = %d, want 7", p.slot)
	}
	if got := len(mem.load()); got != 7+1+2 { // one clip of 番組cooking, both of 番組travel, no breaks
		t.Errorf("index has %d items, want 10", got)
	}
}

// count: nil = forever (Python None), 0 or negative = no slot at all (the Python loop condition
// `count is None or n < count` is false at once), the resume still runs first.
func TestRunCountSemantics(t *testing.T) {
	stubAll(t)
	mem := &memIndex{}
	loadIndex, appendIndex = mem.load, mem.append
	var calls int
	writeProgramme = func(_ *llmClientT, fmtKey string, _ *materialT, _ float64, slot *int, _ logT, _ *randT) (*script.Programme, error) {
		calls++
		return nil, nil // skipped slot: counted, then a 60 s (here 0) sleep
	}
	gpu := &fakeGPU{}
	llm := &fakeLLM{}
	p, lb := newTestProducer(t, gpu, llm, Options{})
	for _, n := range []int{0, -1} {
		calls = 0
		lb.lines = nil
		p.Run(context.Background(), intp(n), []string{"nature"})
		if calls != 0 || len(lb.all()) != 0 || p.slot != 0 {
			t.Errorf("count %d: writer called %d times, %d log lines, slot %d; want nothing", n, calls, len(lb.all()), p.slot)
		}
	}
	calls = 0
	p.Run(context.Background(), intp(2), []string{"nature"})
	if calls != 2 {
		t.Errorf("count 2: writer called %d times", calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	writeProgramme = func(_ *llmClientT, fmtKey string, _ *materialT, _ float64, slot *int, _ logT, _ *randT) (*script.Programme, error) {
		calls++
		if calls == 5 {
			cancel()
		}
		return nil, nil
	}
	p.Run(ctx, nil, []string{"nature"})
	if calls != 5 {
		t.Errorf("count nil: writer called %d times before the cancel, want 5", calls)
	}
}

// A lineup entry that is not a format key (a misconfigured --lineup / --formats) is FORMATS[fmt_key]'s
// KeyError: the prefetch logs "writing slot N failed", the loop body logs "programme failed ... retrying in
// 60s", advances the slot without counting it and never asks the writer for the bad key — so `--count 2`
// still produces two programmes (aitv/producer.py run() lines 311 / 123), not one plus a fabricated
// "=== slot 2/2:  (0 min)".
func TestRunUnknownFormatIsProgrammeFailure(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	mem := &memIndex{}
	loadIndex, appendIndex = mem.load, mem.append
	var calls []writerCall
	writeProgramme = func(_ *llmClientT, fmtKey string, _ *materialT, _ float64, slot *int, _ logT, _ *randT) (*script.Programme, error) {
		calls = append(calls, writerCall{fmtKey, *slot})
		if _, ok := programs.Formats[fmtKey]; !ok {
			t.Errorf("writer asked for %q", fmtKey)
		}
		s := *slot
		return testProgramme(fmt.Sprintf("20260917-0000%02d-000001", len(calls)), fmtKey, fmt.Sprintf("番組%d", len(calls)), &s, "a"), nil
	}
	writeBreak = func(_ *llmClientT, fmtKey string) (*script.Programme, error) {
		return testProgramme("20260917-000100-bbbbbb", fmtKey, "ブレイク", nil, "x"), nil
	}
	gpu := renderingGPU(t, src, 1)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{Minutes: 1, Lineup: []string{"nature", "foo"}})
	p.Run(context.Background(), intp(2), nil)
	lb.mustHave(t,
		"[aitv] === slot 1/2: 自然ドキュメンタリー (1 min)",
		"[aitv] writing slot 2 failed (unknown format \"foo\")",
		"[aitv] === done: 番組1 (1 clips, 0 failed)",
		"[aitv] programme failed (unknown format \"foo\"); retrying in 60s",
		"[aitv] === done: 番組2 (1 clips, 0 failed)")
	if lb.has("=== slot 2/2") {
		t.Errorf("the bad slot must not be announced: %q", lb.all())
	}
	if lb.count("=== slot 1/2") != 2 || lb.count("programme failed") != 1 {
		t.Errorf("log: %q", lb.all())
	}
	if want := []writerCall{{"nature", 0}, {"nature", 0}}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("writeProgramme calls = %v, want %v", calls, want)
	}
	if p.slot != 1 {
		t.Errorf("slot = %d, want 1", p.slot)
	}
	if got := len(mem.load()); got != 4 { // nature, break, nature, break
		t.Errorf("index has %d items, want 4", got)
	}
}

// A clean run that produced no file logs "render failed: None" (the Python printed its None error).
func TestRenderFailedNone(t *testing.T) {
	stubAll(t)
	gpu := &fakeGPU{run: func(int, comfy.Graph, comfy.RunOptions) comfy.RunResult { return comfy.RunResult{OK: true} }}
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{Width: 640, Height: 352, Steps: 8})
	prog := testProgramme(fixturePID, "nature", fixtureTitle, intp(4), "c0")
	if it, err := p.RenderSegment(context.Background(), prog, 0, "本編", prog.Sections[0].Segments[0]); it != nil || err != nil {
		t.Errorf("RenderSegment = %+v, %v", it, err)
	}
	if got := lb.all(); len(got) != 1 || got[0] != "[aitv] render failed: None" {
		t.Errorf("log = %q, want the None line", got)
	}
}

// A library directory that cannot be created (the path is a regular file) is RenderSegment's error: one
// "programme failed", no interrupt / free, no escalation — instead of six failed renders.
func TestRenderSegmentLibraryNotCreatable(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	makeClip(t, src)
	libFile := filepath.Join(dir, "library")
	if err := os.WriteFile(libFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	libDir = func() string { return libFile }
	gpu := renderingGPU(t, src, 10)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{Width: 640, Height: 352, Steps: 8})
	prog := testProgramme(fixturePID, "nature", fixtureTitle, intp(4), "c0", "c1", "c2", "c3", "c4", "c5", "c6", "c7")
	item, err := p.RenderSegment(context.Background(), prog, 0, "本編", prog.Sections[0].Segments[0])
	if item != nil || err == nil || !strings.Contains(err.Error(), "library") {
		t.Errorf("RenderSegment = %+v, %v; want the mkdir error", item, err)
	}
	if got := lb.all(); len(got) != 0 {
		t.Errorf("nothing is logged by RenderSegment itself: %q", got)
	}
	made, failed, err := p.RenderProgramme(context.Background(), prog)
	if made != 0 || failed != 0 || err == nil {
		t.Errorf("RenderProgramme = %d, %d, %v; want the error after the first render", made, failed, err)
	}
	if gpu.runCalls != 2 || gpu.interrupts != 0 || gpu.frees != 0 {
		t.Errorf("runs=%d interrupts=%d frees=%d: the mkdir error must not escalate", gpu.runCalls, gpu.interrupts, gpu.frees)
	}
}

// TestRenderSegmentKana: the dialogue reaches H3 in kana, with the cast's own name readings, while the
// programme on disk keeps the kanji; the index records what was sent.
func TestRenderSegmentKana(t *testing.T) {
	haveFFmpeg(t)
	stubAll(t)
	src := filepath.Join(t.TempDir(), "src.mp4")
	makeClip(t, src)
	gpu := renderingGPU(t, src, 10)
	p, lb := newTestProducer(t, gpu, &fakeLLM{}, Options{})
	if p.yomi == nil {
		t.Fatal("reading dictionary not loaded")
	}
	prog := testProgramme(fixturePID, "cooking", "3分クッキング", intp(5), "c0")
	prog.Cast = []programs.Cast{{Role: "chef", Name: "高橋 悠真", Kana: "たかはし ゆうま", Regular: true}}
	seg := script.Segment{Caption: "火加減", Seconds: 8,
		Prompt: "[Shot 1] The chef (S1) says: <d>[Japanese] 高橋悠真です。火加減を見ながら3分煮ます。</d>\noverall_soundscape: simmering."}
	item, err := p.RenderSegment(context.Background(), prog, 0, "調理", seg)
	if err != nil || item == nil {
		t.Fatalf("RenderSegment = %v, %v; log: %v", item, err, lb.all())
	}
	want := "[Shot 1] The chef (S1) says: <d>[Japanese] たかはしゆうまです。ひかげんをみながらさんぷんにます。</d>\noverall_soundscape: simmering."
	if item.Prompt != want {
		t.Errorf("prompt sent:\n got %q\nwant %q", item.Prompt, want)
	}
	if gpu.lastPrompt != want {
		t.Errorf("GenSpec prompt: got %q", gpu.lastPrompt)
	}
	if seg.Prompt == want {
		t.Error("the script's own segment must keep the kanji")
	}
}
