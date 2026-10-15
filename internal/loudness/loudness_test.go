package loudness

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firemio/generate-local-H3/internal/logx"
)

// Golden values captured from the Python implementation (aitv/loudness.py, .venv python, ffmpeg 8.1.1):
//
//	>>> loudness.measure('aitv/library/cm-20260910-184522-64ba79.mp4')
//	{'input_i': -16.03, 'input_tp': -5.51, 'input_lra': 0.9, 'input_thresh': -26.26, 'target_offset': -1.02}
//	>>> loudness.filter_for(_)
//	(pyFilterReal below)
//	>>> loudness.measure(<the same clip with -af volume=0>)
//	{'input_i': -inf, 'input_tp': -inf, 'input_lra': 0.0, 'input_thresh': -70.0, 'target_offset': inf}
//	>>> loudness.filter_for(_)
//	(pyFilterSilent below); audio_filter_args -> []
//	>>> loudness.measure(<the same clip with -af volume=-10dB>)
//	{'input_i': -26.05, 'input_tp': -15.14, 'input_lra': 1.0, 'input_thresh': -36.29, 'target_offset': -1.02}
const (
	libClip = "../../aitv/library/cm-20260910-184522-64ba79.mp4"

	pyFilterReal   = "loudnorm=I=-16.0:TP=-1.5:LRA=11.0:measured_I=-16.03:measured_TP=-5.51:measured_LRA=0.9:measured_thresh=-26.26:offset=-1.02:linear=true:print_format=none"
	pyFilterSilent = "loudnorm=I=-16.0:TP=-1.5:LRA=11.0:measured_I=-inf:measured_TP=-inf:measured_LRA=0.0:measured_thresh=-70.0:offset=inf:linear=true:print_format=none"
	pyFilterQuiet  = "loudnorm=I=-16.0:TP=-1.5:LRA=11.0:measured_I=-26.05:measured_TP=-15.14:measured_LRA=1.0:measured_thresh=-36.29:offset=-1.02:linear=true:print_format=none"
)

var (
	pyMeasureReal   = Measurement{InputI: -16.03, InputTP: -5.51, InputLRA: 0.9, InputThresh: -26.26, Offset: -1.02}
	pyMeasureSilent = Measurement{InputI: inf(-1), InputTP: inf(-1), InputLRA: 0.0, InputThresh: -70.0, Offset: inf(1)}
	pyMeasureQuiet  = Measurement{InputI: -26.05, InputTP: -15.14, InputLRA: 1.0, InputThresh: -36.29, Offset: -1.02}
)

func inf(sign int) float64 {
	var zero float64
	if sign < 0 {
		return -1 / zero
	}
	return 1 / zero
}

// TestPyStr: Python str(float) for each value, captured with `print(repr(v), '->', str(v))`.
func TestPyStr(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{-27.4, "-27.4"},
		{-0.3, "-0.3"},
		{-16.0, "-16.0"},
		{0.0, "0.0"},
		{5.0, "5.0"},
		{-70.0, "-70.0"},
		{100.0, "100.0"},
		{-23.85, "-23.85"},
		{123456789.123, "123456789.123"},
		{0.0001, "0.0001"},
		{1e-5, "1e-05"},
		{1.5e-7, "1.5e-07"},
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1e17, "1e+17"},
		{1e21, "1e+21"},
		{inf(-1), "-inf"},
		{inf(1), "inf"},
		{0 * inf(1), "nan"},
	}
	for _, c := range cases {
		if got := pyStr(c.v); got != c.want {
			t.Errorf("pyStr(%v) = %q, want %q", c.v, got, c.want)
		}
	}
	// Computed at run time (as untyped constants Go would fold 0.1 + 0.2 to exactly 0.3 and -0.0 to 0).
	tenth, fifth, zero := 0.1, 0.2, 0.0
	if got := pyStr(tenth + fifth); got != "0.30000000000000004" {
		t.Errorf("pyStr(0.1+0.2) = %q, want %q", got, "0.30000000000000004")
	}
	if got := pyStr(-zero); got != "-0.0" {
		t.Errorf("pyStr(-0.0) = %q, want %q", got, "-0.0")
	}
}

func TestFilter(t *testing.T) {
	if got := params(); got != "I=-16.0:TP=-1.5:LRA=11.0" {
		t.Errorf("params() = %q", got)
	}
	for _, c := range []struct {
		m    Measurement
		want string
	}{
		{pyMeasureReal, pyFilterReal},
		{pyMeasureSilent, pyFilterSilent},
		{pyMeasureQuiet, pyFilterQuiet},
	} {
		if got := Filter(&c.m); got != c.want {
			t.Errorf("Filter(%+v)\n got %q\nwant %q", c.m, got, c.want)
		}
	}
}

// TestFilterTargetOverride: Python float("-23") prints as -23.0 in the filter.
func TestFilterTargetOverride(t *testing.T) {
	saved := TargetI
	defer func() { TargetI = saved }()
	TargetI = -23
	want := "loudnorm=I=-23.0:TP=-1.5:LRA=11.0:measured_I=-16.03:"
	if got := Filter(&pyMeasureReal); !strings.HasPrefix(got, want) {
		t.Errorf("Filter with TargetI=-23 = %q, want prefix %q", got, want)
	}
}

// AITV_LUFS is a float with surrounding whitespace ignored; anything else keeps the default and reports
// the error (init logs it and keeps the default).
func TestEnvTarget(t *testing.T) {
	for _, c := range []struct {
		env  string
		want float64
		bad  bool
	}{
		{"-23", -23, false},
		{" -14.5 ", -14.5, false},
		{"loud", -16, true},
		{"", -16, true},
	} {
		t.Setenv("AITV_LUFS", c.env)
		got, err := envTarget(-16)
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("AITV_LUFS=%q: envTarget = %v, %v; want %v, error %v", c.env, got, err, c.want, c.bad)
		}
	}
	os.Unsetenv("AITV_LUFS")
	if got, err := envTarget(-16); got != -16 || err != nil {
		t.Errorf("unset: envTarget = %v, %v", got, err)
	}
}

func TestParseMeasurement(t *testing.T) {
	real := string(readFixture(t, "pass1-real.txt"))
	if got := parseMeasurement(real); got == nil || *got != pyMeasureReal {
		t.Errorf("pass1-real: got %+v, want %+v", got, pyMeasureReal)
	}
	silent := string(readFixture(t, "pass1-silent.txt"))
	if got := parseMeasurement(silent); got == nil || *got != pyMeasureSilent {
		t.Errorf("pass1-silent: got %+v, want %+v", got, pyMeasureSilent)
	}
	notMedia := string(readFixture(t, "pass1-notmedia.txt"))
	if got := parseMeasurement(notMedia); got != nil {
		t.Errorf("pass1-notmedia: got %+v, want nil", got)
	}
	for name, s := range map[string]string{
		"empty":        "",
		"no close":     "x {\"input_i\": \"1\"",
		"close before": "} {",
		"bad json":     "{ input_i: 1 }",
		"missing key":  `{"input_i": "-16.03", "input_tp": "-5.51", "input_lra": "0.9", "input_thresh": "-26.26"}`,
		"bad value":    `{"input_i": "loud", "input_tp": "-5.51", "input_lra": "0.9", "input_thresh": "-26.26", "target_offset": "0"}`,
		"not object":   `["input_i"]`,
	} {
		if got := parseMeasurement(s); got != nil {
			t.Errorf("%s: got %+v, want nil", name, got)
		}
	}
	// Numbers instead of strings are accepted the way Python's float() accepted them; the last {...} wins.
	got := parseMeasurement("{} noise {\"input_i\": -20, \"input_tp\": \"-1\", \"input_lra\": 2, \"input_thresh\": \"-30\", \"target_offset\": 0.5}\n")
	want := Measurement{InputI: -20, InputTP: -1, InputLRA: 2, InputThresh: -30, Offset: 0.5}
	if got == nil || *got != want {
		t.Errorf("numeric block: got %+v, want %+v", got, want)
	}
}

// TestMeasureParity: Go Measure on the real library clip vs the Python measurement, within 0.05.
func TestMeasureParity(t *testing.T) {
	requireFFmpeg(t)
	if _, err := os.Stat(libClip); err != nil {
		t.Skipf("library clip not present: %v", err)
	}
	got := Measure(libClip)
	if got == nil {
		t.Fatal("Measure returned nil")
	}
	for _, f := range []struct {
		name      string
		got, want float64
	}{
		{"input_i", got.InputI, pyMeasureReal.InputI},
		{"input_tp", got.InputTP, pyMeasureReal.InputTP},
		{"input_lra", got.InputLRA, pyMeasureReal.InputLRA},
		{"input_thresh", got.InputThresh, pyMeasureReal.InputThresh},
		{"target_offset", got.Offset, pyMeasureReal.Offset},
	} {
		if d := f.got - f.want; d > 0.05 || d < -0.05 {
			t.Errorf("%s: got %v, python %v", f.name, f.got, f.want)
		}
	}
	if *got == pyMeasureReal {
		if args := AudioFilterArgs(libClip); len(args) != 2 || args[0] != "-af" || args[1] != pyFilterReal {
			t.Errorf("AudioFilterArgs = %q, want [-af %s]", args, pyFilterReal)
		}
	} else {
		t.Logf("measurement differs from the Python golden in the last digit (%+v); filter string not compared", *got)
	}
}

func TestMeasureUnreadable(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	if got := Measure(filepath.Join(dir, "missing.mp4")); got != nil {
		t.Errorf("missing file: got %+v, want nil", got)
	}
	junk := filepath.Join(dir, "junk.mp4")
	if err := os.WriteFile(junk, []byte("not a video {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Measure(junk); got != nil {
		t.Errorf("junk file: got %+v, want nil", got)
	}
	if got := AudioFilterArgs(junk); got != nil {
		t.Errorf("AudioFilterArgs(junk) = %q, want nil", got)
	}
}

func TestAudioFilterArgsSilent(t *testing.T) {
	src := sourceClip(t)
	silent := filepath.Join(t.TempDir(), "silent.mp4")
	ffmpeg(t, "-i", src, "-c:v", "copy", "-af", "volume=0", "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2", "-t", "2", silent)
	m := Measure(silent)
	if m == nil {
		t.Fatal("Measure(silent) returned nil")
	}
	if !(m.InputI < SilenceI) {
		t.Errorf("silent clip measured %v LUFS, expected below %v", m.InputI, SilenceI)
	}
	if got := AudioFilterArgs(silent); got != nil {
		t.Errorf("AudioFilterArgs(silent) = %q, want nil", got)
	}
}

// TestRenormalize mirrors the Python session on a temp library of three copies of a clip:
//
//	a-level.mp4  the clip as is (-16.03 LUFS: at level, skipped)
//	b-quiet.mp4  -af volume=-10dB (-26.05 LUFS: fixed)
//	c-silent.mp4 -af volume=0 (-inf: silent, skipped)
//
//	[renorm] 2/3 b-quiet.mp4: -26.1 LUFS -> -16  (dry run)
//	[renorm] done in 0s: total 3, fixed 1, at_level 1, silent 1, locked 0, failed 0
//	{'total': 3, 'fixed': 1, 'at_level': 1, 'silent': 1, 'locked': 0, 'failed': 0}
//	== limit 1
//	[renorm] done in 0s: total 1, fixed 0, at_level 1, silent 0, locked 0, failed 0
//	== real
//	[renorm] 2/3 b-quiet.mp4: -26.1 LUFS -> -16
//	[renorm] done in 0s: total 3, fixed 1, at_level 1, silent 1, locked 0, failed 0
//	== after (b-quiet.mp4 measures -16.05; a second run changes nothing)
//	[renorm] done in 0s: total 3, fixed 0, at_level 2, silent 1, locked 0, failed 0
func TestRenormalize(t *testing.T) {
	lib, quiet := tempLibrary(t)
	quietSig := videoSignature(t, quiet)
	before := snapshot(t, lib)

	// Dry run: counts as if fixed, touches nothing.
	log := &logCapture{}
	got, err := Renormalize(lib, true, 0, log.f)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{Total: 3, Fixed: 1, AtLevel: 1, Silent: 1}
	if got != want {
		t.Errorf("dry run: %+v, want %+v", got, want)
	}
	log.expect(t,
		"[renorm] 2/3 b-quiet.mp4: -26.1 LUFS -> -16  (dry run)",
		`^\[renorm\] done in \d+s: total 3, fixed 1, at_level 1, silent 1, locked 0, failed 0$`)
	if after := snapshot(t, lib); after != before {
		t.Errorf("dry run changed the library:\nbefore %v\nafter  %v", before, after)
	}

	// --limit 1 sees only a-level.mp4.
	log = &logCapture{}
	if got := mustRenormalize(t, lib, true, 1, log.f); got != (Stats{Total: 1, AtLevel: 1}) {
		t.Errorf("limit 1: %+v", got)
	}
	log.expect(t, `^\[renorm\] done in \d+s: total 1, fixed 0, at_level 1, silent 0, locked 0, failed 0$`)
	// A limit past the end (or none) is everything.
	if got := mustRenormalize(t, lib, true, 5, (&logCapture{}).f); got != want {
		t.Errorf("limit 5: %+v", got)
	}

	// The real thing.
	log = &logCapture{}
	if got := mustRenormalize(t, lib, false, 0, log.f); got != want {
		t.Errorf("renormalize: %+v, want %+v", got, want)
	}
	log.expect(t,
		"[renorm] 2/3 b-quiet.mp4: -26.1 LUFS -> -16",
		`^\[renorm\] done in \d+s: total 3, fixed 1, at_level 1, silent 1, locked 0, failed 0$`)
	assertNoTemp(t, lib)
	m := Measure(quiet)
	if m == nil {
		t.Fatal("cannot measure the fixed clip")
	}
	if d := m.InputI - TargetI; d > 0.3 || d < -0.3 {
		t.Errorf("fixed clip measures %v LUFS, want %v (python got -16.05)", m.InputI, TargetI)
	}
	if sig := videoSignature(t, quiet); sig != quietSig {
		t.Errorf("video stream was not copied as is: %q -> %q", quietSig, sig)
	}

	// Idempotent: a second run leaves everything alone.
	log = &logCapture{}
	if got := mustRenormalize(t, lib, false, 0, log.f); got != (Stats{Total: 3, AtLevel: 2, Silent: 1}) {
		t.Errorf("second run: %+v", got)
	}
	log.expect(t, `^\[renorm\] done in \d+s: total 3, fixed 0, at_level 2, silent 1, locked 0, failed 0$`)
	assertNoTemp(t, lib)
}

// TestRenormalizeLocked: a clip held open by another handle (the broadcaster's ffmpeg) cannot be replaced on
// Windows; it is reported as locked, the original is intact and the .renorm.tmp is removed.
func TestRenormalizeLocked(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("rename over an open file only fails on Windows")
	}
	lib, quiet := tempLibrary(t)
	before := snapshot(t, lib)
	h, err := os.Open(quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	saved := renameRetryDelay
	renameRetryDelay = 10 * time.Millisecond
	defer func() { renameRetryDelay = saved }()

	log := &logCapture{}
	got := mustRenormalize(t, lib, false, 0, log.f)
	want := Stats{Total: 3, AtLevel: 1, Silent: 1, Locked: 1}
	if got != want {
		t.Errorf("locked: %+v, want %+v", got, want)
	}
	log.expect(t,
		"[renorm] 2/3 b-quiet.mp4: -26.1 LUFS -> -16",
		"[renorm] b-quiet.mp4: in use by the broadcaster, skipped (run renorm again later)",
		`^\[renorm\] done in \d+s: total 3, fixed 0, at_level 1, silent 1, locked 1, failed 0$`)
	assertNoTemp(t, lib)
	if after := snapshot(t, lib); after != before {
		t.Errorf("locked run changed the library:\nbefore %v\nafter  %v", before, after)
	}
}

func TestRenormalizeFailed(t *testing.T) {
	requireFFmpeg(t)
	lib := t.TempDir()
	if err := os.WriteFile(filepath.Join(lib, "broken.mp4"), []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := &logCapture{}
	if got := mustRenormalize(t, lib, false, 0, log.f); got != (Stats{Total: 1, Failed: 1}) {
		t.Errorf("failed: %+v", got)
	}
	log.expect(t,
		"[renorm] broken.mp4: could not measure",
		`^\[renorm\] done in \d+s: total 1, fixed 0, at_level 0, silent 0, locked 0, failed 1$`)
	assertNoTemp(t, lib)
}

// A library directory that is missing (a fresh checkout before the first produce run) or not a directory
// made the Python's os.listdir raise FileNotFoundError / NotADirectoryError before anything was logged;
// the error is returned (aitv renorm exits 1 with it), nothing is logged.
func TestRenormalizeMissingDir(t *testing.T) {
	log := &logCapture{}
	got, err := Renormalize(filepath.Join(t.TempDir(), "nope"), false, 0, log.f)
	if err == nil || !os.IsNotExist(err) || got != (Stats{}) {
		t.Errorf("missing dir: %+v, %v", got, err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Renormalize(file, true, 0, log.f); err == nil || got != (Stats{}) {
		t.Errorf("not a directory: %+v, %v", got, err)
	}
	if len(log.lines) != 0 {
		t.Errorf("logged before failing: %q", log.lines)
	}
}

func mustRenormalize(t *testing.T, lib string, dryRun bool, limit int, log logx.Logger) Stats {
	t.Helper()
	got, err := Renormalize(lib, dryRun, limit, log)
	if err != nil {
		t.Fatalf("Renormalize: %v", err)
	}
	return got
}

// --- helpers ---

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-y", "-hide_banner", "-loglevel", "error"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// sourceClip is the real library clip (-16.03 LUFS) when it is present (aitv/library is runtime data, not
// in git), else a 3 s sine tone clip synthesised to about -16.05 LUFS so the tests still run.
func sourceClip(t *testing.T) string {
	t.Helper()
	requireFFmpeg(t)
	if _, err := os.Stat(libClip); err == nil {
		return libClip
	}
	out := filepath.Join(t.TempDir(), "synth.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3",
		"-f", "lavfi", "-i", "color=c=black:s=64x64:r=24:d=3",
		"-af", "volume=5.8dB", "-shortest", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "160k", "-ac", "2", "-movflags", "+faststart", out)
	m := Measure(out)
	if m == nil || m.InputI-TargetI > Tolerance || TargetI-m.InputI > Tolerance {
		t.Skipf("synthesised clip is not at level (%+v); library clip absent", m)
	}
	return out
}

// tempLibrary builds the three-clip library of TestRenormalize and returns it with the path of b-quiet.mp4.
func tempLibrary(t *testing.T) (lib, quiet string) {
	t.Helper()
	src := sourceClip(t)
	lib = t.TempDir()
	copyFile(t, src, filepath.Join(lib, "a-level.mp4"))
	quiet = filepath.Join(lib, "b-quiet.mp4")
	ffmpeg(t, "-i", src, "-c:v", "copy", "-af", "volume=-10dB", "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2", quiet)
	ffmpeg(t, "-i", src, "-c:v", "copy", "-af", "volume=0", "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2", "-t", "2",
		filepath.Join(lib, "c-silent.mp4"))
	if err := os.WriteFile(filepath.Join(lib, "index.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	return lib, quiet
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// snapshot is "name size mtime" for every entry, to prove a run touched nothing.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %d %s; ", e.Name(), info.Size(), info.ModTime().Format(time.RFC3339Nano))
	}
	return b.String()
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.renorm.tmp"))
	if len(matches) > 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// videoSignature is the video stream's codec, size and frame count (ffprobe), which -c:v copy must preserve.
// Empty when ffprobe is unavailable (the check is then skipped by comparing empty with empty).
func videoSignature(t *testing.T, path string) string {
	t.Helper()
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return ""
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height,nb_frames", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) f(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// expect checks the captured lines one to one: a pattern starting with ^ is a regexp, anything else must
// match exactly.
func (l *logCapture) expect(t *testing.T, want ...string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) != len(want) {
		t.Errorf("log: got %d lines, want %d\n got %q\nwant %q", len(l.lines), len(want), l.lines, want)
		return
	}
	for i, w := range want {
		got := l.lines[i]
		if strings.HasPrefix(w, "^") {
			if !regexp.MustCompile(w).MatchString(got) {
				t.Errorf("log line %d: %q does not match %q", i+1, got, w)
			}
		} else if got != w {
			t.Errorf("log line %d:\n got %q\nwant %q", i+1, got, w)
		}
	}
}
