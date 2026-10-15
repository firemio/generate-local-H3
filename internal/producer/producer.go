// Package producer is the production loop: lineup slot -> real material -> two-pass script -> H3 renders
// clip by clip -> library (port of aitv/producer.py).
//
// Designed to run unattended for days: every step that can fail (LLM, ComfyUI, ffmpeg, index write) is
// contained, and a wedged or dead ComfyUI is escalated (interrupt -> free -> restart) instead of stalling
// the channel forever. The next programme's script is written on the CPU while the current one renders on
// the GPU.
package producer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/firemio/generate-local-H3/internal/comfy"
	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/llm"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/loudness"
	"github.com/firemio/generate-local-H3/internal/paths"
	"github.com/firemio/generate-local-H3/internal/programs"
	"github.com/firemio/generate-local-H3/internal/script"
	"github.com/firemio/generate-local-H3/internal/sources"
	"github.com/firemio/generate-local-H3/internal/winproc"
	"github.com/firemio/generate-local-H3/internal/yomi"
)

// Library clip size: every clip is normalised to this so the HLS stream-copy never changes size.
const (
	LibWidth  = 832
	LibHeight = 480
)

// Collaborators the loop calls, as variables so the tests can drive the loop without a GPU, a writer LLM,
// ComfyUI or the real library (the Python tests patched the same module attributes).
var (
	loadIndex       = index.Load
	appendIndex     = index.Append
	history         = script.History
	writeProgramme  = script.WriteProgramme
	writeBreak      = script.WriteBreak
	materialFor     = sources.MaterialFor
	audioFilterArgs = loudness.AudioFilterArgs
	libDir          = paths.Lib
	scriptsDir      = paths.Scripts
	rawDir          = paths.RawDir
	now             = time.Now
)

// Waits and back-offs (aitv/producer.py defaults); variables so the tests can shorten them.
var (
	comfyPoll         = 15 * time.Second  // wait_for_comfy poll
	comfyRestartAfter = 600 * time.Second // wait_for_comfy restart_after
	llmPoll           = 30 * time.Second  // wait_for_llm
	restartSettle     = 30 * time.Second  // recover_comfy: after a restart, before waiting for it
	retrySleep        = 60 * time.Second  // between programmes that failed or were skipped
	failureUnit       = 20 * time.Second  // render_programme: min(failureMax, failureUnit*failed)
	failureMax        = 300 * time.Second
	normalizeTimeout  = 900 * time.Second
)

const (
	timeLayout = "2006-01-02T15:04:05"
	idLayout   = "20060102-150405"
)

// comfyAPI is the part of *comfy.Client the producer uses (a fake stands in for it in the tests).
type comfyAPI interface {
	Alive() bool
	Run(ctx context.Context, graph comfy.Graph, opt comfy.RunOptions) (comfy.RunResult, error)
	Interrupt()
	Free(unloadModels, freeMemory bool)
}

// llmAPI is the part of *llm.Client the producer itself uses (the script writer takes the real client).
type llmAPI interface {
	Alive() bool
}

// Normalize re-encodes src to dst as a uniform h264/aac mp4 (library size, 24 fps, 48 kHz stereo, one
// loudness level): ffmpeg -y -hide_banner -loglevel error -i src -vf scale=W:H:flags=lanczos,format=yuv420p
// -r 24 -c:v libx264 -preset veryfast -crf 20 -profile:v high -level 4.1 -g 48 -keyint_min 48 -sc_threshold 0
// <loudness.AudioFilterArgs(src)...> -c:a aac -b:a 160k -ar 48000 -ac 2 -movflags +faststart dst
// (900 s timeout).
func Normalize(src, dst string, width, height int) error {
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-i", src,
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos,format=yuv420p", width, height), "-r", "24",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-profile:v", "high", "-level", "4.1",
		"-g", "48", "-keyint_min", "48", "-sc_threshold", "0"}
	args = append(args, audioFilterArgs(src)...)
	args = append(args, "-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2",
		"-movflags", "+faststart", dst)
	ctx, cancel := context.WithTimeout(context.Background(), normalizeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	winproc.NoWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("ffmpeg timed out after %s", normalizeTimeout)
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return fmt.Errorf("ffmpeg: %w: %s", err, lastLine(msg))
	}
	return fmt.Errorf("ffmpeg: %w", err)
}

func lastLine(s string) string {
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// SweepRaw deletes ComfyUI's raw renders in paths.RawDir() older than olderThan (they are exact duplicates
// of library clips: ~1.4 MB each, ~200 a day); recent files are kept in case a run is still in flight.
// Logs "[aitv] swept N raw render(s) from output/aitv" when anything was removed. Returns the count.
func SweepRaw(olderThan time.Duration, log logx.Logger) int {
	if log == nil {
		log = logx.Printf
	}
	dir := rawDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	t, n := now(), 0
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		info, err := os.Stat(p) // follows links, like os.path.isfile
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if t.Sub(info.ModTime()) > olderThan {
			if os.Remove(p) == nil {
				n++
			}
		}
	}
	if n > 0 {
		log("[aitv] swept %d raw render(s) from output/aitv", n)
	}
	return n
}

// Options configure a Producer.
type Options struct {
	// Width, Height, Steps reach the GenSpec as given (cmd/aitv's flag defaults are 640 / 352 / 8).
	Width, Height, Steps int
	Turbo                bool
	// Seed seeds the RNG (nil = random).
	Seed *int64
	// Lineup overrides programs.Lineup (nil = default).
	Lineup []string
	// Minutes overrides every format's length (0 = each format's own).
	Minutes float64
	// Log: nil = logx.Printf.
	Log logx.Logger
	// RestartComfy relaunches ComfyUI (nil = never).
	RestartComfy func() error
}

// Producer owns one production loop.
type Producer struct {
	comfy *comfy.Client
	llm   *llm.Client
	opt   Options
	rng   *rand.Rand
	log   logx.Logger
	// gpu / writer are comfy / llm behind the interfaces the loop calls (tests substitute fakes);
	// comfyBase / llmBase are their URLs for the log lines.
	gpu       comfyAPI
	writer    llmAPI
	comfyBase string
	llmBase   string
	// lineup is the broadcast day; Run(formats) replaces it for one run.
	lineup []string
	// slot is the next lineup slot to produce; resumed from the index at start (ResumeSlot).
	slot        int
	breakToggle int
	// prefetch: the script of the next slot being written on the CPU while the current programme renders.
	prefetch *prefetch
	// yomi converts the Japanese dialogue of a prompt to kana right before the render, so H3 does not
	// have to guess kanji readings (nil = send the writer's text as is: AITV_NO_YOMI=1, or the
	// dictionary failed to load).
	yomi *yomi.Reader
}

type prefetch struct {
	slot int
	done chan struct{}
	prog *script.Programme
}

// New creates a producer; the starting slot is ResumeSlot().
func New(c *comfy.Client, l *llm.Client, opt Options) *Producer {
	p := &Producer{comfy: c, llm: l, opt: opt, log: opt.Log}
	if p.log == nil {
		p.log = logx.Printf
	}
	if os.Getenv("AITV_NO_YOMI") == "" {
		r, err := yomi.New()
		if err != nil {
			p.log("[aitv] reading dictionary failed to load (%v); dialogue goes to H3 as written", err)
		}
		p.yomi = r
	}
	if c != nil {
		p.gpu, p.comfyBase = c, c.Base
	}
	if l != nil {
		p.writer, p.llmBase = l, l.BaseURL
	}
	seed := time.Now().UnixNano()
	if opt.Seed != nil {
		seed = *opt.Seed
	}
	p.rng = newRand(seed)
	if len(opt.Lineup) > 0 {
		p.lineup = append([]string(nil), opt.Lineup...)
	} else {
		p.lineup = append([]string(nil), programs.Lineup...)
	}
	p.slot = p.ResumeSlot()
	return p
}

// lockedSource makes one *rand.Rand shareable between the render loop (seeds) and the prefetch goroutine
// (material sampling, cast names): Python's random.Random was protected by the GIL, rand.Rand is not.
type lockedSource struct {
	mu  sync.Mutex
	src rand.Source
}

func (s *lockedSource) Int63() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src.Int63()
}

func (s *lockedSource) Seed(seed int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.src.Seed(seed)
}

// newRand is a goroutine-safe *rand.Rand seeded with seed.
func newRand(seed int64) *rand.Rand {
	return rand.New(&lockedSource{src: rand.NewSource(seed)})
}

// ---- lineup ------------------------------------------------------------

// posMod is Python's % for a positive divisor (never negative).
func posMod(a, n int) int { return ((a % n) + n) % n }

// ResumeSlot continues after the last programme that reached the library (aitv/producer.py _resume_slot):
// walking the index from the end, the first item with a slot whose format is lineup[slot % len(lineup)]
// gives (slot + 1) % len(lineup); 0 when none.
func (p *Producer) ResumeSlot() int {
	n := len(p.lineup)
	if n == 0 {
		return 0
	}
	items := loadIndex()
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Slot != nil && it.Format == p.lineup[posMod(*it.Slot, n)] {
			return posMod(*it.Slot+1, n)
		}
	}
	return 0
}

func (p *Producer) minutes(fmtKey string) float64 {
	if p.opt.Minutes != 0 {
		return p.opt.Minutes
	}
	return programs.Formats[fmtKey].Minutes
}

// formatOf is programs.Formats[fmtKey]: a lineup entry that is not a format (a misconfigured --lineup /
// --formats) is an error — "writing slot N failed" in the prefetch, "programme failed ... retrying in 60s"
// in the loop body (the slot advanced and not counted).
func formatOf(fmtKey string) (programs.Format, error) {
	f, ok := programs.Formats[fmtKey]
	if !ok {
		return programs.Format{}, fmt.Errorf("unknown format %q", fmtKey)
	}
	return f, nil
}

// Unfinished finds the newest programme in the index that still has clips to render (a restart or crash cut
// it short; aitv/producer.py _unfinished): walking the index from the end, the first item with a
// programme_id decides — if its segments_total is 0 or all its indices are done, nil; else the script is
// loaded (nil when it cannot be read) and Done is filled with the rendered indices.
func (p *Producer) Unfinished() *script.Programme {
	items := loadIndex()
	done := map[string]map[int]bool{}
	for _, it := range items {
		if it.ProgrammeID == "" {
			continue
		}
		if done[it.ProgrammeID] == nil {
			done[it.ProgrammeID] = map[int]bool{}
		}
		done[it.ProgrammeID][it.SegIndex()] = true
	}
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.ProgrammeID == "" {
			continue
		}
		if it.SegmentsTotal == 0 || len(done[it.ProgrammeID]) >= it.SegmentsTotal {
			return nil
		}
		prog, err := loadScript(it.ProgrammeID)
		if err != nil {
			return nil // unreadable or not a script: nothing to resume
		}
		prog.Done = done[it.ProgrammeID]
		return prog
	}
	return nil
}

// loadScript reads scripts/<id>.json.
func loadScript(id string) (*script.Programme, error) {
	data, err := os.ReadFile(filepath.Join(scriptsDir(), id+".json"))
	if err != nil {
		return nil, err
	}
	return script.Decode(data)
}

// ---- script ------------------------------------------------------------

// write is material + script for one slot; (nil, nil) when the slot cannot be made right now.
func (p *Producer) write(slot int) (*script.Programme, error) {
	fmtKey := p.lineup[slot]
	format, err := formatOf(fmtKey)
	if err != nil {
		return nil, err
	}
	if gapH := format.MinGapHours; gapH > 0 {
		last, err := history(fmtKey, 1)
		if err != nil {
			return nil, err
		}
		if len(last) > 0 {
			written, err := time.ParseInLocation(timeLayout, last[0].Written, time.Local)
			if err != nil {
				return nil, fmt.Errorf("bad written time %q: %w", last[0].Written, err)
			}
			ageH := now().Sub(written).Hours()
			if ageH < gapH {
				p.log("[aitv] last %s was written %.1f h ago (< %s h): the material would be the same; slot %d skipped this round",
					fmtKey, ageH, fmtG(gapH), slot+1)
				return nil, nil
			}
		}
	}
	material := materialFor(fmtKey, p.rng)
	if material == nil && format.NeedsMaterial {
		p.log("[aitv] no real material for %s (offline?); slot %d skipped this round", fmtKey, slot+1)
		return nil, nil
	}
	s := slot
	return writeProgramme(p.llm, fmtKey, material, p.minutes(fmtKey), &s, p.log, p.rng)
}

// startPrefetch writes slot's script in the background (the Python daemon thread + box).
func (p *Producer) startPrefetch(slot int) {
	pf := &prefetch{slot: slot, done: make(chan struct{})}
	p.prefetch = pf
	go func() {
		defer close(pf.done)
		prog, err := p.guarded(func() (*script.Programme, error) { return p.write(slot) })
		if err != nil {
			p.log("[aitv] writing slot %d failed (%s)", slot+1, errText(err))
			return
		}
		pf.prog = prog
	}()
}

// scriptFor takes the prefetched script for slot, or writes it now.
func (p *Producer) scriptFor(slot int) (*script.Programme, error) {
	if pf := p.prefetch; pf != nil && pf.slot == slot {
		p.prefetch = nil
		<-pf.done
		return pf.prog, nil
	}
	p.prefetch = nil // a prefetch for another slot is dropped (its goroutine finishes on its own)
	return p.write(slot)
}

// guarded runs fn, turning a panic (a TODO body, a nil map, an index out of range...) into an error, the
// way the Python loop caught Exception.
func (p *Producer) guarded(fn func() (*script.Programme, error)) (prog *script.Programme, err error) {
	defer func() {
		if r := recover(); r != nil {
			prog, err = nil, panicError(r)
		}
	}()
	return fn()
}

// panicError wraps a recovered panic value.
func panicError(r any) error {
	if e, ok := r.(error); ok {
		return fmt.Errorf("panic: %w", e)
	}
	return fmt.Errorf("panic: %v", r)
}

// errText is the message of err for the log lines ("" for nil).
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// fmtG formats a float the short way for the log lines ("6", "0.1", "5").
func fmtG(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }

// ---- render ------------------------------------------------------------

// RenderSegment renders one clip and puts it in the library (aitv/producer.py render_segment): GenSpec
// (t2v, Width x Height, seg seconds, random seed in [1, 2^31), Steps, Turbo, prefix "aitv/seg"), comfy.Run
// with a silent log and a 1200 s stall timeout; on a failed render log "[aitv] render failed: <err>" ("None"
// when the run was clean but produced no file) and return (nil, nil). An error from comfy.Run (a refused
// websocket, a rejected graph, a cancelled ctx) is returned as is, and so is a library directory that
// cannot be created: those abandon the whole programme rather than counting as one failed clip.
// The raw file is normalised into <lib>/<format>-<uid>.mp4 where uid = "20060102-150405-" + last 6 of the
// programme id + "-NN" (falls back to a plain copy when ffmpeg fails, logging it); the raw outputs are then
// deleted. Returns the index item (seconds = spec.Length()/24, render_s rounded to 0.1, created now,
// sources = the programme's first 8) and logs "[aitv] + <file>  (Xs, rendered in Ns) [<title> / <section> i/N: <caption>]".
func (p *Producer) RenderSegment(ctx context.Context, prog *script.Programme, idx int, section string, seg script.Segment) (*index.Item, error) {
	seed := 1 + p.rng.Int63n(1<<31-1) // randrange(1, 2**31)
	prompt := p.speech(prog, seg.Prompt)
	spec := comfy.NewGenSpec(prompt)
	spec.Mode = "t2v"
	spec.Width, spec.Height = p.opt.Width, p.opt.Height
	spec.Seconds = float64(seg.Seconds)
	spec.Seed = uint64(seed)
	spec.Steps = p.opt.Steps
	spec.Turbo = p.opt.Turbo
	spec.FilenamePrefix = "aitv/seg"
	res, err := p.gpu.Run(ctx, comfy.Build(spec), comfy.RunOptions{Log: logx.Discard, StallTimeout: 1200 * time.Second})
	if err != nil {
		return nil, err
	}
	if !res.OK || len(res.LocalFiles) == 0 {
		p.log("[aitv] render failed: %s", noneIfEmpty(res.Err))
		return nil, nil
	}
	uid := now().Format(idLayout) + "-" + lastRunes(prog.ID, 6) + fmt.Sprintf("-%02d", idx)
	lib := libDir()
	dst := filepath.Join(lib, fmt.Sprintf("%s-%s.mp4", prog.Format, uid))
	if err := os.MkdirAll(lib, 0o755); err != nil {
		return nil, err // the programme fails, no escalation
	}
	raw := res.LocalFiles[0]
	if err := Normalize(raw, dst, LibWidth, LibHeight); err != nil {
		p.log("[aitv] ffmpeg normalize failed (%s); copying raw", errText(err))
		if err2 := copyFile(raw, dst); err2 != nil {
			p.log("[aitv] could not keep the clip: %s", errText(err2))
			return nil, nil
		}
	}
	for _, f := range res.LocalFiles { // the library copy is authoritative; drop the duplicate
		_ = os.Remove(f)
	}
	segIdx := idx
	item := &index.Item{
		ID:            uid,
		File:          filepath.Base(dst),
		Format:        prog.Format,
		Program:       prog.Title,
		ProgrammeID:   prog.ID,
		Slot:          prog.Slot,
		Section:       section,
		SegmentIndex:  &segIdx,
		SegmentsTotal: prog.SegmentsTotal,
		Caption:       seg.Caption,
		Seconds:       comfy.FramesSeconds(spec.Length()),
		Seed:          seed,
		RenderS:       math.Round(res.Seconds*10) / 10,
		Created:       now().Format(timeLayout), // stamped after the normalize, as the Python did
		Prompt:        prompt,
	}
	if len(prog.Sources) > 0 {
		item.Sources = append([]string(nil), prog.Sources[:min(8, len(prog.Sources))]...)
	}
	p.log("[aitv] + %s  (%.1fs, rendered in %.0fs) [%s / %s %d/%d: %s]",
		item.File, item.Seconds, res.Seconds, prog.Title, section, idx+1, prog.SegmentsTotal, seg.Caption)
	return item, nil
}

// noneIfEmpty is the text of RunResult.Err, or "None" when there was none (the Python printed None).
func noneIfEmpty(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// lastRunes is s[-n:] in Python terms.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// copyFile is shutil.copy: contents only.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// RenderProgramme renders every segment not in prog.Done: WaitForComfy before each; a successful clip is
// appended to the index (a failed index write is logged, the clip stays on disk); a failed render calls
// RecoverComfy(failed) and sleeps min(300, 20*failed) s; six failures abandon the programme with a log line.
// Returns (made, failed, nil), or the RenderSegment error (the Python render_programme had no try around
// render_segment: the exception left the programme where it was, the rest of its clips unrendered).
func (p *Producer) RenderProgramme(ctx context.Context, prog *script.Programme) (made, failed int, err error) {
	for _, s := range prog.Segments() {
		if prog.Done[s.Index] {
			continue
		}
		if ctx.Err() != nil {
			return made, failed, nil
		}
		p.WaitForComfy(ctx)
		if ctx.Err() != nil {
			return made, failed, nil
		}
		item, err := p.RenderSegment(ctx, prog, s.Index, s.Section, s.Segment)
		if err != nil {
			return made, failed, err
		}
		if item != nil {
			made++
			if err := appendIndexSafe(*item); err != nil { // a failed index write must not lose the other segments
				p.log("[aitv] index write failed (%s); clip kept on disk as %s", errText(err), item.File)
			}
		} else {
			failed++
			p.RecoverComfy(ctx, failed)
			if !sleep(ctx, min(failureMax, failureUnit*time.Duration(failed))) {
				return made, failed, nil
			}
			if failed >= 6 {
				p.log("[aitv] giving up on %s after %d failed renders", prog.Title, failed)
				break
			}
		}
	}
	return made, failed, nil
}

// appendIndexSafe is appendIndex with panics turned into errors.
func appendIndexSafe(item index.Item) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return appendIndex(item)
}

// ---- health ------------------------------------------------------------

// sleep waits d or until ctx is cancelled; false when cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// callHook runs an optional hook, turning a panic into an error.
func callHook(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	return fn()
}

// WaitForComfy blocks until the ComfyUI API answers, polling every 15 s; if it stays down for 600 s and
// RestartComfy is set, it is relaunched once (log lines as in Python: "not reachable; waiting...",
// "still down; relaunching it", "ComfyUI is back").
func (p *Producer) WaitForComfy(ctx context.Context) {
	if p.gpu.Alive() {
		return
	}
	p.log("[aitv] ComfyUI at %s not reachable; waiting...", p.comfyBase)
	t0 := now()
	restarted := false
	for !p.gpu.Alive() {
		if !restarted && p.opt.RestartComfy != nil && now().Sub(t0) > comfyRestartAfter {
			p.log("[aitv] ComfyUI still down; relaunching it")
			if err := callHook(p.opt.RestartComfy); err != nil {
				p.log("[aitv] relaunch failed: %s", errText(err))
			}
			restarted = true
			t0 = now()
		}
		if !sleep(ctx, comfyPoll) {
			return
		}
	}
	p.log("[aitv] ComfyUI is back")
}

// RecoverComfy escalates after repeated render failures: interrupt + free (models unloaded) every time;
// from the 4th failure also RestartComfy (when set), sleep 30 s and WaitForComfy. /system_stats keeps
// answering after a HIP hang or a driver reset, so "alive" is not enough evidence that the GPU works.
func (p *Producer) RecoverComfy(ctx context.Context, failures int) {
	p.log("[aitv] %d consecutive failure(s): interrupting and freeing models", failures)
	if err := callHook(func() error { p.gpu.Interrupt(); p.gpu.Free(true, true); return nil }); err != nil {
		p.log("[aitv] recovery call failed: %s", errText(err))
	}
	if failures >= 4 && p.opt.RestartComfy != nil {
		p.log("[aitv] restarting ComfyUI")
		if err := callHook(p.opt.RestartComfy); err != nil {
			p.log("[aitv] restart failed: %s", errText(err))
		}
		if !sleep(ctx, restartSettle) {
			return
		}
		p.WaitForComfy(ctx)
	}
}

// WaitForLLM blocks (30 s polls) until the writer LLM answers, logging once when it had to wait.
func (p *Producer) WaitForLLM(ctx context.Context) {
	waited := false
	for !p.writer.Alive() {
		if !waited {
			p.log("[aitv] writer LLM at %s not answering; waiting...", p.llmBase)
			waited = true
		}
		if !sleep(ctx, llmPoll) {
			return
		}
	}
	if waited {
		p.log("[aitv] writer LLM is back")
	}
}

// ---- loop --------------------------------------------------------------

// ProduceBreak writes and renders one fresh commercial or ident (alternating), after each programme. A
// failed script is logged and skipped; a RenderProgramme error is returned (in the Python it propagated to
// the programme-level handler of run()).
func (p *Producer) ProduceBreak(ctx context.Context) error {
	fmtKey := programs.BreakFormats[posMod(p.breakToggle, len(programs.BreakFormats))]
	p.breakToggle++
	prog, err := p.guarded(func() (*script.Programme, error) { return writeBreak(p.llm, fmtKey) })
	if err != nil {
		p.log("[aitv] %s script failed (%s); skipped", fmtKey, errText(err))
		return nil
	}
	_, _, err = p.RenderProgramme(ctx, prog)
	return err
}

// Run is the production loop (aitv/producer.py Producer.run). count nil = forever (Python's None); any other
// value is the number of slots to attempt, so 0 or a negative count attempts none (the resume still runs).
// formats non-nil replaces the lineup for this run (and skips the resume). First an Unfinished programme
// is resumed ("[aitv] === resuming: <title> (N clips left)"). Then per slot: WaitForLLM, WaitForComfy, log
// "[aitv] === slot i/n: <title> (<minutes> min)", take the prefetched script for this slot (or write it
// now: a news slot whose last episode is younger than MinGapHours is skipped this round, as is a material
// format without material — both logged; the loop then sleeps 60 s), advance the slot, start the prefetch
// of the next slot, render, log "[aitv] === done: ...", SweepRaw(1 h) and ProduceBreak when anything was
// made. Any error/panic in a programme is logged ("[aitv] programme failed (...); retrying in 60s"), the
// slot advances, sleep 60 s. Returns nil when ctx is cancelled or count programmes were attempted (the
// error return is the shape the station's supervisor runs; nothing else ends the loop).
func (p *Producer) Run(ctx context.Context, count *int, formats []string) error {
	if len(formats) > 0 {
		p.lineup = append([]string(nil), formats...)
		p.slot = 0
	}
	n := 0
	var unfinished *script.Programme
	if len(formats) == 0 {
		unfinished = p.Unfinished()
	}
	if unfinished != nil {
		if err := p.resume(ctx, unfinished); err != nil {
			p.log("[aitv] resume failed (%s); moving on", errText(err))
		}
	}
	for count == nil || n < *count {
		if ctx.Err() != nil {
			return nil
		}
		slot := p.slot
		if err := p.produceSlot(ctx, slot, count, &n); err != nil { // LLM/HTTP/ffmpeg/... keep the station alive
			if ctx.Err() != nil {
				return nil
			}
			p.log("[aitv] programme failed (%s); retrying in 60s", errText(err))
			p.slot = posMod(slot+1, len(p.lineup))
			if !sleep(ctx, retrySleep) {
				return nil
			}
		}
	}
	return nil
}

// resume renders what is left of an unfinished programme (the Python try block around it).
func (p *Producer) resume(ctx context.Context, prog *script.Programme) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	p.WaitForComfy(ctx)
	if ctx.Err() != nil {
		return nil
	}
	left := prog.SegmentsTotal - len(prog.Done)
	p.log("[aitv] === resuming: %s (%d clips left)", prog.Title, left)
	made, failed, err := p.RenderProgramme(ctx, prog)
	if err != nil {
		return err
	}
	p.log("[aitv] === done: %s (%d clips, %d failed)", prog.Title, made, failed)
	if made > 0 {
		SweepRaw(time.Hour, p.log)
	}
	return nil
}

// produceSlot is one iteration of the Python loop body (the try block); a panic becomes an error.
func (p *Producer) produceSlot(ctx context.Context, slot int, count *int, n *int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = panicError(r)
		}
	}()
	fmtKey := p.lineup[slot]
	p.WaitForLLM(ctx)
	p.WaitForComfy(ctx)
	if ctx.Err() != nil {
		return nil
	}
	format, err := formatOf(fmtKey)
	if err != nil {
		return err
	}
	p.log("[aitv] === slot %d/%d: %s (%s min)", slot+1, len(p.lineup), format.Title, fmtG(p.minutes(fmtKey)))
	prog, err := p.scriptFor(slot)
	if err != nil {
		return err
	}
	p.slot = posMod(slot+1, len(p.lineup))
	*n++
	if prog == nil {
		sleep(ctx, retrySleep)
		return nil
	}
	if count == nil || *n < *count {
		p.startPrefetch(p.slot) // write the next one while this one renders
	}
	made, failed, err := p.RenderProgramme(ctx, prog)
	if err != nil {
		return err
	}
	p.log("[aitv] === done: %s (%d clips, %d failed)", prog.Title, made, failed)
	if made > 0 && ctx.Err() == nil {
		SweepRaw(time.Hour, p.log)
		return p.ProduceBreak(ctx)
	}
	return nil
}

// speech is the prompt H3 gets: the writer's text with every <d>[Japanese] ... </d> body converted to kana
// (the script on disk keeps the kanji), the cast's names read the way the writer wrote them in Kana.
func (p *Producer) speech(prog *script.Programme, prompt string) string {
	if p.yomi == nil {
		return prompt
	}
	names := make([]string, len(prog.Cast))
	kanas := make([]string, len(prog.Cast))
	for i, c := range prog.Cast {
		names[i], kanas[i] = c.Name, c.Kana
	}
	return p.yomi.Dialogue(prompt, yomi.CastNames(names, kanas))
}
