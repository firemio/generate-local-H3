# Go port of the station — implementation notes

The station and the generation CLI are Go programs (`cmd/`, `internal/`). They replaced the Python
implementation on 2026-09-17; that code now lives in `archive/python/` and is **reference only** — it is
not run, not imported by anything, and not a target the Go code has to match byte for byte. Where this
file says "as the Python did" it is describing a format or a rule that was kept because clients, files on
disk or the operator's `grep` depend on it, not an obligation to emulate CPython.

## Layout

```
go.mod                       module github.com/firemio/generate-local-H3 (Go 1.26; one dependency: gorilla/websocket)
cmd/aitv/main.go             the station CLI: produce | serve | hls | station | renorm
cmd/h3gen/main.go            the generation CLI: gen | bench | info
internal/paths               repo root + data dirs
internal/logx                line logger (stdout -> output/station.log)
internal/winproc             CREATE_NO_WINDOW helper for child processes
internal/comfy               graph.go (H3 API graph builder) + client.go (ComfyUI HTTP + websocket client)
internal/llm                 Ollama / OpenAI-compatible chat, JSON extraction
internal/programs            station identity, formats, lineup, cast, writer prompts
internal/sources             JMA forecast + NHK RSS material for the writer
internal/index               aitv/library/index.json store
internal/rundown             what airs next (programmes, airable clips, NextBlock)
internal/script              two-pass programme writer, scripts/<id>.json
internal/loudness            loudnorm measure/filter + `aitv renorm`
internal/hls                 ffmpeg HLS broadcaster
internal/server              web server (player, playlist.json, now.json, clips, HLS files)
internal/producer            production loop (write -> render -> normalize -> index)
archive/python/              the previous implementation, kept for reading only
```

There is no `internal/argparse` and no `internal/pyval`: the CLIs use `flag.FlagSet`, numbers go through
`strconv`, JSON through `encoding/json`, HTTP through `net/http`. Both CLIs are plain `package main`;
nothing outside `cmd/` depends on them.

Runtime data does not move: `aitv/library/` (clips, `index.json`, `scripts/*.json`), `aitv/hls/`,
`aitv/web/` (player page, `hls.min.js`), `output/` (ComfyUI output, logs, `runs.jsonl`).

Size after the simplification pass (non-test / test lines, `wc -l`):

```
cmd/aitv          459 /  417      internal/loudness   312 /  563
cmd/h3gen         558 /  519      internal/producer   806 / 1274
internal/comfy   1075 / 2357      internal/programs   465 /  491
internal/hls      424 /  509      internal/rundown    293 /  587
internal/index    269 /  330      internal/script     920 / 1248
internal/llm      323 /  663      internal/server     219 /  336
internal/logx      28 /    0      internal/sources    604 / 1081
internal/paths     72 /    0      internal/winproc     26 /    0
                                  total              6853 / 10375
```

(before the pass: ~11,000 non-test lines, most of the difference being the CPython emulation layers).

## Conventions

- **Same job as the Python file it replaces, written as Go.** Log lines keep the same `[aitv]` /
  `[h3gen]` / `[renorm]` prefixes and wording (they are what the operator greps in `output/station.log`):
  "programme failed (...); retrying in 60s", "HLS broadcaster crashed (...); restarting in 10s",
  "producer crashed (...); restarting in 30s", "production finished; station keeps playing", the h3gen
  banner / `OK` / `FAILED` / node-time list.
- **Flags.** One `flag.NewFlagSet` per subcommand, no abbreviations; `-h` prints the flag list on stdout
  with exit 0, an unknown flag, a bad value or a stray argument is a usage error on stderr with exit 2, a
  failed command is exit 1. Integers are what `strconv.ParseInt(s, 0, ...)` accepts; `aitv --count 0` (or
  negative) means forever, `aitv --seed` is int64, `h3gen --seed` is uint64 (ComfyUI's range). A
  non-integer `AITV_LLM_NUM_GPU` / `COMFY_PORT` counts as unset.
- **JSON output** is `encoding/json` with `SetEscapeHTML(false)` (prompts contain `<d>` tags) and, for
  files and `--dry-run`, `SetIndent("", " ")`. Struct field order is the key order; the ComfyUI graph
  (`comfy.Graph` / `comfy.Inputs`) is insertion-ordered on purpose because ComfyUI's planner runs nodes in
  JSON key order and `runs.jsonl` `node_times` follow it. Wire JSON is UTF-8 (no `\uXXXX` escaping).
- **JSON input from the LLM is untyped.** `llm.ChatJSON` returns `map[string]any` (a bare list or scalar
  reply is "not JSON" and retried); the writer coerces leniently (`"seconds": "60"` is 60, `"regular":
  "yes"` is true, a name that is a number is `fmt.Sprint`ed) and treats what it cannot read as missing —
  it never fails a programme on a value's type.
- **Files on disk are typed.** `index.json` decodes into `[]index.Item`; an entry that does not decode is
  skipped (logged once per process) and gone after the next save. `scripts/*.json` decodes into
  `script.Programme`; a file that does not decode is skipped by `History`. `Slot` is `*int` because `null`
  and `0` differ. Two small format helpers survive so the files keep the shape the library already has:
  floats that the old writer stored as `6.0` / `200.0` (`index.pyFloat`, `script.pyFloat`), and the
  `theme` / `summary` / `next_time` keys present for a programme and absent for a break
  (`Programme.IsBreak`). Nothing re-saves an old file, so old files are read, never rewritten.
- **Atomic writes.** Write `<file>.tmp` then `os.Rename`. `index.Save` retries the rename 40 × 50 ms (a
  reader holding the file open makes the rename fail with a sharing violation on Windows) and falls back
  to an in-place rewrite rather than lose a clip; `script.Save` renames once (the id is fresh).
- **Unicode.** "Characters" are code points: `utf8.RuneCountInString` / `[]rune` for the dialogue budgets
  and every `[:n]` cut. Regexps use RE2; whitespace classes are spelled out (`[\s\p{Z}\x{85}]`) because the
  LLM writes U+3000 in Japanese text.
- **Rounding.** Segment counts and section seconds round half to even (`math.RoundToEven`, pinned by
  goldens); `render_s` and the h3gen node times round to 0.1 with `math.Round`.
- **Time.** Local time everywhere: `"2006-01-02T15:04:05"` (index `created`, script `written`) and
  `"20060102-150405"` (ids). Parse with `time.ParseInLocation(..., time.Local)`. NHK `pubDate` goes through
  `time.Parse` over the RFC 1123 / 822 family.
- **Randomness.** `*rand.Rand` is passed in (nil = time-seeded, `--seed` seeds it); distributions and
  rules matter, exact sequences do not.
- **Subprocesses.** `ffmpeg`, `ffprobe`, `powershell.exe` come from PATH; `exec.CommandContext` where a ctx
  exists, `context.WithTimeout` for the timeouts, always `Wait`. Children that must not pop a console use
  `winproc.NoWindow`.
- **Context.** Long loops take a `context.Context` and return promptly when it is cancelled. Ctrl+C /
  SIGTERM cancel the station's root context (`signal.NotifyContext`); a render in flight returns at once
  and the prompt is left running in ComfyUI.
- **Errors, not panics**, in library code; `comfy.Run` returns an error for a refused websocket, a
  rejected `/prompt` (the error text carries ComfyUI's own message), a failed download or a cancelled ctx,
  and every other failure in `RunResult.Err`. The station's supervisor loops (`hlsForever`,
  `produceForever`) and the producer's per-slot body recover from panics, print the stack to stderr and
  restart after the usual delay.
- **Windows** is the target platform. Do not set `SO_REUSEADDR` (a second station on the same port must
  fail: "port N is already in use (is another station running on host:port?)", exit 1).
- **No new dependencies** beyond `github.com/gorilla/websocket`. `gofmt` clean, `go vet ./...` clean.

## The web server

`server.Handler` is one `switch` over the routes: `/` and `/index.html` (the player page),
`/playlist.json`, `/now.json`, `/hls.min.js`, `/clips/<name>`, `/hls/<name>`. Files go through `http.ServeContent`, so
Range requests are RFC 7233 (`bytes=-100` is the last 100 bytes, 416 + `Content-Range: bytes */size` when
unsatisfiable), HEAD works everywhere, `Last-Modified` is sent and other methods get 405. Every response
carries `Cache-Control: no-store` and `Access-Control-Allow-Origin: *`. Only the base name of the request
path is used (`.` / `..` / anything with a separator is a 404), so traversal is impossible.

## Tests

Every package has `_test.go` files; `go test ./...` takes about a minute (the hls and producer tests run
real ffmpeg). The approach:

- **Golden values** for pure functions whose output other things depend on: `comfy.SnapFrames`
  (4→107, 5→124, 6→158, 8→192, 9.4→226, 10→243; the 17k+5 frame grid with half-to-even rounding),
  `comfy.Canvas`, `comfy.Build` for t2v / i2v / r2v specs (inline goldens in `graph_test.go`; the h3gen
  `--dry-run` output against `cmd/h3gen/testdata/dryrun_*.json`, the flag parsing against `specs.json`),
  every writer prompt (`programs` + `script/golden_test.go`), `SanitizePrompt` / `SpeechChars` /
  `FitSpeech`, `sources` text blocks from the JMA / NHK fixtures, the loudness pass-1 parser and filter
  string, `/playlist.json` against `server/testdata/playlist_golden.json`. The goldens were captured from
  the previous implementation once; they are values, not a live comparison, so the tests run without
  Python.
- **Fakes, never the real services.** ComfyUI, Ollama, JMA and NHK are `httptest` servers (HTTP and
  websocket); the filesystem is `t.TempDir()` behind package-level seams (`indexPath`, `libDir`,
  `scriptsDir`, `now`, the producer's `comfyAPI` / `llmAPI` interfaces). Nothing in the tests talks to
  port 8188 or 8800 or writes under `aitv/`.
- **Read-only use of the real library.** The index, rundown and script tests read the real
  `aitv/library/index.json` and `scripts/*.json` when present (round trip: load → save to a temp path →
  load → equal) and skip when they are not.
- **Behaviour, not quirks.** Tests pin what a caller can observe (exit codes, log lines, file shapes,
  headers, the retry / restart rules, the stall and timeout clocks); nothing pins CPython error messages,
  `int()` / `float()` parsing rules, dict repr or `json.dumps` byte layouts any more.
- `AITV_OFFLINE=1` makes the sources return nothing without a network call (tested); `ffmpeg` missing
  skips the hls / loudness tests that need it.

## Deployment

- `scripts/build.ps1` → `go build -o bin\ ./cmd/...` (`bin\aitv.exe`, `bin\h3gen.exe`; `-Test` runs
  `go vet` and `go test ./...` first). Go 1.26+ on PATH is the only requirement; the one dependency is
  fetched into the module cache on the first build.
- `scripts/run_station.ps1` launches `bin\aitv.exe station ...` with stdout → `output/station.log` and
  stderr → `output/station.err.log`. `bin\aitv.exe` is the running station: build into another directory
  (`go build -o bin_new/ ./cmd/...`) and swap after the station is stopped, never overwrite the binary
  while it runs.
- `aitv station` supervises three goroutines in one process: the web server, the HLS broadcaster
  (restarted 10 s after a crash) and the producer (re-created 30 s after a crash; when ComfyUI stops
  answering it is relaunched through `scripts/run_comfyui.ps1`, detached, no console window). `--no-produce`
  runs only the first two: the library airs as it is and nothing talks to ComfyUI or the LLM.
- `archive/python/` is reference only. `scripts/check_gpu.py` is still Python because it probes the
  ComfyUI venv's torch install.
