// Package index is the single owner of aitv/library/index.json (port of aitv/index_store.py).
//
// Everything in the station (producer, HLS broadcaster, web server) goes through here so that:
//   - readers never hold the file open while parsing (Windows refuses a rename onto an open handle),
//   - the writer retries the atomic replace instead of losing a clip that took ~400 s of GPU time,
//   - in-process access is serialised by one lock.
//
// The file is a JSON array of Item, written with one-space indentation and without HTML escaping
// (prompts contain <d> tags), UTF-8, like Python's json.dumps(ensure_ascii=False, indent=1).
package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/paths"
)

// Item is one clip in the library. Fields written by the current producer: id, file, format, program,
// programme_id, slot, section, segment_index, segments_total, caption, seconds, seed, render_s, created,
// prompt, sources. Older entries (the one-script-per-clip library) lack programme_id/slot/section/
// segment_index/segments_total/sources.
type Item struct {
	ID      string `json:"id"`
	File    string `json:"file"`
	Format  string `json:"format"`
	Program string `json:"program"`
	// ProgrammeID groups the clips of one programme ("" for the old library).
	ProgrammeID string `json:"programme_id,omitempty"`
	// Slot is the lineup slot the programme was made for; nil for breaks (cm/ident) and old clips
	// (a break's nil is still written as "slot": null, like the Python producer did: see MarshalJSON).
	Slot *int `json:"slot,omitempty"`
	// Section is the section name inside the programme.
	Section string `json:"section,omitempty"`
	// SegmentIndex is the clip's position in the programme (0-based); nil for old clips.
	SegmentIndex *int `json:"segment_index,omitempty"`
	// SegmentsTotal is the programme's clip count (0 = unknown / old clip).
	SegmentsTotal int     `json:"segments_total,omitempty"`
	Caption       string  `json:"caption"`
	Seconds       float64 `json:"seconds"`
	Seed          int64   `json:"seed"`
	RenderS       float64 `json:"render_s"`
	// Created is local time "2006-01-02T15:04:05".
	Created string `json:"created"`
	Prompt  string `json:"prompt"`
	// Sources are the real headlines / forecast lines the programme was written from (at most 8).
	Sources []string `json:"sources,omitempty"`
}

// SegIndex is SegmentIndex or 0.
func (it Item) SegIndex() int {
	if it.SegmentIndex == nil {
		return 0
	}
	return *it.SegmentIndex
}

// itemJSON is the on-disk shape of an Item: the slot key is a raw value so that it can be present as
// null (a break of the current producer) or absent (an old clip), and floats format like Python's repr.
type itemJSON struct {
	ID            string          `json:"id"`
	File          string          `json:"file"`
	Format        string          `json:"format"`
	Program       string          `json:"program"`
	ProgrammeID   string          `json:"programme_id,omitempty"`
	Slot          json.RawMessage `json:"slot,omitempty"`
	Section       string          `json:"section,omitempty"`
	SegmentIndex  *int            `json:"segment_index,omitempty"`
	SegmentsTotal int             `json:"segments_total,omitempty"`
	Caption       string          `json:"caption"`
	Seconds       pyFloat         `json:"seconds"`
	Seed          int64           `json:"seed"`
	RenderS       pyFloat         `json:"render_s"`
	Created       string          `json:"created"`
	Prompt        string          `json:"prompt"`
	Sources       []string        `json:"sources,omitempty"`
}

// MarshalJSON writes the item with the keys the Python producer wrote: a clip of the current producer
// (ProgrammeID set) always carries the "slot" key, null for a break; an old clip (no ProgrammeID) has no
// slot key at all; floats are formatted like Python's repr ("200.0", not "200"). HTML is never escaped
// here; an encoder that escapes it re-escapes the output anyway.
func (it Item) MarshalJSON() ([]byte, error) {
	doc := itemJSON{ID: it.ID, File: it.File, Format: it.Format, Program: it.Program, ProgrammeID: it.ProgrammeID,
		Section: it.Section, SegmentIndex: it.SegmentIndex, SegmentsTotal: it.SegmentsTotal,
		Caption: it.Caption, Seconds: pyFloat(it.Seconds), Seed: it.Seed, RenderS: pyFloat(it.RenderS),
		Created: it.Created, Prompt: it.Prompt, Sources: it.Sources}
	switch {
	case it.Slot != nil:
		doc.Slot = json.RawMessage(strconv.Itoa(*it.Slot))
	case it.ProgrammeID != "":
		doc.Slot = json.RawMessage("null")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// pyFloat marshals like Python's float repr: shortest round-trip digits, "200.0" for integral values,
// exponent form ("1e-05", "1e+16") only when the decimal point falls before 10^-4 or after 10^16.
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, fmt.Errorf("json: unsupported value: %v", v)
	}
	return []byte(formatPyFloat(v)), nil
}

func formatPyFloat(v float64) string {
	e := strconv.FormatFloat(v, 'e', -1, 64) // d.ddde±XX, shortest digits
	i := strings.LastIndexByte(e, 'e')
	exp, _ := strconv.Atoi(e[i+1:])
	decpt := exp + 1 // position of the decimal point relative to the digit string
	if decpt <= -4 || decpt > 16 {
		return e // Python uses the same mantissa and a signed two-digit exponent
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.ContainsRune(s, '.') {
		s += ".0"
	}
	return s
}

var (
	mu sync.Mutex

	// indexPath / libDir are variables so tests can point the store at a temporary directory.
	indexPath = paths.Index
	libDir    = paths.Lib

	saveAttempts = 40
	savePause    = 50 * time.Millisecond

	// warnSkipped logs once per process about entries Parse could not read (see there).
	warnSkipped sync.Once
)

// Load reads and parses the index. The file is read fully (and closed) before parsing so a concurrent
// replace can proceed. Any problem (missing file, torn read during a replace, invalid JSON, not a list)
// yields an empty slice: the caller simply retries on its next pass.
func Load() []Item {
	data, err := os.ReadFile(indexPath())
	if err != nil {
		return []Item{}
	}
	return Parse(data)
}

// Parse decodes an index document the way Load reads the file: an empty slice for anything that is not a
// JSON list. An entry that is not a clip record (a hand-edited "section": 1) must not empty the whole
// index and make the next Append drop every other clip: the entries that do decode are kept, the others
// are skipped (logged once per process) and are gone after the next Save.
func Parse(data []byte) []Item {
	var items []Item
	if err := json.Unmarshal(data, &items); err == nil {
		if items == nil { // "null"
			items = []Item{}
		}
		return items
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil || raws == nil {
		return []Item{} // torn read during a replace, or not a list
	}
	items = make([]Item, 0, len(raws))
	skipped := 0
	for _, r := range raws {
		var it Item
		if err := json.Unmarshal(r, &it); err != nil {
			skipped++
			continue
		}
		items = append(items, it)
	}
	if skipped > 0 {
		warnSkipped.Do(func() { logx.Printf("[aitv] index.json: %d entries are not clip records; skipped", skipped) })
	}
	return items
}

// encode renders the whole index like Python's json.dumps(items, ensure_ascii=False, indent=1): one-space
// indentation, no HTML escaping, no trailing newline.
func encode(items []Item) ([]byte, error) {
	if items == nil {
		items = []Item{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(items); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Save writes the whole index atomically: <index>.tmp then rename onto index.json, retried 40 times at
// 50 ms (a rename fails with a sharing violation while another process holds the file open); after the
// retry budget it falls back to rewriting in place (not atomic, but never drops a clip). The in-process
// lock is held for the whole operation.
func Save(items []Item) error {
	if err := os.MkdirAll(libDir(), 0o755); err != nil {
		return err
	}
	payload, err := encode(items)
	if err != nil {
		return err
	}
	path := indexPath()
	tmp := path + ".tmp"
	mu.Lock()
	defer mu.Unlock()
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	var last error
	for i := 0; i < saveAttempts; i++ {
		err := os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		last = err // sharing violation while a reader holds the file
		time.Sleep(savePause)
	}
	// last resort: in-place rewrite (not atomic, but never drops the clip)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("could not update index.json (%v); in-place rewrite also failed: %v", last, err)
	}
	_ = os.Remove(tmp)
	return nil
}

// Append is Load + append + Save under the in-process lock.
func Append(item Item) error {
	mu.Lock()
	items := Load()
	mu.Unlock()
	return Save(append(items, item))
}

// Playable returns the entries whose mp4 exists in the library directory.
func Playable() []Item {
	out := []Item{}
	lib := libDir()
	for _, it := range Load() {
		if it.File == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(lib, it.File)); err == nil {
			out = append(out, it)
		}
	}
	return out
}
