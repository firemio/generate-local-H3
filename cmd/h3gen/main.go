// Command h3gen is the headless MiniMax H3 generation CLI (port of h3gen/cli.py):
//
//	h3gen gen --prompt "A cat walking on a beach, waves, seagulls" --seconds 4 --steps 8
//	h3gen gen --mode i2v --first-frame my.png --prompt "..." --seconds 5
//	h3gen gen --mode r2v --ref-image anchor.png --ref-audio voice.wav --prompt "<Picture 1> ... <d>[ja]こんにちは</d>"
//	h3gen bench --seconds 4 --steps 8 --runs 2
//	h3gen info
//
// Flags of gen/bench (same names and defaults as the Python CLI): --host (COMFY_HOST or 127.0.0.1), --port
// (COMFY_PORT or 8188), --prompt, --prompt-file, --mode t2v|i2v|r2v, --aspect 16:9, --mp 0.4 (megapixel tier:
// 0.4 draft, 1.0 native), --width/--height (both required to override --aspect/--mp), --seconds 5, --seed 0
// (up to 2^64-1), --steps 8, --no-turbo, --lora-strength 1, --shift-video, --shift-audio, --attention
// ("pytorch attention" | "comfy kitchen attention"), --scheduler simple, --first-frame, --last-frame,
// --ref-image (repeatable), --ref-audio (repeatable), --ref-image-size match|max, --unet, --clip, --clip-device
// default|cpu, --prefix
// h3/h3, --download-dir, --tag, --dry-run (print the API graph as indented JSON and exit). bench adds --runs 2
// and uses a default beach prompt when none is given (seed increments per run; stops at the first failure,
// prints cold/warm mean).
//
// Flags are the standard flag package's: "--name value" and "--name=value", exact names only. An unknown
// flag, a value the flag package rejects or a stray argument prints the usage on stderr and exits 2; -h
// prints a command's flags on stdout and exits 0.
//
// gen: uploads the frame / reference files, prints "[h3gen] <mode> WxH Nf (S.SSs) steps=.. turbo=.. seed=..",
// runs, appends a JSON line to <root>/output/runs.jsonl ({ts, tag, mode, w, h, frames, steps, turbo, seed,
// unet, clip, attention, ok, seconds, node_times, outputs, error, prompt[:300]}), prints "[h3gen] OK|FAILED
// in Ns", the node times sorted descending, "[h3gen] -> <file>" per output and the error if any. Exit 1 on
// failure. A client error out of Run (a refused websocket, a rejected graph, a failed download) goes to
// stderr with exit 1 and no runs.jsonl line. info prints /system_stats as indented JSON.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/firemio/generate-local-H3/internal/comfy"
	"github.com/firemio/generate-local-H3/internal/paths"
)

const usageText = `usage: h3gen {gen,bench,info} [flags]

  h3gen gen --prompt "A cat walking on a beach, waves, seagulls" --seconds 4 --steps 8
  h3gen gen --mode i2v --first-frame my.png --prompt "..." --seconds 5
  h3gen gen --mode r2v --ref-image anchor.png --ref-audio voice.wav --prompt "<Picture 1> ... <d>[ja]こんにちは</d>"
  h3gen bench --seconds 4 --steps 8 --runs 2
  h3gen info

"h3gen <command> -h" lists the command's flags.
`

// benchPrompt is what bench generates when neither --prompt nor --prompt-file is given.
const benchPrompt = "A golden retriever runs along a sunny beach toward the camera, waves rolling in, " +
	"seagulls overhead. Camera tracks low and steady. Audio: waves, wind, distant gulls, paws on wet sand."

// stdout / stderr are the CLI's streams; tests swap them.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

// runsLog is <root>/output/runs.jsonl (a variable so the tests can point it elsewhere).
var runsLog = func() string { return filepath.Join(paths.Output(), "runs.jsonl") }

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses the subcommand and flags and returns the exit code: 0 on success (or after -h/--help), 1 on
// a failed / impossible generation, 2 on a usage error.
func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	cmd := args[0]
	switch cmd {
	case "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return 0
	case "gen", "bench":
		o, code := parseGen(cmd, args[1:])
		if o == nil {
			return code
		}
		if cmd == "gen" {
			return cmdGen(o)
		}
		return cmdBench(o)
	case "info":
		fs := flag.NewFlagSet("h3gen info", flag.ContinueOnError)
		host := fs.String("host", "127.0.0.1", "ComfyUI host")
		port := fs.Int("port", 8188, "ComfyUI port")
		if code, ok := parseFlags(fs, args[1:]); !ok {
			return code
		}
		return cmdInfo(*host, *port)
	default:
		fmt.Fprint(stderr, usageText)
		fmt.Fprintf(stderr, "h3gen: unknown command %q\n", cmd)
		return 2
	}
}

// genOpts is the parsed command line of gen / bench.
type genOpts struct {
	host         string
	port         int
	prompt       string
	promptFile   string
	mode         string
	aspect       string
	mp           float64
	width        int // 0 = not given
	height       int
	seconds      float64
	seed         uint64
	steps        int
	noTurbo      bool
	loraStrength float64
	shiftVideo   optFloat
	shiftAudio   optFloat
	attention    string
	scheduler    string
	firstFrame   string
	lastFrame    string
	refImage     stringList
	refAudio     stringList
	refImageSize string
	unet         string
	clip         string
	clipDevice   string
	prefix       string
	downloadDir  string
	tag          string
	dryRun       bool
	runs         int // bench only
}

// parseGen parses the gen / bench command line. It returns (nil, code) when there is nothing to run.
func parseGen(cmd string, args []string) (*genOpts, int) {
	o := &genOpts{mode: "t2v", refImageSize: "match", clipDevice: "default"}
	fs := flag.NewFlagSet("h3gen "+cmd, flag.ContinueOnError)
	fs.StringVar(&o.host, "host", envOr("COMFY_HOST", "127.0.0.1"), "ComfyUI host (COMFY_HOST)")
	fs.IntVar(&o.port, "port", envInt("COMFY_PORT", 8188), "ComfyUI port (COMFY_PORT)")
	fs.StringVar(&o.prompt, "prompt", "", "the prompt")
	fs.StringVar(&o.promptFile, "prompt-file", "", "read the prompt from this UTF-8 file")
	fs.Var(choice{&o.mode, []string{"t2v", "i2v", "r2v"}}, "mode", "t2v | i2v | r2v")
	fs.StringVar(&o.aspect, "aspect", "16:9", "16:9 | 9:16 | 1:1")
	fs.Float64Var(&o.mp, "mp", 0.4, "megapixels tier: 0.4 draft, 1.0 native")
	fs.IntVar(&o.width, "width", 0, "override --aspect/--mp (needs --height too)")
	fs.IntVar(&o.height, "height", 0, "override --aspect/--mp (needs --width too)")
	fs.Float64Var(&o.seconds, "seconds", 5.0, "clip length (snapped to the 17k+5 frame grid)")
	fs.Uint64Var(&o.seed, "seed", 0, "noise seed (0 .. 2^64-1)")
	fs.IntVar(&o.steps, "steps", 8, "sampler steps")
	fs.BoolVar(&o.noTurbo, "no-turbo", false, "disable turbo LoRA (use 20 steps)")
	fs.Float64Var(&o.loraStrength, "lora-strength", 1.0, "strength_model of the turbo LoRA")
	fs.Var(&o.shiftVideo, "shift-video", "MiniMaxH3SigmaShift shift_video (default: model default 12.0)")
	fs.Var(&o.shiftAudio, "shift-audio", "MiniMaxH3SigmaShift shift_audio (default: model default 3.0)")
	fs.Var(choice{&o.attention, []string{"pytorch attention", "comfy kitchen attention"}}, "attention",
		"\"pytorch attention\" | \"comfy kitchen attention\"")
	fs.StringVar(&o.scheduler, "scheduler", "simple", "BasicScheduler scheduler")
	fs.StringVar(&o.firstFrame, "first-frame", "", "first frame image (i2v)")
	fs.StringVar(&o.lastFrame, "last-frame", "", "last frame image (i2v)")
	fs.Var(&o.refImage, "ref-image", "reference image (r2v; repeatable)")
	fs.Var(&o.refAudio, "ref-audio", "reference audio (r2v; repeatable)")
	fs.Var(choice{&o.refImageSize, []string{"match", "max"}}, "ref-image-size", "match | max")
	fs.StringVar(&o.unet, "unet", "", "override diffusion model filename")
	fs.StringVar(&o.clip, "clip", "", "override text encoder filename")
	fs.Var(choice{&o.clipDevice, []string{"default", "cpu"}}, "clip-device",
		"default | cpu (cpu keeps the 26 GB text encoder out of VRAM; see docs/pitfalls.md §8)")
	fs.StringVar(&o.prefix, "prefix", "h3/h3", "SaveVideo filename_prefix")
	fs.StringVar(&o.downloadDir, "download-dir", "", "copy outputs here via /view (only needed for a remote ComfyUI)")
	fs.StringVar(&o.tag, "tag", "", "free-form label written to runs.jsonl")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the API graph and exit")
	if cmd == "bench" {
		fs.IntVar(&o.runs, "runs", 2, "number of runs")
	}
	if code, ok := parseFlags(fs, args); !ok {
		return nil, code
	}
	return o, 0
}

// parseFlags parses args with fs. After -h / --help it prints the flag list on stdout and returns
// (0, false); on an unknown flag, a value the flag package rejects or a stray argument it prints the error
// and the flag list on stderr and returns (2, false).
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	fs.SetOutput(io.Discard) // the usage is printed below, on the right stream
	err := fs.Parse(args)
	if err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err == nil {
		return 0, true
	}
	w, code := stderr, 2
	if errors.Is(err, flag.ErrHelp) {
		w, code = stdout, 0
	} else {
		fmt.Fprintf(w, "%s: %v\n", fs.Name(), err)
	}
	fs.SetOutput(w)
	fmt.Fprintf(w, "usage: %s [flags]\n", fs.Name())
	fs.PrintDefaults()
	return code, false
}

// optFloat is a float64 flag that tells "not given" from 0: v is nil until the flag is set.
type optFloat struct{ v *float64 }

func (o *optFloat) String() string {
	if o == nil || o.v == nil {
		return ""
	}
	return strconv.FormatFloat(*o.v, 'g', -1, 64)
}

func (o *optFloat) Set(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	o.v = &f
	return nil
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}

// choice is a string flag restricted to a fixed set of values.
type choice struct {
	p       *string
	allowed []string
}

func (c choice) String() string {
	if c.p == nil {
		return ""
	}
	return *c.p
}

func (c choice) Set(s string) error {
	if !slices.Contains(c.allowed, s) {
		return fmt.Errorf("must be one of: %s", strings.Join(c.allowed, ", "))
	}
	*c.p = s
	return nil
}

// envOr is the environment variable's value when it is set and not empty, else def.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envInt is envOr for an int; a value that is not an int counts as unset.
func envInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil {
		return n
	}
	return def
}

// specFromArgs builds the GenSpec: the prompt (inline or from --prompt-file), the canvas (--width and
// --height together, else Canvas(--aspect, --mp)), the model file overrides (--unet replaces ref2va for
// r2v and fl2va otherwise; --clip replaces clip) and the frame / reference files. With a client and not
// --dry-run the files are uploaded and the uploaded names go into the spec; otherwise the paths are used
// as given (the dry-run graph shows them).
func specFromArgs(o *genOpts, client *comfy.Client) (comfy.GenSpec, error) {
	prompt := o.prompt
	if o.promptFile != "" {
		b, err := os.ReadFile(o.promptFile)
		if err != nil {
			return comfy.GenSpec{}, err
		}
		prompt = strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	if prompt == "" {
		return comfy.GenSpec{}, errors.New("--prompt or --prompt-file required")
	}
	if math.IsNaN(o.seconds) || math.IsInf(o.seconds, 0) {
		return comfy.GenSpec{}, errors.New("--seconds must be a finite number")
	}
	spec := comfy.NewGenSpec(prompt)
	spec.Mode = o.mode
	spec.Width, spec.Height = o.width, o.height
	if o.width == 0 || o.height == 0 {
		spec.Width, spec.Height = comfy.Canvas(o.aspect, o.mp)
	}
	spec.Seconds = o.seconds
	spec.Seed = o.seed
	spec.Steps = o.steps
	spec.Turbo = !o.noTurbo
	spec.LoraStrength = o.loraStrength
	spec.ShiftVideo, spec.ShiftAudio = o.shiftVideo.v, o.shiftAudio.v
	spec.Attention = o.attention
	spec.Scheduler = o.scheduler
	spec.RefImageSize = o.refImageSize
	spec.ClipDevice = o.clipDevice
	spec.FilenamePrefix = o.prefix
	spec.Files = maps.Clone(comfy.DefaultFiles)
	if o.unet != "" {
		spec.Files[unetKey(o.mode)] = o.unet
	}
	if o.clip != "" {
		spec.Files["clip"] = o.clip
	}
	spec.FirstFrame, spec.LastFrame = o.firstFrame, o.lastFrame
	spec.RefImages = append([]string{}, o.refImage...)
	spec.RefAudios = append([]string{}, o.refAudio...)
	if client == nil || o.dryRun {
		return spec, nil
	}
	upload := func(dst *string, kind string) error {
		if *dst == "" {
			return nil
		}
		name, err := client.Upload(*dst, kind, "h3gen", true)
		if err != nil {
			return fmt.Errorf("upload %s: %w", *dst, err)
		}
		*dst = name
		return nil
	}
	if err := cmp.Or(upload(&spec.FirstFrame, "image"), upload(&spec.LastFrame, "image")); err != nil {
		return comfy.GenSpec{}, err
	}
	for i := range spec.RefImages {
		if err := upload(&spec.RefImages[i], "image"); err != nil {
			return comfy.GenSpec{}, err
		}
	}
	for i := range spec.RefAudios {
		if err := upload(&spec.RefAudios[i], "audio"); err != nil {
			return comfy.GenSpec{}, err
		}
	}
	return spec, nil
}

// unetKey is the DefaultFiles entry the mode's diffusion model comes from.
func unetKey(mode string) string {
	if mode == "r2v" {
		return "ref2va"
	}
	return "fl2va"
}

// fail prints err on stderr and returns exit code 1.
func fail(err error) int {
	fmt.Fprintf(stderr, "h3gen: %v\n", err)
	return 1
}

// printJSON prints v on stdout as indented JSON, without HTML escaping (prompts carry <d> tags).
func printJSON(v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		return fail(err)
	}
	return 0
}

// record appends entry to path as one JSON line, creating the directory.
func record(path string, entry map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	err = enc.Encode(entry)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// timestamp is the local time as 2006-01-02T15:04:05.
func timestamp() string { return time.Now().Format("2006-01-02T15:04:05") }

// nullable is a string recorded as null when empty (attention, error).
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// round1 rounds to one decimal.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// roundedTimes is node_times with every value rounded to one decimal.
func roundedTimes(m map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = round1(v)
	}
	return out
}

// sortedNodes lists the node ids of node_times by descending time, ties by id.
func sortedNodes(m map[string]float64) []string {
	ids := slices.Collect(maps.Keys(m))
	slices.SortFunc(ids, func(a, b string) int { return cmp.Or(cmp.Compare(m[b], m[a]), strings.Compare(a, b)) })
	return ids
}

// truncRunes is s cut to n code points.
func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func cmdGen(o *genOpts) int {
	var client *comfy.Client
	if !o.dryRun {
		client = comfy.New(o.host, o.port, "")
	}
	spec, err := specFromArgs(o, client)
	if err != nil {
		return fail(err)
	}
	graph := comfy.Build(spec)
	if o.dryRun {
		return printJSON(graph)
	}
	if !client.Alive() {
		return fail(fmt.Errorf("ComfyUI not reachable at %s (start scripts\\run_comfyui.ps1)", client.Base))
	}
	length := spec.Length()
	fmt.Fprintf(stdout, "[h3gen] %s %dx%d %df (%.2fs) steps=%d turbo=%t seed=%d\n",
		spec.Mode, spec.Width, spec.Height, length, comfy.FramesSeconds(length), spec.Steps, spec.Turbo, spec.Seed)
	res, err := client.Run(context.Background(), graph, comfy.RunOptions{DownloadDir: o.downloadDir})
	if err != nil { // a client error: nothing recorded
		return fail(err)
	}
	entry := map[string]any{
		"ts": timestamp(), "tag": o.tag, "mode": spec.Mode, "w": spec.Width, "h": spec.Height,
		"frames": length, "steps": spec.Steps, "turbo": spec.Turbo, "seed": spec.Seed,
		"unet": spec.Files[unetKey(spec.Mode)], "clip": spec.Files["clip"], "attention": nullable(spec.Attention),
		"ok": res.OK, "seconds": round1(res.Seconds), "node_times": roundedTimes(res.NodeTimes),
		"outputs": res.LocalFiles, "error": nullable(res.Err), "prompt": truncRunes(spec.Prompt, 300),
	}
	if err := record(runsLog(), entry); err != nil {
		return fail(fmt.Errorf("cannot write %s: %w", runsLog(), err))
	}
	status := "FAILED"
	if res.OK {
		status = "OK"
	}
	fmt.Fprintf(stdout, "[h3gen] %s in %.1fs\n", status, res.Seconds)
	for _, id := range sortedNodes(res.NodeTimes) {
		fmt.Fprintf(stdout, "    %-10s %8.1fs\n", id, res.NodeTimes[id])
	}
	for _, f := range res.LocalFiles {
		fmt.Fprintf(stdout, "[h3gen] -> %s\n", f)
	}
	if res.Err != "" {
		fmt.Fprintf(stdout, "[h3gen] error: %s\n", res.Err)
	}
	if res.OK {
		return 0
	}
	return 1
}

func cmdBench(o *genOpts) int {
	client := comfy.New(o.host, o.port, "")
	if !client.Alive() {
		return fail(errors.New("ComfyUI not reachable"))
	}
	if o.prompt == "" && o.promptFile == "" {
		o.prompt = benchPrompt
	}
	tag := o.tag
	if tag == "" {
		tag = "bench"
	}
	var times []float64
	for i := 0; i < o.runs; i++ {
		o.seed += uint64(i)
		spec, err := specFromArgs(o, client)
		if err != nil {
			return fail(err)
		}
		res, err := client.Run(context.Background(), comfy.Build(spec), comfy.RunOptions{DownloadDir: o.downloadDir})
		if err != nil {
			return fail(err)
		}
		times = append(times, res.Seconds)
		entry := map[string]any{
			"ts": timestamp(), "tag": tag, "run": i, "mode": spec.Mode, "w": spec.Width, "h": spec.Height,
			"frames": spec.Length(), "steps": spec.Steps, "turbo": spec.Turbo, "attention": nullable(spec.Attention),
			"ok": res.OK, "seconds": round1(res.Seconds), "node_times": roundedTimes(res.NodeTimes),
			"error": nullable(res.Err),
		}
		if err := record(runsLog(), entry); err != nil {
			return fail(fmt.Errorf("cannot write %s: %w", runsLog(), err))
		}
		status := "FAIL"
		if res.OK {
			status = "OK"
		}
		var top []string
		for _, id := range sortedNodes(res.NodeTimes)[:min(4, len(res.NodeTimes))] {
			top = append(top, fmt.Sprintf("%s=%.0fs", id, res.NodeTimes[id]))
		}
		fmt.Fprintf(stdout, "[bench] run %d: %s %.1fs  %s\n", i, status, res.Seconds, strings.Join(top, " "))
		if !res.OK {
			fmt.Fprintf(stdout, "[bench] error: %s\n", res.Err)
			return 1
		}
	}
	if len(times) > 1 {
		sum := 0.0
		for _, t := range times[1:] {
			sum += t
		}
		fmt.Fprintf(stdout, "[bench] first(cold)=%.1fs  warm mean=%.1fs\n", times[0], sum/float64(len(times)-1))
	}
	return 0
}

// cmdInfo prints /system_stats as indented JSON.
func cmdInfo(host string, port int) int {
	client := comfy.New(host, port, "")
	stats, err := client.SystemStats()
	if err != nil {
		return fail(fmt.Errorf("%s/system_stats: %w", client.Base, err))
	}
	return printJSON(stats)
}
