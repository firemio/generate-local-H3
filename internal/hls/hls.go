// Package hls is the broadcaster: it turns the library into one continuous live HLS stream with ffmpeg
// (port of aitv/hls.py).
//
// A single long-running ffmpeg reads MPEG-TS from stdin (-re = real-time pacing) and writes a sliding HLS
// window to <dir>/live.m3u8. The broadcaster feeds it clip after clip (remuxed to TS, stream copy) in the
// order the Rundown decides, block by block, and picks up new clips as the producer adds them. Any HLS
// player (the web page via hls.js, VLC, OBS Media Source) can tune in; <dir>/now.json tells the web page
// what is on air (keyed by wall-clock time, matched to the stream's program-date-time on the player side).
package hls

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/paths"
	"github.com/firemio/generate-local-H3/internal/rundown"
	"github.com/firemio/generate-local-H3/internal/winproc"
)

// OnAir is one entry of now.json (the last 40 clips put on air).
type OnAir struct {
	T       float64 `json:"t"` // unix seconds, fractional
	Program string  `json:"program"`
	Caption string  `json:"caption"`
	Section string  `json:"section"`
	Format  string  `json:"format"`
	Seconds float64 `json:"seconds"`
	Created string  `json:"created"`
	ID      string  `json:"id"`
}

// DefaultSegmentSeconds is the station's -hls_time (the Python broadcast() default).
const DefaultSegmentSeconds = 2

// Options configure Broadcast.
type Options struct {
	// SegmentSeconds is -hls_time, passed to ffmpeg verbatim — `aitv hls --segment N` hands over N unchecked
	// as the Python did, 0 (a segment per keyframe) and negative values (ffmpeg refuses to start; the
	// broadcaster keeps restarting it) included. The station uses DefaultSegmentSeconds.
	SegmentSeconds int
	// Window is -hls_list_size (0 = 10).
	Window int
	// Dir is the HLS output directory ("" = paths.HLS()). It is emptied at start.
	Dir string
	// Lib is the clip directory ("" = paths.Lib()).
	Lib string
	// Lineup overrides the rundown's lineup (nil = programs.Lineup).
	Lineup []string
	// Log: nil = logx.Printf.
	Log logx.Logger

	// nextBlock replaces "index.Playable() -> rundown.NextBlock" (tests feed a fixed clip list; nil = the
	// real rundown). It returns the next ordered block, empty when nothing is airable.
	nextBlock func() []index.Item
	// onSpawn is told about every ffmpeg started (tests check that each one was waited for).
	onSpawn func(*child)
}

const (
	onAirKeep  = 40               // now.json keeps the last 40 clips
	chunkSize  = 1 << 16          // remux -> encoder copy chunk
	stopWait   = 5 * time.Second  // close stdin / kill grace for the encoder
	remuxWait  = 10 * time.Second // grace for the remux to exit once its output was consumed
	emptySleep = 5 * time.Second  // pause when the library has nothing to air
)

// Broadcast runs until ctx is cancelled (then stops ffmpeg and returns nil) or something unrecoverable
// happens (returns the error; the station restarts it). Behaviour (aitv/hls.py broadcast):
//
//   - encoder: ffmpeg -hide_banner -loglevel error -re -fflags +genpts -f mpegts -i pipe:0 -c copy -f hls
//     -hls_time N -hls_list_size W -hls_flags delete_segments+append_list+omit_endlist+program_date_time
//     -hls_segment_filename <dir>/seg%06d.ts <dir>/live.m3u8. NOTE: no +igndts — the mpegts demuxer already
//     has correct DTS; discarding it makes ffmpeg clamp DTS across B-frame groups and destroys presentation
//     timestamps after the first clip boundary. +genpts alone is enough.
//   - loop: if the encoder exited, log and restart it; when the queue is empty, index.Playable() ->
//     rundown.NextBlock (log "[aitv] library empty, waiting for the producer..." once and sleep 5 s when nothing,
//     else log "[aitv] block: fmt > fmt > ..."); pop the next clip (skip when its file vanished); log
//     "[aitv] >> <program> / <caption>"; append to now.json (atomic tmp+rename, errors ignored); run
//     ffmpeg -hide_banner -loglevel error -i <clip> -c:v copy -bsf:v h264_mp4toannexb -c:a copy -f mpegts pipe:1
//     and copy its stdout into the encoder's stdin in 64 KiB chunks.
//   - a write error on the encoder's stdin means the encoder is gone: kill the remux first (it is blocked
//     writing into a pipe nobody drains, so waiting on it would hang), log "[aitv] encoder pipe broke;
//     restarting encoder", then stop and restart the encoder. The remux is always waited for (10 s, then
//     killed).
//   - stopping the encoder: close its stdin, wait 5 s, kill, wait 5 s.
func Broadcast(ctx context.Context, opt Options) error {
	b := &broadcaster{ctx: ctx, dir: opt.Dir, lib: opt.Lib, segment: opt.SegmentSeconds, window: opt.Window,
		log: opt.Log, onSpawn: opt.onSpawn}
	if b.window <= 0 {
		b.window = 10
	}
	if b.dir == "" {
		b.dir = paths.HLS()
	}
	if b.lib == "" {
		b.lib = paths.Lib()
	}
	if b.log == nil {
		b.log = logx.Printf
	}
	next := opt.nextBlock
	if next == nil {
		rd := rundown.New(nil, opt.Lineup)
		next = func() []index.Item {
			items := index.Playable() // only clips whose file is on disk
			if len(items) == 0 {
				return nil
			}
			return rd.NextBlock(items)
		}
	}

	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(b.dir, e.Name()))
	}
	nowFile := filepath.Join(b.dir, "now.json")

	if err := b.startEncoder(); err != nil {
		return b.fail(err)
	}
	b.log("[aitv] HLS broadcaster started -> aitv/hls/live.m3u8")
	defer func() { b.enc.stop() }()

	var queue []index.Item
	var onAir []OnAir
	warnedEmpty := false
	for {
		if ctx.Err() != nil {
			return nil
		}
		if b.enc.exited() { // encoder died: replace it and keep the channel going
			b.log("[aitv] HLS encoder exited (%d); restarting", b.enc.exitCode())
			b.enc.stop()
			if err := b.startEncoder(); err != nil {
				return b.fail(err)
			}
		}
		if len(queue) == 0 {
			queue = next()
			if len(queue) == 0 {
				if !warnedEmpty {
					b.log("[aitv] library empty, waiting for the producer...")
					warnedEmpty = true
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(emptySleep):
				}
				continue
			}
			warnedEmpty = false
			formats := make([]string, len(queue))
			for i, c := range queue {
				formats[i] = c.Format
			}
			b.log("[aitv] block: %s", strings.Join(formats, " > "))
		}
		it := queue[0]
		queue = queue[1:]
		if it.File == "" {
			continue
		}
		src := filepath.Join(b.lib, it.File)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		b.log("[aitv] >> %s / %s", it.Program, it.Caption)
		onAir = logOnAir(nowFile, onAir, it)
		broken, err := b.feed(src)
		if err != nil {
			return b.fail(err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if broken {
			b.enc.stop()
			if err := b.startEncoder(); err != nil {
				return b.fail(err)
			}
		}
	}
}

// broadcaster is the state of one Broadcast call.
type broadcaster struct {
	ctx     context.Context
	dir     string
	lib     string
	segment int
	window  int
	log     logx.Logger
	onSpawn func(*child)
	enc     *child // the current HLS encoder
}

// fail maps an error hit after ctx was cancelled to the clean nil return.
func (b *broadcaster) fail(err error) error {
	if b.ctx.Err() != nil {
		return nil
	}
	return err
}

// startEncoder launches the HLS encoder (see Broadcast) as b.enc.
func (b *broadcaster) startEncoder() error {
	// NOTE: no +igndts. The mpegts demuxer already has correct DTS; discarding it makes ffmpeg
	// reconstruct timestamps and clamp DTS to prev+1 tick across B-frame groups, which destroys
	// presentation timestamps after the first clip boundary (measured: 71% of frames affected).
	// +genpts alone is enough, and ffmpeg re-bases each clip's timestamps continuously.
	cmd := exec.CommandContext(b.ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-re", "-fflags", "+genpts",
		"-f", "mpegts", "-i", "pipe:0",
		"-c", "copy", "-f", "hls", "-hls_time", strconv.Itoa(b.segment), "-hls_list_size", strconv.Itoa(b.window),
		"-hls_flags", "delete_segments+append_list+omit_endlist+program_date_time",
		"-hls_segment_filename", filepath.Join(b.dir, "seg%06d.ts"), filepath.Join(b.dir, "live.m3u8"))
	cmd.Stderr = os.Stderr
	winproc.NoWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// When ctx is cancelled, os/exec closes the encoder's stdin (it finishes the window and exits) and
	// kills it after WaitDelay: the same close / wait / kill sequence as child.stop, but it also unblocks
	// a Write stuck on a hung encoder.
	cmd.Cancel = stdin.Close
	cmd.WaitDelay = stopWait
	c, err := b.spawn(cmd, stdin)
	if err != nil {
		return err
	}
	b.enc = c
	return nil
}

// feed remuxes one clip to MPEG-TS and pipes it into the encoder. broken reports that the encoder's stdin
// failed (the encoder is gone; the caller restarts it); err is a failure to launch ffmpeg.
//
// The remux's stdout is a pipe of our own (os.Pipe), not cmd.StdoutPipe: the child is reaped by a background
// Wait, and os/exec's Wait closes the read end of a StdoutPipe as soon as the process exits — under a Write
// blocked by the encoder's -re pacing, that dropped the bytes still buffered in the pipe (the last TS
// packets of every clip). Our read end stays open until the loop has read to EOF, as the Python read
// remux.stdout to EOF before wait().
func (b *broadcaster) feed(src string) (broken bool, err error) {
	cmd := exec.CommandContext(b.ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-i", src,
		"-c:v", "copy", "-bsf:v", "h264_mp4toannexb", "-c:a", "copy",
		"-f", "mpegts", "pipe:1")
	cmd.Stderr = os.Stderr
	winproc.NoWindow(cmd)
	out, w, err := os.Pipe()
	if err != nil {
		return false, err
	}
	cmd.Stdout = w
	remux, err := b.spawn(cmd, nil)
	_ = w.Close() // the child holds its own handle; ours must go so EOF follows the child's exit
	if err != nil {
		_ = out.Close()
		return false, err
	}
	buf := make([]byte, chunkSize)
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			if _, werr := b.enc.stdin.Write(buf[:n]); werr != nil {
				// The encoder is gone (or ctx closed its stdin). Kill the remux first: it is blocked
				// writing the rest of the clip into a stdout pipe nobody drains any more, so waiting on
				// it alone would hang forever.
				if b.ctx.Err() == nil {
					broken = true
					b.log("[aitv] encoder pipe broke; restarting encoder")
				}
				break
			}
		}
		if rerr != nil {
			break // io.EOF once the child has exited and every buffered byte was read
		}
	}
	if broken {
		remux.kill()
	}
	_ = out.Close()
	if !remux.wait(remuxWait) {
		remux.kill()
		remux.wait(stopWait)
	}
	return broken, nil
}

// spawn starts cmd as a child (stdin is the encoder's pipe, nil for a remux) and reports it to onSpawn.
func (b *broadcaster) spawn(cmd *exec.Cmd, stdin io.WriteCloser) (*child, error) {
	c, err := startChild(cmd)
	if err != nil {
		return nil, err
	}
	c.stdin = stdin
	if b.onSpawn != nil {
		b.onSpawn(c)
	}
	return c, nil
}

// child is a running ffmpeg with a background Wait, so that its exit can be polled (Python's poll()) and
// waited for with a timeout (wait(timeout=...)). Every child is reaped: the goroutine runs until Wait
// returns, and the process is killed when it overstays its grace.
type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser // the encoder's stdin (nil for the remux)
	done  chan struct{}  // closed once Wait returned (cmd.ProcessState is valid from then on)
}

func startChild(cmd *exec.Cmd) (*child, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait() // the exit status is read from ProcessState; a kill / ctx error is expected
		close(c.done)
	}()
	return c, nil
}

// exited reports whether the process has been reaped (non-blocking).
func (c *child) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// wait blocks until the process has been reaped or d elapses; it reports whether it was reaped.
func (c *child) wait(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (c *child) kill() { _ = c.cmd.Process.Kill() }

// exitCode is the process exit code once it was reaped (-1 otherwise).
func (c *child) exitCode() int {
	if !c.exited() || c.cmd.ProcessState == nil {
		return -1
	}
	return c.cmd.ProcessState.ExitCode()
}

// stop is Python's _stop: close stdin, wait 5 s, kill, wait 5 s. nil is fine.
func (c *child) stop() {
	if c == nil {
		return
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.wait(stopWait) {
		return
	}
	c.kill()
	c.wait(stopWait)
}

// logOnAir appends the clip to the on-air ring (last 40) and rewrites now.json atomically (tmp + rename;
// errors are ignored, like Python's _log_on_air). It returns the trimmed ring.
func logOnAir(nowFile string, onAir []OnAir, it index.Item) []OnAir {
	onAir = append(onAir, OnAir{
		T:       float64(time.Now().UnixMicro()) / 1e6,
		Program: it.Program,
		Caption: it.Caption,
		Section: it.Section,
		Format:  it.Format,
		Seconds: it.Seconds,
		Created: it.Created,
		ID:      it.ID,
	})
	if len(onAir) > onAirKeep {
		onAir = onAir[len(onAir)-onAirKeep:]
	}
	if data, err := encodeOnAir(onAir); err == nil {
		tmp := nowFile + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err == nil {
			_ = os.Rename(tmp, nowFile)
		}
	}
	return onAir
}

// encodeOnAir is json.dumps(on_air, ensure_ascii=False): compact, UTF-8, no HTML escaping.
func encodeOnAir(onAir []OnAir) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(onAir); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
