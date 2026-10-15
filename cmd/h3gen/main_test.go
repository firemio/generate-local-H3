package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/firemio/generate-local-H3/internal/comfy"
)

// capture swaps the CLI's streams for buffers for the duration of the test.
func capture(t *testing.T) (out, errBuf *bytes.Buffer) {
	t.Helper()
	out, errBuf = &bytes.Buffer{}, &bytes.Buffer{}
	oldOut, oldErr := stdout, stderr
	stdout, stderr = out, errBuf
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	return out, errBuf
}

func TestRepeatableFlags(t *testing.T) {
	capture(t)
	o, code := parseGen("gen", []string{"--prompt", "p", "--ref-image", "a.png", "--ref-image=b.png",
		"--ref-audio", "v.wav", "--ref-image", "c.png"})
	if o == nil {
		t.Fatalf("parse failed with code %d", code)
	}
	if want := []string{"a.png", "b.png", "c.png"}; !reflect.DeepEqual([]string(o.refImage), want) {
		t.Errorf("--ref-image: got %v want %v", o.refImage, want)
	}
	if want := []string{"v.wav"}; !reflect.DeepEqual([]string(o.refAudio), want) {
		t.Errorf("--ref-audio: got %v want %v", o.refAudio, want)
	}
	o, _ = parseGen("gen", []string{"--prompt", "p"})
	if len(o.refImage) != 0 || len(o.refAudio) != 0 {
		t.Errorf("absent repeatable flags should be empty: %v %v", o.refImage, o.refAudio)
	}
}

func TestParseDefaultsAndChoices(t *testing.T) {
	_, errBuf := capture(t)
	for _, k := range []string{"COMFY_HOST", "COMFY_PORT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	o, _ := parseGen("bench", []string{})
	if o == nil {
		t.Fatal("bench with no flags must parse (the prompt is checked later)")
	}
	want := &genOpts{host: "127.0.0.1", port: 8188, mode: "t2v", aspect: "16:9", mp: 0.4, seconds: 5,
		steps: 8, loraStrength: 1, scheduler: "simple", refImageSize: "match", prefix: "h3/h3", runs: 2}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("defaults:\n got %+v\nwant %+v", o, want)
	}
	if o.shiftVideo.v != nil || o.shiftAudio.v != nil {
		t.Error("unset --shift-* must be nil")
	}

	// environment defaults, read at parse time
	t.Setenv("COMFY_HOST", "10.0.0.5")
	t.Setenv("COMFY_PORT", "9999")
	o, _ = parseGen("gen", []string{"--prompt", "p"})
	if o.host != "10.0.0.5" || o.port != 9999 {
		t.Errorf("env defaults: host=%q port=%d", o.host, o.port)
	}
	t.Setenv("COMFY_PORT", "abc")
	if o, code := parseGen("gen", []string{"--prompt", "p"}); code != 0 || o.port != 8188 {
		t.Errorf("a non-integer COMFY_PORT counts as unset: code %d port %d", code, o.port)
	}
	t.Setenv("COMFY_PORT", "8188")

	// choices
	for _, bad := range [][]string{
		{"--mode", "x2v"}, {"--attention", "flash"}, {"--ref-image-size", "huge"},
	} {
		errBuf.Reset()
		if o, code := parseGen("gen", append([]string{"--prompt", "p"}, bad...)); o != nil || code != 2 {
			t.Errorf("%v: got %v code %d, want usage error", bad, o, code)
		}
		if !strings.Contains(errBuf.String(), "must be one of") {
			t.Errorf("%v: message %q should list the choices", bad, errBuf.String())
		}
	}
	o, _ = parseGen("gen", []string{"--prompt", "p", "--mode", "r2v", "--attention", "comfy kitchen attention",
		"--ref-image-size", "max", "--shift-video", "8", "--shift-audio=2.5", "--no-turbo", "--dry-run",
		"--width", "700", "--seed", "18446744073709551615"})
	if o == nil {
		t.Fatal("valid choices rejected")
	}
	if o.mode != "r2v" || o.attention != "comfy kitchen attention" || o.refImageSize != "max" {
		t.Errorf("choices not stored: %+v", o)
	}
	if v := o.shiftVideo.v; v == nil || *v != 8 {
		t.Errorf("--shift-video: %v", v)
	}
	if v := o.shiftAudio.v; v == nil || *v != 2.5 {
		t.Errorf("--shift-audio: %v", v)
	}
	if !o.noTurbo || !o.dryRun || o.width != 700 || o.height != 0 || o.seed != 1<<64-1 {
		t.Errorf("flags: %+v", o)
	}
	// values the flag package rejects are usage errors
	for _, bad := range [][]string{{"--seed", "-1"}, {"--steps", "x"}, {"--seconds", "4x"}, {"--shift-video", "y"}, {"--steps"}} {
		errBuf.Reset()
		if o, code := parseGen("gen", append([]string{"--prompt", "p"}, bad...)); o != nil || code != 2 ||
			!strings.HasPrefix(errBuf.String(), "h3gen gen: ") || !strings.Contains(errBuf.String(), "usage: h3gen gen [flags]") {
			t.Errorf("%v: got %v code %d stderr %q, want a usage error", bad, o, code, errBuf.String())
		}
	}
}

func TestRunUsage(t *testing.T) {
	out, errBuf := capture(t)
	if code := run(nil); code != 2 || !strings.Contains(errBuf.String(), "usage: h3gen") {
		t.Errorf("no subcommand: code %d, stderr %q", code, errBuf.String())
	}
	for _, cmd := range []string{"bogus", "help"} {
		errBuf.Reset()
		out.Reset()
		if code := run([]string{cmd}); code != 2 || out.Len() != 0 || !strings.Contains(errBuf.String(), "unknown command \""+cmd+"\"") {
			t.Errorf("%s: code %d, stdout %q, stderr %q", cmd, code, out.String(), errBuf.String())
		}
	}
	errBuf.Reset()
	out.Reset()
	if code := run([]string{"--help"}); code != 0 || !strings.Contains(out.String(), "h3gen {gen,bench,info}") {
		t.Errorf("--help: code %d, stdout %q", code, out.String())
	}
	for _, argv := range [][]string{{"gen", "-h"}, {"bench", "--help"}, {"info", "--help"}} {
		out.Reset()
		errBuf.Reset()
		if code := run(argv); code != 0 || !strings.HasPrefix(out.String(), "usage: h3gen "+argv[0]+" [flags]\n") ||
			!strings.Contains(out.String(), "-port") || errBuf.Len() != 0 {
			t.Errorf("%v: code %d, stdout %q, stderr %q (help goes to stdout)", argv, code, out.String(), errBuf.String())
		}
	}
	errBuf.Reset()
	if code := run([]string{"gen", "--dry-run", "--prompt", "x", "stray"}); code != 2 ||
		!strings.Contains(errBuf.String(), `unexpected argument "stray"`) {
		t.Errorf("stray positional: code %d, stderr %q", code, errBuf.String())
	}
	errBuf.Reset()
	if code := run([]string{"gen", "--dry-run", "--prompt", "x", "--nope"}); code != 2 ||
		!strings.Contains(errBuf.String(), "flag provided but not defined: -nope") {
		t.Errorf("unknown flag: code %d, stderr %q", code, errBuf.String())
	}
	errBuf.Reset()
	if code := run([]string{"gen", "--dry-run", "--prompt", "x", "--sec", "4"}); code != 2 {
		t.Errorf("abbreviations are not accepted: code %d", code)
	}
	errBuf.Reset()
	if code := run([]string{"info", "--port", "x"}); code != 2 {
		t.Errorf("info bad port: code %d", code)
	}
	errBuf.Reset()
	if code := run([]string{"gen", "--dry-run"}); code != 1 ||
		strings.TrimSpace(errBuf.String()) != "h3gen: --prompt or --prompt-file required" {
		t.Errorf("missing prompt: code %d, stderr %q", code, errBuf.String())
	}
	errBuf.Reset()
	if code := run([]string{"gen", "--dry-run", "--prompt-file", filepath.Join(t.TempDir(), "missing.txt")}); code != 1 ||
		!strings.Contains(errBuf.String(), "missing.txt") {
		t.Errorf("missing prompt file: code %d, stderr %q", code, errBuf.String())
	}
	// inf / nan parse as floats but never reach the graph; 1e400 is out of range for the flag package itself
	for s, want := range map[string]int{"inf": 1, "nan": 1, "1e400": 2} {
		errBuf.Reset()
		out.Reset()
		if code := run([]string{"gen", "--dry-run", "--prompt", "x", "--seconds", s}); code != want || out.Len() != 0 ||
			(want == 1 && !strings.Contains(errBuf.String(), "--seconds must be a finite number")) {
			t.Errorf("--seconds %s: code %d, stdout %q, stderr %q", s, code, out.String(), errBuf.String())
		}
	}
}

// pySpec is dataclasses.asdict(GenSpec) plus "length", as testdata/specs.json holds it.
type pySpec struct {
	Prompt         string            `json:"prompt"`
	Mode           string            `json:"mode"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	Seconds        float64           `json:"seconds"`
	Seed           uint64            `json:"seed"`
	Steps          int               `json:"steps"`
	Turbo          bool              `json:"turbo"`
	LoraStrength   float64           `json:"lora_strength"`
	ShiftVideo     *float64          `json:"shift_video"`
	ShiftAudio     *float64          `json:"shift_audio"`
	Attention      *string           `json:"attention"`
	Scheduler      string            `json:"scheduler"`
	Sampler        string            `json:"sampler"`
	FirstFrame     *string           `json:"first_frame"`
	LastFrame      *string           `json:"last_frame"`
	RefImages      []string          `json:"ref_images"`
	RefAudios      []string          `json:"ref_audios"`
	RefImageSize   string            `json:"ref_image_size"`
	FilenamePrefix string            `json:"filename_prefix"`
	Files          map[string]string `json:"files"`
	Length         int               `json:"length"`
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toPySpec(s comfy.GenSpec) pySpec {
	return pySpec{Prompt: s.Prompt, Mode: s.Mode, Width: s.Width, Height: s.Height, Seconds: s.Seconds,
		Seed: s.Seed, Steps: s.Steps, Turbo: s.Turbo, LoraStrength: s.LoraStrength, ShiftVideo: s.ShiftVideo,
		ShiftAudio: s.ShiftAudio, Attention: optStr(s.Attention), Scheduler: s.Scheduler, Sampler: s.Sampler,
		FirstFrame: optStr(s.FirstFrame), LastFrame: optStr(s.LastFrame), RefImages: s.RefImages,
		RefAudios: s.RefAudios, RefImageSize: s.RefImageSize, FilenamePrefix: s.FilenamePrefix,
		Files: s.Files, Length: s.Length()}
}

// TestSpecFromArgs is the parity check against the Python cli.spec_from_args(args, None): testdata/specs.json
// holds, per case, the argument vector and the resulting dataclass (width/height from --aspect/--mp or
// --width/--height, --unet replacing fl2va or ref2va by mode, --clip, every other flag passed through).
func TestSpecFromArgs(t *testing.T) {
	capture(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "specs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Argv []string `json:"argv"`
		Spec pySpec   `json:"spec"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("only %d cases in specs.json", len(cases))
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			o, code := parseGen("gen", c.Argv)
			if o == nil {
				t.Fatalf("parse %v failed (code %d)", c.Argv, code)
			}
			spec, err := specFromArgs(o, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := toPySpec(spec); !reflect.DeepEqual(got, c.Spec) {
				gj, _ := json.MarshalIndent(got, "", " ")
				wj, _ := json.MarshalIndent(c.Spec, "", " ")
				t.Errorf("spec mismatch for %v\n got %s\nwant %s", c.Argv, gj, wj)
			}
		})
	}
	// --unet lands on the key the mode uses and leaves the other alone, DefaultFiles untouched.
	o, _ := parseGen("gen", []string{"--prompt", "p", "--unet", "U"})
	s, _ := specFromArgs(o, nil)
	if s.Files["fl2va"] != "U" || s.Files["ref2va"] != comfy.DefaultFiles["ref2va"] {
		t.Errorf("t2v --unet: %v", s.Files)
	}
	o, _ = parseGen("gen", []string{"--prompt", "p", "--mode", "r2v", "--unet", "U"})
	s, _ = specFromArgs(o, nil)
	if s.Files["ref2va"] != "U" || s.Files["fl2va"] != comfy.DefaultFiles["fl2va"] {
		t.Errorf("r2v --unet: %v", s.Files)
	}
	if comfy.DefaultFiles["fl2va"] == "U" || comfy.DefaultFiles["ref2va"] == "U" {
		t.Error("DefaultFiles must not be modified")
	}
	// --prompt-file wins over --prompt; CRLF becomes LF
	pf := filepath.Join(t.TempDir(), "p.txt")
	if err := os.WriteFile(pf, []byte("two\r\nlines\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o, _ = parseGen("gen", []string{"--prompt", "inline", "--prompt-file", pf})
	if s, err = specFromArgs(o, nil); err != nil {
		t.Fatal(err)
	}
	if s.Prompt != "two\nlines\n" {
		t.Errorf("prompt file: %q", s.Prompt)
	}
}

// dryRunCases are the exact command lines the fixtures were produced with (python -m h3gen gen --dry-run ...).
var dryRunCases = map[string][]string{
	"dryrun_t2v.json": {"gen", "--dry-run", "--prompt", "A cat on a beach <d>[ja]こんにちは</d> & \"quotes\"",
		"--seconds", "4", "--steps", "8"},
	"dryrun_r2v.json": {"gen", "--dry-run", "--mode", "r2v", "--ref-image", "anchor.png", "--ref-image", "second.png",
		"--ref-audio", "voice.wav", "--prompt", "<Picture 1> speaks <d>[ja]こんにちは</d>", "--seconds", "9.4",
		"--steps", "4", "--seed", "7", "--shift-video", "8", "--attention", "pytorch attention",
		"--unet", "custom_ref.safetensors", "--prefix", "aitv/seg"},
	"dryrun_i2v.json": {"gen", "--dry-run", "--mode", "i2v", "--first-frame", "f.png", "--last-frame", "l.png",
		"--prompt", "x", "--no-turbo", "--width", "640", "--height", "352", "--mp", "1.0"},
}

// TestDryRun checks that --dry-run prints indented, unescaped JSON equal (as values) to what the Python
// CLI printed for the same command line.
func TestDryRun(t *testing.T) {
	for fixture, argv := range dryRunCases {
		t.Run(fixture, func(t *testing.T) {
			out, errBuf := capture(t)
			if code := run(argv); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errBuf.String())
			}
			var got, want map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("dry-run output is not JSON: %v\n%s", err, out.String())
			}
			raw, err := os.ReadFile(filepath.Join("testdata", fixture))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("graph differs from the Python dry-run:\n got %s\nwant %s", out.String(), raw)
			}
			if strings.Contains(out.String(), `\u003c`) || strings.Contains(out.String(), `\u0026`) {
				t.Error("HTML escaping must be off")
			}
			if !strings.Contains(out.String(), "\n \"") {
				t.Error("output should be indented with one space")
			}
		})
	}
	// the prompt's <d> tags and quotes survive verbatim
	out, _ := capture(t)
	run(dryRunCases["dryrun_t2v.json"])
	if !strings.Contains(out.String(), `"prompt": "A cat on a beach <d>[ja]こんにちは</d> & \"quotes\""`) {
		t.Errorf("prompt not verbatim in:\n%s", out.String())
	}
	// the seed reaches the graph up to 2^64-1 (what ComfyUI accepts)
	out, _ = capture(t)
	if code := run([]string{"gen", "--prompt", "x", "--dry-run", "--seed", "18446744073709551615"}); code != 0 ||
		!strings.Contains(out.String(), "\"noise_seed\": 18446744073709551615") {
		t.Errorf("--seed 2^64-1: code %d, stdout %q", code, out.String())
	}
}

func TestRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output", "runs.jsonl")
	gen := map[string]any{
		"ts": "2026-09-17T01:02:03", "tag": "", "mode": "r2v", "w": 832, "h": 480, "frames": 124, "steps": 8,
		"turbo": true, "seed": uint64(1<<64 - 1), "attention": nullable(""), "ok": true, "seconds": round1(87.04),
		"node_times": roundedTimes(map[string]float64{"clip": 12.55, "sample": 60.0, "vae_a": 0.26}),
		"outputs":    []string{`C:\out\h3_00001_.mp4`}, "error": nullable(""), "prompt": "<d>[ja]こんにちは</d> & \"q\"\n",
	}
	bench := map[string]any{"ts": "2026-09-17T01:02:03", "tag": "bench", "run": 1, "ok": false, "error": nullable("CUDA out of memory")}
	for _, e := range []map[string]any{gen, bench} {
		if err := record(path, e); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want two lines, got %q", raw)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got["attention"] != nil || got["seed"] != float64(1<<64-1) || got["prompt"] != "<d>[ja]こんにちは</d> & \"q\"\n" ||
		got["node_times"].(map[string]any)["vae_a"] != 0.3 || got["seconds"] != 87.0 {
		t.Errorf("gen line: %s", lines[0])
	}
	if strings.Contains(lines[0], `\u003c`) || !strings.Contains(lines[0], `"seed":18446744073709551615`) {
		t.Errorf("gen line must not HTML-escape and must keep the seed's digits: %s", lines[0])
	}
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatal(err)
	}
	if got["error"] != "CUDA out of memory" || got["run"] != 1.0 {
		t.Errorf("bench line: %s", lines[1])
	}
}

func TestHelpers(t *testing.T) {
	s := strings.Repeat("あ", 299) + "いう"
	if got := truncRunes(s, 300); len([]rune(got)) != 300 || !strings.HasSuffix(got, "い") {
		t.Errorf("truncRunes: %d runes, ends %q", len([]rune(got)), got[len(got)-3:])
	}
	if got := truncRunes("short", 300); got != "short" {
		t.Errorf("truncRunes short: %q", got)
	}
	if nullable("") != nil || nullable("x") != "x" {
		t.Error("nullable")
	}
	if round1(87.04) != 87.0 || round1(12.55) != 12.6 || round1(0.26) != 0.3 {
		t.Error("round1")
	}
	got := sortedNodes(map[string]float64{"clip": 12.6, "sample": 60, "vae_a": 0.5, "vae_v": 0.5})
	if want := []string{"sample", "clip", "vae_a", "vae_v"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sortedNodes: %v want %v", got, want)
	}
	if rt := roundedTimes(map[string]float64{"a": 0.25, "b": 2.0}); rt["a"] != 0.3 || rt["b"] != 2.0 {
		t.Errorf("roundedTimes: %v", rt)
	}
	if runsLog() == "" || !strings.HasSuffix(runsLog(), filepath.Join("output", "runs.jsonl")) {
		t.Errorf("runsLog: %q", runsLog())
	}
}

// fakeComfy answers /system_stats and rejects everything the client does next in the requested way: the
// websocket handshake (404) or the graph (/prompt 400 with ComfyUI's error JSON).
func fakeComfy(t *testing.T, mode string) (host string, port int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"system": {"os": "nt", "ram_total": 137000000000, "argv": ["C:\\テスト\\main.py"], "html": "<a&b>"}, "devices": [{"name": "d", "vram_free": 15000000000.0, "index": 0}]}`))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if mode == "ws" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		// accept the upgrade and hold the socket open until the client goes away
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			base64.StdEncoding.EncodeToString(accept[:]))
		buf := make([]byte, 1)
		for {
			if _, err := conn.Read(buf); err != nil {
				conn.Close()
				return
			}
		}
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error": {"type": "invalid_prompt", "message": "Cannot execute because a node is missing the class_type property.", "details": "Node ID '#save'", "extra_info": {}}, "node_errors": {}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	port, _ = strconv.Atoi(u.Port())
	return u.Hostname(), port
}

// A refused websocket or a rejected graph is a client error: the CLI prints it on stderr and exits 1 with
// nothing appended to runs.jsonl and no "[h3gen] FAILED" line.
func TestGenClientErrorNotRecorded(t *testing.T) {
	for _, mode := range []string{"ws", "prompt"} {
		host, port := fakeComfy(t, mode)
		log := filepath.Join(t.TempDir(), "output", "runs.jsonl")
		old := runsLog
		runsLog = func() string { return log }
		t.Cleanup(func() { runsLog = old })
		out, errBuf := capture(t)
		code := run([]string{"gen", "--host", host, "--port", strconv.Itoa(port), "--prompt", "probe", "--tag", "probe"})
		if code != 1 || !strings.HasPrefix(errBuf.String(), "h3gen: ") {
			t.Errorf("%s: exit %d stderr %q, want 1 with the error", mode, code, errBuf.String())
		}
		if strings.Contains(out.String(), "FAILED") || strings.Contains(out.String(), "[h3gen] error:") {
			t.Errorf("%s: stdout reports a failed run: %q", mode, out.String())
		}
		if !strings.HasPrefix(out.String(), "[h3gen] t2v 832x480 124f (5.17s) steps=8 turbo=true seed=0\n") {
			t.Errorf("%s: stdout lacks the banner: %q", mode, out.String())
		}
		if _, err := os.Stat(log); err == nil {
			t.Errorf("%s: runs.jsonl was written", mode)
		}
		// bench: the same, before any "[bench] run" line
		out, errBuf = capture(t)
		code = run([]string{"bench", "--host", host, "--port", strconv.Itoa(port), "--runs", "1"})
		if code != 1 || out.Len() != 0 || !strings.HasPrefix(errBuf.String(), "h3gen: ") {
			t.Errorf("bench %s: exit %d, stdout %q, stderr %q", mode, code, out.String(), errBuf.String())
		}
		if _, err := os.Stat(log); err == nil {
			t.Errorf("bench %s: runs.jsonl was written", mode)
		}
	}
}

// info prints /system_stats as indented JSON, values intact, no HTML escaping.
func TestInfo(t *testing.T) {
	host, port := fakeComfy(t, "ws")
	out, errBuf := capture(t)
	if code := run([]string{"info", "--host", host, "--port", strconv.Itoa(port)}); code != 0 || errBuf.Len() != 0 {
		t.Fatalf("info: code %d stderr %q", code, errBuf.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	sys := got["system"].(map[string]any)
	if sys["ram_total"] != 137000000000.0 || sys["html"] != "<a&b>" || !strings.Contains(out.String(), "\n \"system\"") ||
		!strings.Contains(out.String(), "テスト") || strings.Contains(out.String(), `\u003c`) {
		t.Errorf("info output:\n%s", out.String())
	}
	// an unreachable server is exit 1
	_, errBuf = capture(t)
	if code := run([]string{"info", "--host", "127.0.0.1", "--port", "1"}); code != 1 || !strings.Contains(errBuf.String(), "system_stats") {
		t.Errorf("unreachable: code %d stderr %q", code, errBuf.String())
	}
}
