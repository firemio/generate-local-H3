package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/firemio/generate-local-H3/internal/llm"
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

// clearLLMEnv unsets the variables the common flags read their defaults from.
func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AITV_LLM_URL", "AITV_LLM_MODEL", "AITV_LLM_NUM_GPU"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func commonDefaults(cmd string) config {
	return config{
		cmd:       cmd,
		comfyHost: "127.0.0.1",
		comfyPort: 8188,
		llmURL:    "http://127.0.0.1:11434",
		llmModel:  "gemma4:latest",
		width:     640,
		height:    352,
		steps:     8,
	}
}

func TestProduceDefaults(t *testing.T) {
	clearLLMEnv(t)
	capture(t)
	got, code := parse([]string{"produce"})
	if code != 0 || got == nil {
		t.Fatalf("code %d, config %v", code, got)
	}
	if want := commonDefaults("produce"); !reflect.DeepEqual(*got, want) {
		t.Errorf("got %+v\nwant %+v", *got, want)
	}
	if got.seed.v != nil || countOf(got) != nil {
		t.Errorf("seed and count should be nil (random / forever) when not given: %v %v", got.seed.v, countOf(got))
	}
}

func TestProduceAllFlags(t *testing.T) {
	clearLLMEnv(t)
	capture(t)
	got, code := parse([]string{"produce", "--count", "3", "--formats", "news,nature", "--minutes", "6",
		"--width", "832", "--height=480", "--steps", "4", "--no-turbo", "--seed", "42",
		"--llm-model", "x", "--llm-url", "http://127.0.0.1:1234/v1", "--llm-gpu-layers", "2",
		"--comfy-host", "h", "--comfy-port", "1", "--lineup", "a,b"})
	if code != 0 || got == nil {
		t.Fatalf("code %d, config %v", code, got)
	}
	seed := int64(42)
	want := config{
		cmd:          "produce",
		comfyHost:    "h",
		comfyPort:    1,
		llmURL:       "http://127.0.0.1:1234/v1",
		llmGPULayers: 2,
		llmModel:     "x",
		width:        832,
		height:       480,
		steps:        4,
		noTurbo:      true,
		seed:         optInt64{&seed},
		count:        3,
		formats:      "news,nature",
		lineup:       "a,b",
		minutes:      6,
	}
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got %+v\nwant %+v", *got, want)
	}
	if n := countOf(got); n == nil || *n != 3 {
		t.Errorf("countOf = %v, want 3", n)
	}
}

// --llm-gpu-layers reaches the writer as given (-1 is Ollama's every-layer value), the environment only
// supplies the default.
func TestGPULayers(t *testing.T) {
	capture(t)
	t.Setenv("AITV_LLM_NUM_GPU", "5")
	got, _ := parse([]string{"produce", "--llm-gpu-layers", "-1"})
	if l := llm.New(got.llmURL, got.llmModel, &got.llmGPULayers); l.NumGPU != -1 {
		t.Errorf("writer NumGPU = %d, want -1", l.NumGPU)
	}
	got, _ = parse([]string{"produce"})
	if got.llmGPULayers != 5 {
		t.Errorf("AITV_LLM_NUM_GPU=5 should be the default, got %d", got.llmGPULayers)
	}
	t.Setenv("AITV_LLM_NUM_GPU", "abc")
	if got, code := parse([]string{"produce"}); code != 0 || got.llmGPULayers != 0 {
		t.Errorf("a non-integer AITV_LLM_NUM_GPU counts as unset: code %d, layers %d", code, got.llmGPULayers)
	}
}

func TestSeed(t *testing.T) {
	capture(t)
	got, _ := parse([]string{"produce", "--seed", "0"})
	if got.seed.v == nil || *got.seed.v != 0 {
		t.Errorf("--seed 0 must be an explicit seed 0, got %v", got.seed.v)
	}
	got, _ = parse([]string{"produce", "--seed", "0", "--seed=7"})
	if got.seed.v == nil || *got.seed.v != 7 {
		t.Errorf("last --seed wins, got %v", got.seed.v)
	}
	got, _ = parse([]string{"produce", "--seed", "-5"})
	if got.seed.v == nil || *got.seed.v != -5 {
		t.Errorf("negative seed value, got %v", got.seed.v)
	}
	if got, code := parse([]string{"produce", "--seed", "x"}); got != nil || code != 2 {
		t.Errorf("--seed x: (%v, %d), want a usage error", got, code)
	}
}

func TestEnvDefaults(t *testing.T) {
	capture(t)
	t.Setenv("AITV_LLM_URL", "http://127.0.0.1:1234/v1")
	t.Setenv("AITV_LLM_MODEL", "qwen3:8b")
	t.Setenv("AITV_LLM_NUM_GPU", "99")
	got, code := parse([]string{"station"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	if got.llmURL != "http://127.0.0.1:1234/v1" || got.llmModel != "qwen3:8b" || got.llmGPULayers != 99 {
		t.Errorf("env defaults not applied: %+v", *got)
	}
	// explicit flags override the environment
	got, _ = parse([]string{"station", "--llm-url", "http://x", "--llm-model", "m", "--llm-gpu-layers", "1"})
	if got.llmURL != "http://x" || got.llmModel != "m" || got.llmGPULayers != 1 {
		t.Errorf("flags must override env: %+v", *got)
	}
}

func TestServeHLSRenormFlags(t *testing.T) {
	capture(t)
	cases := []struct {
		args []string
		want config
	}{
		{[]string{"serve"}, config{cmd: "serve", host: "127.0.0.1", port: 8800}},
		{[]string{"serve", "--host", "0.0.0.0", "--port=8801"}, config{cmd: "serve", host: "0.0.0.0", port: 8801}},
		{[]string{"hls"}, config{cmd: "hls", segment: 2}},
		{[]string{"hls", "--segment", "4"}, config{cmd: "hls", segment: 4}},
		{[]string{"renorm"}, config{cmd: "renorm"}},
		{[]string{"renorm", "--dry-run", "--limit", "5"}, config{cmd: "renorm", dryRun: true, limit: 5}},
	}
	for _, tc := range cases {
		got, code := parse(tc.args)
		if code != 0 || got == nil {
			t.Errorf("%v: code %d", tc.args, code)
			continue
		}
		if !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("%v: got %+v\nwant %+v", tc.args, *got, tc.want)
		}
	}
}

// TestStationAsLaunched is the exact argument list scripts/run_station.ps1 builds (with -Formats
// news,nature -Minutes 6 -NoTurbo). --no-turbo takes no value and may be followed by nothing.
func TestStationAsLaunched(t *testing.T) {
	clearLLMEnv(t)
	capture(t)
	got, code := parse([]string{"station", "--host", "127.0.0.1", "--port", "8800", "--width", "640",
		"--height", "352", "--steps", "8", "--llm-model", "gemma4:latest", "--formats", "news,nature",
		"--minutes", "6", "--no-turbo"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	want := commonDefaults("station")
	want.host, want.port = "127.0.0.1", 8800
	want.formats, want.minutes, want.noTurbo = "news,nature", 6, true
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got %+v\nwant %+v", *got, want)
	}
}

func TestStationFlags(t *testing.T) {
	clearLLMEnv(t)
	capture(t)
	got, code := parse([]string{"station", "--no-hls", "--count", "1"})
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	want := commonDefaults("station")
	want.host, want.port = "127.0.0.1", 8800
	want.noHLS, want.count = true, 1
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("got %+v\nwant %+v", *got, want)
	}
	// a bool flag in the middle does not swallow the next flag
	got, _ = parse([]string{"station", "--no-turbo", "--formats", "news", "--no-hls", "--no-produce", "--port", "8801"})
	if !got.noTurbo || got.formats != "news" || !got.noHLS || !got.noProduce || got.port != 8801 {
		t.Errorf("bool flags must take no value: %+v", *got)
	}
	// a value with a space, given as --name=value
	if got, _ := parse([]string{"produce", "--formats=a b"}); got.formats != "a b" {
		t.Errorf("--formats=a b: %q", got.formats)
	}
}

// A usage error prints the usage on stderr, nothing on stdout, exit 2; the hook never runs.
func TestUsageErrors(t *testing.T) {
	cases := []struct {
		args []string
		msg  string
	}{
		{nil, "usage: aitv"},
		{[]string{"bogus"}, `unknown command "bogus"`},
		{[]string{"help"}, `unknown command "help"`},
		{[]string{"serve", "--bogus"}, "flag provided but not defined: -bogus"},
		{[]string{"serve", "extra"}, `unexpected argument "extra"`},
		{[]string{"serve", "--port", "x"}, `invalid value "x" for flag -port`},
		{[]string{"produce", "--minutes"}, "flag needs an argument: -minutes"},
		{[]string{"produce", "--steps", "0x"}, `invalid value "0x" for flag -steps`},
		{[]string{"produce", "--min", "6"}, "flag provided but not defined: -min"}, // no abbreviations
		{[]string{"station", "--no"}, "flag provided but not defined: -no"},
	}
	for _, tc := range cases {
		out, errb := capture(t)
		c, code := parse(tc.args)
		if c != nil || code != 2 {
			t.Errorf("%v: want (nil, 2), got (%v, %d)", tc.args, c, code)
		}
		if !strings.Contains(errb.String(), tc.msg) {
			t.Errorf("%v: stderr %q should contain %q", tc.args, errb.String(), tc.msg)
		}
		if len(tc.args) > 1 && !strings.Contains(errb.String(), "usage: aitv "+tc.args[0]+" [flags]") {
			t.Errorf("%v: stderr %q should carry the command's usage", tc.args, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("%v: nothing should go to stdout on an error, got %q", tc.args, out.String())
		}
	}
	// run() itself returns the code
	capture(t)
	if code := run([]string{"bogus"}); code != 2 {
		t.Errorf("unknown subcommand: want 2, got %d", code)
	}
	if code := run(nil); code != 2 {
		t.Errorf("no subcommand: want 2, got %d", code)
	}
}

// -h / --help print on stdout, nothing on stderr, exit 0: the subcommand list at the top level, the flag
// list for a subcommand.
func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}} {
		out, errb := capture(t)
		c, code := parse(args)
		if c != nil || code != 0 || errb.Len() != 0 {
			t.Errorf("%v: want (nil, 0), got (%v, %d) stderr %q", args, c, code, errb.String())
		}
		if !strings.Contains(out.String(), "aitv station") {
			t.Errorf("%v: usage should list the subcommands on stdout, got %q", args, out.String())
		}
	}
	for _, sub := range []string{"produce", "station", "serve", "hls", "renorm"} {
		for _, h := range []string{"-h", "--help"} {
			out, errb := capture(t)
			c, code := parse([]string{sub, h})
			if c != nil || code != 0 {
				t.Errorf("%s %s: want (nil, 0), got (%v, %d)", sub, h, c, code)
			}
			if !strings.HasPrefix(out.String(), "usage: aitv "+sub+" [flags]\n") || errb.Len() != 0 {
				t.Errorf("%s %s: help must go to stdout only, got stdout %q stderr %q", sub, h, out.String(), errb.String())
			}
		}
	}
	out, _ := capture(t)
	parse([]string{"produce", "-h"})
	if !strings.Contains(out.String(), "-no-turbo") || !strings.Contains(out.String(), "-llm-url") {
		t.Errorf("produce -h should list the common flags, got %q", out.String())
	}
}

func TestSplitListAndEnv(t *testing.T) {
	if splitList("") != nil {
		t.Error(`splitList("") must be nil (not given)`)
	}
	if got := splitList("news,nature"); !reflect.DeepEqual(got, []string{"news", "nature"}) {
		t.Errorf("got %v", got)
	}
	t.Setenv("AITV_TEST_ENVOR", "")
	if got := envOr("AITV_TEST_ENVOR", "def"); got != "def" {
		t.Errorf("empty counts as unset, got %q", got)
	}
	t.Setenv("AITV_TEST_ENVOR", "v")
	if got := envOr("AITV_TEST_ENVOR", "def"); got != "v" {
		t.Errorf("got %q", got)
	}
	t.Setenv("AITV_TEST_ENVINT", " 7 ")
	if got := envInt("AITV_TEST_ENVINT", 1); got != 7 {
		t.Errorf("envInt = %d, want 7", got)
	}
	t.Setenv("AITV_TEST_ENVINT", "x")
	if got := envInt("AITV_TEST_ENVINT", 1); got != 1 {
		t.Errorf("envInt on a bad value = %d, want the default", got)
	}
}

// station binds the web port before anything else: when another process holds it, the error is printed
// and the exit code is 1, with no producer / broadcaster started. serve fails the same way.
func TestStationPortTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	for _, args := range [][]string{{"station", "--port", port, "--no-hls"}, {"serve", "--port", port}} {
		_, errb := capture(t)
		done := make(chan int, 1)
		go func() { done <- run(args) }()
		select {
		case code := <-done:
			if code != 1 || !strings.HasPrefix(errb.String(), "aitv: ") {
				t.Errorf("%v: want exit 1 with the error on stderr, got %d %q", args, code, errb.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v did not return after a failed bind (it must not start the producer)", args)
		}
	}
}

func TestRecovering(t *testing.T) {
	_, errb := capture(t)
	err := recovering(func() error { panic("TODO") })
	if err == nil || err.Error() != "panic: TODO" {
		t.Errorf("a panic becomes an error, got %v", err)
	}
	err = recovering(func() error { var m map[string]int; m["x"] = 1; return nil })
	if err == nil || !strings.Contains(err.Error(), "assignment to entry in nil map") {
		t.Errorf("runtime panics are recovered, got %v", err)
	}
	if !strings.Contains(errb.String(), "TestRecovering") {
		t.Errorf("the stack of a recovered panic goes to stderr, got %q", errb.String())
	}
	if err := recovering(func() error { return errors.New("plain") }); err == nil || err.Error() != "plain" {
		t.Errorf("errors pass through, got %v", err)
	}
	if err := recovering(func() error { return nil }); err != nil {
		t.Errorf("got %v", err)
	}
}

// TestRestartComfyHook launches a stand-in run_comfyui.ps1 under a temp root and checks the detached
// launch, the append-mode log files and the log line. Windows only (powershell.exe).
func TestRestartComfyHook(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "Write-Output \"fake comfyui stdout\"\n[Console]::Error.WriteLine(\"fake comfyui stderr\")\n"
	if err := os.WriteFile(filepath.Join(root, "scripts", "run_comfyui.ps1"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	var lines []string
	hook := restartComfyHook(root, func(format string, args ...any) { lines = append(lines, format) })
	// output/ does not exist yet: the hook creates it, twice = two appended lines per file
	for i := 0; i < 2; i++ {
		t0 := time.Now()
		if err := hook(); err != nil {
			t.Fatalf("hook: %v", err)
		}
		if d := time.Since(t0); d > 5*time.Second {
			t.Errorf("hook must not wait for the child (took %v)", d)
		}
	}
	if len(lines) != 2 || lines[0] != "[aitv] launched scripts/run_comfyui.ps1" {
		t.Errorf("log lines: %q", lines)
	}
	waitFor := func(name, want string, n int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			b, _ := os.ReadFile(filepath.Join(root, "output", name))
			if strings.Count(string(b), want) >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: want %d x %q, got %q", name, n, want, string(b))
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	waitFor("comfyui.log", "fake comfyui stdout", 2)
	waitFor("comfyui.err.log", "fake comfyui stderr", 2)
}
