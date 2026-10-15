// Package loudness keeps the whole channel at one level (port of aitv/loudness.py).
//
// H3 renders come out anywhere between -30 and -10 LUFS (a whispering narrator, then a shouting ident), so
// every clip is normalised on its way into the library with ffmpeg's two-pass loudnorm: pass 1 measures,
// pass 2 applies a plain gain (linear mode) unless the true-peak ceiling forces limiting.
//
// Target: -16 LUFS integrated / -1.5 dBTP, the streaming level (Apple, Spotify, YouTube are -14..-16).
// ARIB TR-B32 broadcast is -24 LKFS, which is too quiet for a browser player. Override with AITV_LUFS.
package loudness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/firemio/generate-local-H3/internal/logx"
)

// TargetI is the integrated loudness target in LUFS (AITV_LUFS, default -16).
var TargetI = -16.0

const (
	TargetTP  = -1.5  // dBTP ceiling
	TargetLRA = 11.0  // loudness range
	Tolerance = 1.0   // LU: a clip within this of the target is left alone (Renormalize is idempotent)
	SilenceI  = -50.0 // LUFS: below this there is no usable audio; +35 dB of gain would only raise hiss
)

const (
	measureTimeout = 120 * time.Second // pass 1 (Python: measure(timeout=120))
	encodeTimeout  = 300 * time.Second // pass 2 in Renormalize (Python: subprocess.run(timeout=300))
	renameTries    = 4                 // the broadcaster holds a clip open for ~10 s while it remuxes it
)

// renameRetryDelay is the pause between rename attempts in Renormalize (a variable so tests can shorten it).
var renameRetryDelay = 4 * time.Second

func init() {
	var err error
	if TargetI, err = envTarget(TargetI); err != nil {
		logx.Printf("[renorm] %v; using %v LUFS", err, TargetI)
	}
}

// envTarget reads AITV_LUFS as a float (surrounding whitespace ignored); def is returned when the variable
// is unset, and with the error when its value is not a number.
func envTarget(def float64) (float64, error) {
	s, ok := os.LookupEnv("AITV_LUFS")
	if !ok {
		return def, nil
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def, fmt.Errorf("AITV_LUFS: not a number: %q", s)
	}
	return v, nil
}

// Measurement is loudnorm's pass-1 block.
type Measurement struct {
	InputI      float64 // input_i
	InputTP     float64 // input_tp
	InputLRA    float64 // input_lra
	InputThresh float64 // input_thresh
	Offset      float64 // target_offset
}

// params is the target part shared by both passes: I=-16.0:TP=-1.5:LRA=11.0.
func params() string {
	return "I=" + pyStr(TargetI) + ":TP=" + pyStr(TargetTP) + ":LRA=" + pyStr(TargetLRA)
}

// Measure runs ffmpeg -hide_banner -nostats -i <path> -vn -af loudnorm=I=..:TP=..:LRA=..:print_format=json
// -f null - (120 s timeout) and parses the JSON block at the end of stderr (values may be "-inf"; parse
// them as floats). nil when the file cannot be measured.
func Measure(path string) *Measurement {
	ctx, cancel := context.WithTimeout(context.Background(), measureTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostats", "-i", path, "-vn",
		"-af", "loudnorm="+params()+":print_format=json", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second // after the kill on timeout, do not hang on the stderr pipe
	// Python did not check the exit status: whatever ffmpeg printed is parsed, and a run that produced no
	// JSON block (unreadable file, no audio stream, ffmpeg missing) is simply "cannot measure".
	_ = cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	return parseMeasurement(stderr.String())
}

// parseMeasurement finds the last {...} block in ffmpeg's stderr and reads the five pass-1 values from it.
func parseMeasurement(stderr string) *Measurement {
	start, end := strings.LastIndex(stderr, "{"), strings.LastIndex(stderr, "}")
	if start < 0 || end < start {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(stderr[start:end+1]), &raw); err != nil {
		return nil
	}
	var m Measurement
	for _, f := range []struct {
		key string
		dst *float64
	}{
		{"input_i", &m.InputI},
		{"input_tp", &m.InputTP},
		{"input_lra", &m.InputLRA},
		{"input_thresh", &m.InputThresh},
		{"target_offset", &m.Offset},
	} {
		v, ok := raw[f.key]
		if !ok {
			return nil
		}
		x, ok := toFloat(v)
		if !ok {
			return nil
		}
		*f.dst = x
	}
	return &m
}

// toFloat is Python's float(x) for a decoded JSON value: a number, or a string such as "-16.03" / "-inf".
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Filter is the pass-2 loudnorm filter: loudnorm=I=..:TP=..:LRA=..:measured_I=..:measured_TP=..:
// measured_LRA=..:measured_thresh=..:offset=..:linear=true:print_format=none. Floats are formatted like
// Python's str() (shortest repr, e.g. -27.4, -0.3).
func Filter(m *Measurement) string {
	return "loudnorm=" + params() +
		":measured_I=" + pyStr(m.InputI) +
		":measured_TP=" + pyStr(m.InputTP) +
		":measured_LRA=" + pyStr(m.InputLRA) +
		":measured_thresh=" + pyStr(m.InputThresh) +
		":offset=" + pyStr(m.Offset) +
		":linear=true:print_format=none"
}

// AudioFilterArgs returns []string{"-af", Filter(m)} for src, or nil when there is nothing to correct
// (unmeasurable or below SilenceI).
func AudioFilterArgs(src string) []string {
	m := Measure(src)
	if m == nil || m.InputI < SilenceI {
		return nil
	}
	return []string{"-af", Filter(m)}
}

// Stats summarises a Renormalize run.
type Stats struct {
	Total, Fixed, AtLevel, Silent, Locked, Failed int
}

// Renormalize brings every <lib>/*.mp4 (sorted by name; the first limit of them when limit > 0) to the
// target. A library that cannot be listed is the error, nothing logged.
// Only the audio is re-encoded (video stream-copied): ffmpeg -y -hide_banner -loglevel error -i <src>
// -c:v copy -af <Filter> -c:a aac -b:a 160k -ar 48000 -ac 2 -f mp4 -movflags +faststart <src>.renorm.tmp,
// then rename over the original, retried 4 times at 4 s (the broadcaster holds a clip open ~10 s while it
// remuxes it); a clip that stays locked is skipped and reported ("run renorm again later"). Clips within
// Tolerance are left alone (AtLevel); below SilenceI they are left alone (Silent). Logs
// "[renorm] n/total name: X LUFS -> T" per fix (with "  (dry run)" in dry-run mode) and a final
// "[renorm] done in Ns: total .., fixed .., at_level .., silent .., locked .., failed ..".
func Renormalize(lib string, dryRun bool, limit int, log logx.Logger) (Stats, error) {
	if log == nil {
		log = logx.Printf
	}
	var stats Stats
	entries, err := os.ReadDir(lib)
	if err != nil {
		return stats, err
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mp4") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if limit > 0 && limit < len(files) {
		files = files[:limit]
	}
	stats.Total = len(files)
	t0 := time.Now()
	for i, name := range files {
		n := i + 1
		src := filepath.Join(lib, name)
		m := Measure(src)
		if m == nil {
			stats.Failed++
			log("[renorm] %s: could not measure", name)
			continue
		}
		if m.InputI < SilenceI {
			stats.Silent++
			continue
		}
		if math.Abs(m.InputI-TargetI) <= Tolerance {
			stats.AtLevel++
			continue
		}
		suffix := ""
		if dryRun {
			suffix = "  (dry run)"
		}
		log("[renorm] %d/%d %s: %.1f LUFS -> %.0f%s", n, len(files), name, m.InputI, TargetI, suffix)
		if dryRun {
			stats.Fixed++
			continue
		}
		tmp := src + ".renorm.tmp"
		if err := encode(src, tmp, m); err != nil {
			stats.Failed++
			log("[renorm] %s: ffmpeg failed (%v)", name, err)
			rm(tmp)
			continue
		}
		replaced := false
		for try := 0; try < renameTries; try++ {
			if err := os.Rename(tmp, src); err == nil {
				replaced = true
				break
			}
			time.Sleep(renameRetryDelay)
		}
		if replaced {
			stats.Fixed++
		} else {
			stats.Locked++
			log("[renorm] %s: in use by the broadcaster, skipped (run renorm again later)", name)
			rm(tmp)
		}
	}
	log("[renorm] done in %.0fs: total %d, fixed %d, at_level %d, silent %d, locked %d, failed %d",
		time.Since(t0).Seconds(), stats.Total, stats.Fixed, stats.AtLevel, stats.Silent, stats.Locked, stats.Failed)
	return stats, nil
}

// encode is Renormalize's pass 2: src -> tmp with the video stream-copied and the audio re-encoded through
// Filter(m). ffmpeg's own error lines (-loglevel error) go to our stderr, as they did under Python.
func encode(src, tmp string, m *Measurement) error {
	ctx, cancel := context.WithTimeout(context.Background(), encodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-hide_banner", "-loglevel", "error", "-i", src,
		"-c:v", "copy", "-af", Filter(m),
		"-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2",
		"-f", "mp4", "-movflags", "+faststart", tmp)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", encodeTimeout)
	}
	return err
}

// rm removes path, ignoring errors (Python's _rm).
func rm(path string) {
	_ = os.Remove(path)
}

// pyStr formats v the way Python's str(float) does: the shortest digits that round-trip, in fixed notation
// with at least one digit after the point when the decimal exponent is in [-4, 16), exponent notation
// otherwise (1e-05, 1e+16), and inf / -inf / nan for the specials. ffmpeg prints two decimals, so the
// pass-2 filter shows -16.03, -5.51, 0.9, -inf exactly as Python wrote it.
func pyStr(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(v, 'e', -1, 64) // d.ddde±XX with the shortest round-trip digits
	exp, _ := strconv.Atoi(e[strings.LastIndexByte(e, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return e // Python's repr switches to exponent form at the same points and pads the exponent to 2 digits too
	}
	f := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(f, ".") {
		f += ".0"
	}
	return f
}
