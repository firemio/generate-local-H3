// Command aitv is the station (port of aitv/__main__.py):
//
//	aitv produce [--count N] [--formats news,nature,...] [--minutes 6] [--width 640 --height 352 --steps 8]
//	aitv serve  [--host 127.0.0.1] [--port 8800]        # web player + playlist + HLS files
//	aitv hls    [--segment 2]                          # ffmpeg live HLS broadcaster (needs produced clips)
//	aitv station [--count N] [--no-hls] [--no-produce] # serve + hls + produce, all in one process (24/7)
//	aitv renorm [--dry-run] [--limit N]                # bring the existing library to one loudness (audio only)
//
// Common flags (produce, station), same names and defaults as the Python CLI so scripts/run_station.ps1
// keeps working: --comfy-host 127.0.0.1, --comfy-port 8188, --llm-url (AITV_LLM_URL or
// http://127.0.0.1:11434; an Ollama root or an OpenAI-compatible /v1 base), --llm-gpu-layers
// (AITV_LLM_NUM_GPU or 0), --llm-model (AITV_LLM_MODEL or gemma4:latest), --width 640, --height 352,
// --steps 8, --no-turbo, --seed, --count (0 = forever), --formats, --lineup, --minutes 0.
//
// Flags are the standard flag package's: "--name value" and "--name=value", exact names only. An unknown
// flag, a value the flag package rejects or a stray argument prints the usage on stderr and exits 2; -h
// prints a command's flags on stdout and exits 0; a command that fails exits 1.
//
// station: bind the web port first (fails fast when another station holds it), run the HLS broadcaster in
// a goroutine that restarts it after 10 s on an error or a panic ("[aitv] HLS broadcaster crashed (...);
// restarting in 10s"), and run the producer in a loop that recreates it 30 s after an error or a panic
// ("[aitv] producer crashed (...); restarting in 30s"). A finished --count leaves the station playing
// ("[aitv] production finished; station keeps playing (Ctrl+C to stop)"). --no-produce never starts the
// producer: the library airs as it is and neither ComfyUI nor the writer LLM is contacted ("[aitv]
// production off (--no-produce); airing the library (Ctrl+C to stop)"). Ctrl+C / SIGTERM cancels the
// context, which the render loop consults (a running comfy.Run returns at once) and stops the ffmpeg
// children. On Windows the producer gets a RestartComfy hook that launches powershell.exe -NoProfile
// -ExecutionPolicy Bypass -File <root>/scripts/run_comfyui.ps1 detached, with stdout/stderr appended to
// <root>/output/comfyui.log and comfyui.err.log, no console window (winproc.NoWindow), logging
// "[aitv] launched scripts/run_comfyui.ps1".
//
// Every start logs "[aitv] writer LLM: <model> @ <url>" (+ "  (not answering yet; will wait)").
// Stdout is line-buffered UTF-8 (logx), so Japanese captions in log lines are safe when redirected.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/firemio/generate-local-H3/internal/comfy"
	"github.com/firemio/generate-local-H3/internal/hls"
	"github.com/firemio/generate-local-H3/internal/llm"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/loudness"
	"github.com/firemio/generate-local-H3/internal/paths"
	"github.com/firemio/generate-local-H3/internal/producer"
	"github.com/firemio/generate-local-H3/internal/rtmp"
	"github.com/firemio/generate-local-H3/internal/server"
	"github.com/firemio/generate-local-H3/internal/winproc"
)

const usageText = `usage: aitv {produce,serve,hls,station,renorm,rtmp} [flags]

  aitv produce [--count N] [--formats news,nature,...] [--minutes 6] [--width 640 --height 352 --steps 8]
  aitv serve  [--host 127.0.0.1] [--port 8800]        # web player + playlist + HLS files
  aitv hls    [--segment 2]                          # ffmpeg live HLS broadcaster (needs produced clips)
  aitv station [--count N] [--no-hls] [--no-produce] # serve + hls + produce, all in one process (24/7)
  aitv renorm [--dry-run] [--limit N]                # bring the existing library to one loudness (audio only)
  aitv rtmp   --url rtmp://.../<key> [--portrait]    # push the live channel to YouTube / Twitch / TikTok (AITV_RTMP_URL)

"aitv <command> -h" lists the command's flags.
`

// Restart delays of the station's supervisor loops.
const (
	hlsRestartDelay      = 10 * time.Second
	producerRestartDelay = 30 * time.Second
	// shutdownGrace bounds how long the station waits for the broadcaster / web server goroutines to
	// stop their ffmpeg children after Ctrl+C.
	shutdownGrace = 15 * time.Second
)

// stdout / stderr are the CLI's own streams (usage, errors, panic stacks); tests swap them. Log lines go
// through logx.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run parses the command line and runs the command, returning the exit code.
func run(args []string) int {
	c, code := parse(args)
	if c == nil {
		return code
	}
	return dispatch(c)
}

// config is the parsed command line of one subcommand.
type config struct {
	cmd string

	// common flags (produce, station)
	comfyHost    string
	comfyPort    int
	llmURL       string
	llmGPULayers int
	llmModel     string
	width        int
	height       int
	steps        int
	noTurbo      bool
	seed         optInt64 // not given = random
	count        int      // 0 = forever
	formats      string
	lineup       string
	minutes      float64

	// serve, station
	host string
	port int
	// station
	noHLS     bool
	noProduce bool
	// rtmp (and station --rtmp)
	rtmpOn        bool
	rtmpURL       string
	rtmpSource    string
	rtmpPortrait  bool
	rtmpNoOverlay bool
	rtmpFont      string
	rtmpFPS       int
	rtmpKbps      int
	// hls
	segment int
	// renorm
	dryRun bool
	limit  int
}

// parse turns args into a config. It returns (nil, 0) after -h / --help and (nil, 2) on a usage error: no
// or unknown subcommand, unknown flag, a value the flag package rejects, a stray argument.
func parse(args []string) (*config, int) {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return nil, 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usageText)
		return nil, 0
	}
	c := &config{cmd: args[0]}
	fs := flag.NewFlagSet("aitv "+c.cmd, flag.ContinueOnError)
	common := func() {
		fs.StringVar(&c.comfyHost, "comfy-host", "127.0.0.1", "ComfyUI host")
		fs.IntVar(&c.comfyPort, "comfy-port", 8188, "ComfyUI port")
		fs.StringVar(&c.llmURL, "llm-url", envOr("AITV_LLM_URL", "http://127.0.0.1:11434"),
			"Ollama root or an OpenAI-compatible /v1 base (LM Studio: http://127.0.0.1:1234/v1)")
		fs.IntVar(&c.llmGPULayers, "llm-gpu-layers", envInt("AITV_LLM_NUM_GPU", 0),
			"Ollama num_gpu (0 = CPU only, keeps the iGPU for H3)")
		fs.StringVar(&c.llmModel, "llm-model", envOr("AITV_LLM_MODEL", "gemma4:latest"), "writer model")
		fs.IntVar(&c.width, "width", 640, "render width (the library is always 832x480)")
		fs.IntVar(&c.height, "height", 352, "render height")
		fs.IntVar(&c.steps, "steps", 8, "sampler steps")
		fs.BoolVar(&c.noTurbo, "no-turbo", false,
			"skip the turbo LoRA (its weight merge can OOM-crash ComfyUI on this box; see docs/pitfalls.md)")
		fs.Var(&c.seed, "seed", "RNG seed (default: random)")
		fs.IntVar(&c.count, "count", 0, "number of programmes to produce (0 = forever)")
		fs.StringVar(&c.formats, "formats", "", "comma list that replaces the lineup for this run (news,nature,...)")
		fs.StringVar(&c.lineup, "lineup", "", "comma list of formats for one broadcast day (default: programs.Lineup)")
		fs.Float64Var(&c.minutes, "minutes", 0, "programme length override (0 = each format's own)")
	}
	web := func() {
		fs.StringVar(&c.host, "host", "127.0.0.1", "web server bind address")
		fs.IntVar(&c.port, "port", 8800, "web server port")
	}
	switch c.cmd {
	case "produce":
		common()
	case "serve":
		web()
	case "hls":
		fs.IntVar(&c.segment, "segment", hls.DefaultSegmentSeconds, "HLS segment length in seconds")
	case "station":
		common()
		web()
		fs.BoolVar(&c.noHLS, "no-hls", false, "do not run the HLS broadcaster")
		fs.BoolVar(&c.noProduce, "no-produce", false, "do not run the producer: air the library as it is (ComfyUI and the writer LLM are not contacted)")
		fs.BoolVar(&c.rtmpOn, "rtmp", false, "also push the channel to the ingest in AITV_RTMP_URL (or --rtmp-url)")
		fs.StringVar(&c.rtmpURL, "rtmp-url", os.Getenv("AITV_RTMP_URL"), "ingest URL with the stream key (prefer the AITV_RTMP_URL environment variable: the command line is visible to every process)")
		rtmpFlags(fs, c)
	case "rtmp":
		c.rtmpOn = true
		fs.StringVar(&c.rtmpURL, "url", os.Getenv("AITV_RTMP_URL"), "ingest URL with the stream key (rtmp://a.rtmp.youtube.com/live2/<key>); default AITV_RTMP_URL")
		rtmpFlags(fs, c)
	case "renorm":
		fs.BoolVar(&c.dryRun, "dry-run", false, "measure and report only, change nothing")
		fs.IntVar(&c.limit, "limit", 0, "only the first N clips (0 = all)")
	default:
		fmt.Fprint(stderr, usageText)
		fmt.Fprintf(stderr, "aitv: unknown command %q\n", c.cmd)
		return nil, 2
	}
	if code, ok := parseFlags(fs, args[1:]); !ok {
		return nil, code
	}
	return c, 0
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

// optInt64 is an int64 flag that tells "not given" from 0: v is nil until the flag is set.
type optInt64 struct{ v *int64 }

func (o *optInt64) String() string {
	if o == nil || o.v == nil {
		return ""
	}
	return strconv.FormatInt(*o.v, 10)
}

func (o *optInt64) Set(s string) error {
	n, err := strconv.ParseInt(s, 0, 64)
	if err != nil {
		return err
	}
	o.v = &n
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

// splitList splits a comma list; "" (not given) is nil.
func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// countOf is the producer's programme count: nil (forever) unless --count is positive.
func countOf(c *config) *int {
	if c.count <= 0 {
		return nil
	}
	return &c.count
}

// sleep waits d or until ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// dispatch runs the parsed command and returns the exit code: 1 when it failed (the error on stderr), else 0.
func dispatch(c *config) int {
	// Ctrl+C / SIGTERM cancels ctx; stop restores the default handling (a second Ctrl+C kills the process).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch c.cmd {
	case "produce":
		err = newProducer(ctx, c).Run(ctx, countOf(c), splitList(c.formats))
	case "serve":
		opt := server.Options{Host: c.host, Port: c.port}
		var l net.Listener
		if l, err = server.Listen(opt); err == nil {
			err = server.Serve(ctx, l, opt)
		}
	case "hls":
		err = hls.Broadcast(ctx, hls.Options{SegmentSeconds: c.segment})
	case "renorm":
		_, err = loudness.Renormalize(paths.Lib(), c.dryRun, c.limit, logx.Printf)
	case "rtmp":
		if c.rtmpURL == "" {
			fmt.Fprintln(stderr, "aitv rtmp: --url (or AITV_RTMP_URL) is required")
			return 2
		}
		err = rtmp.Push(ctx, rtmpOptions(c))
	case "station":
		return station(ctx, stop, c)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(stderr, "aitv: %v\n", err)
		return 1
	}
	return 0
}

// newProducer builds the ComfyUI client, the writer LLM (created even when it is not answering yet — the
// producer waits for it per programme) and the producer with the Windows restart hook, then waits for
// ComfyUI.
func newProducer(ctx context.Context, c *config) *producer.Producer {
	cl := comfy.New(c.comfyHost, c.comfyPort, "")
	l := llm.New(c.llmURL, c.llmModel, &c.llmGPULayers)
	note := ""
	if !l.Alive() {
		note = "  (not answering yet; will wait)"
	}
	logx.Printf("[aitv] writer LLM: %s @ %s%s", l.Model, l.BaseURL, note)
	var restart func() error
	if runtime.GOOS == "windows" {
		restart = restartComfyHook(paths.Root(), logx.Printf)
	}
	prod := producer.New(cl, l, producer.Options{
		Width:        c.width,
		Height:       c.height,
		Steps:        c.steps,
		Turbo:        !c.noTurbo,
		Seed:         c.seed.v,
		Lineup:       splitList(c.lineup),
		Minutes:      c.minutes,
		RestartComfy: restart,
	})
	prod.WaitForComfy(ctx)
	return prod
}

// restartComfyHook relaunches ComfyUI through the repo's own launcher, detached: powershell.exe -NoProfile
// -ExecutionPolicy Bypass -File <root>/scripts/run_comfyui.ps1, stdout/stderr appended to
// <root>/output/comfyui.log and comfyui.err.log, no console window. The child is not waited for inline (a
// goroutine reaps it) so the producer can go back to polling /system_stats.
func restartComfyHook(root string, log logx.Logger) func() error {
	return func() error {
		script := filepath.Join(root, "scripts", "run_comfyui.ps1")
		outDir := filepath.Join(root, "output")
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		const mode = os.O_APPEND | os.O_CREATE | os.O_WRONLY
		out, err := os.OpenFile(filepath.Join(outDir, "comfyui.log"), mode, 0o644)
		if err != nil {
			return err
		}
		errf, err := os.OpenFile(filepath.Join(outDir, "comfyui.err.log"), mode, 0o644)
		if err != nil {
			out.Close()
			return err
		}
		cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Stdout = out
		cmd.Stderr = errf
		winproc.NoWindow(cmd)
		if err := cmd.Start(); err != nil {
			out.Close()
			errf.Close()
			return err
		}
		go func() {
			_ = cmd.Wait()
			out.Close()
			errf.Close()
		}()
		log("[aitv] launched scripts/run_comfyui.ps1")
		return nil
	}
}

// station is `aitv station`: bind the web port first (a duplicate station fails fast here: exit 1), serve
// in a goroutine, broadcast in a self-restarting goroutine unless --no-hls, then run the producer loop on
// this goroutine until Ctrl+C (with --no-produce: no producer, just wait for Ctrl+C).
func station(ctx context.Context, stop context.CancelFunc, c *config) int {
	opt := server.Options{Host: c.host, Port: c.port}
	l, err := server.Listen(opt)
	if err != nil {
		fmt.Fprintf(stderr, "aitv: %v\n", err)
		return 1
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := server.Serve(ctx, l, opt); err != nil && ctx.Err() == nil {
			logx.Printf("[aitv] web server stopped: %v", err)
		}
	})
	if !c.noHLS {
		wg.Go(func() { hlsForever(ctx) })
	}
	if c.rtmpOn {
		if c.rtmpURL == "" {
			logx.Printf("[aitv] --rtmp given but no ingest URL (set AITV_RTMP_URL or --rtmp-url); not pushing")
		} else {
			wg.Go(func() { _ = rtmp.Push(ctx, rtmpOptions(c)) }) // Push relaunches ffmpeg itself until ctx ends
		}
	}
	if c.noProduce {
		logx.Printf("[aitv] production off (--no-produce); airing the library (Ctrl+C to stop)")
		<-ctx.Done()
	} else {
		produceForever(ctx, c)
	}
	// Ctrl+C: let the broadcaster stop its ffmpeg children and the server drain, but not forever; and
	// restore default signal handling so a second Ctrl+C ends the process at once.
	stop()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
	return 0
}

// rtmpFlags registers the RTMP push options shared by the rtmp command and station --rtmp (the URL flag
// differs: --url for the command, --rtmp-url for the station; both default to AITV_RTMP_URL so the key
// can stay out of the process list).
func rtmpFlags(fs *flag.FlagSet, c *config) {
	fs.StringVar(&c.rtmpSource, "rtmp-source", "", "the live playlist to push: an http(s) URL or the HLS directory (default: the station's aitv/hls)")
	fs.BoolVar(&c.rtmpPortrait, "portrait", false, "9:16 output for TikTok: the 16:9 picture over a blurred copy")
	fs.BoolVar(&c.rtmpNoOverlay, "no-overlay", false, "send the bare picture without the station graphics")
	fs.StringVar(&c.rtmpFont, "font", "", "TrueType font file for the graphics (default: a Japanese font from the Windows Fonts folder)")
	fs.IntVar(&c.rtmpFPS, "rtmp-fps", 0, "output frame rate (0 = source in landscape, 30 in portrait)")
	fs.IntVar(&c.rtmpKbps, "rtmp-kbps", 0, "video bitrate in kbit/s (0 = 2500 landscape, 4500 portrait)")
}

func rtmpOptions(c *config) rtmp.Options {
	return rtmp.Options{URL: c.rtmpURL, Source: c.rtmpSource, Portrait: c.rtmpPortrait, NoOverlay: c.rtmpNoOverlay,
		Font: c.rtmpFont, FPS: c.rtmpFPS, VideoKbps: c.rtmpKbps}
}

// hlsForever runs hls.Broadcast until ctx is cancelled, restarting it 10 s after an error or a panic
// (the broadcaster restarts its own encoder; this guards the goroutine itself).
func hlsForever(ctx context.Context) {
	for ctx.Err() == nil {
		err := recovering(func() error { return hls.Broadcast(ctx, hls.Options{SegmentSeconds: hls.DefaultSegmentSeconds}) })
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("broadcaster returned")
		}
		logx.Printf("[aitv] HLS broadcaster crashed (%v); restarting in 10s", err)
		sleep(ctx, hlsRestartDelay)
	}
}

// produceForever is the station's production loop: the producer is re-created 30 s after it dies. When a
// positive --count is reached the station keeps playing until Ctrl+C.
func produceForever(ctx context.Context, c *config) {
	for ctx.Err() == nil {
		err := recovering(func() error {
			return newProducer(ctx, c).Run(ctx, countOf(c), splitList(c.formats))
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logx.Printf("[aitv] producer crashed (%v); restarting in 30s", err)
			sleep(ctx, producerRestartDelay)
			continue
		}
		logx.Printf("[aitv] production finished; station keeps playing (Ctrl+C to stop)")
		<-ctx.Done()
	}
}

// recovering runs fn and turns a panic into an error, with the stack on stderr (station.err.log) so the
// crash can be diagnosed while the supervisor loop restarts the goroutine.
func recovering(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "panic: %v\n%s\n", r, debug.Stack())
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}
