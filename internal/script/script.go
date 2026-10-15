// Package script is the two-pass programme writer: outline first, then the clips of each section
// (port of aitv/script.py).
//
// The whole script is kept as aitv/library/scripts/<programme id>.json so a programme can be read as a
// document (title, cast, sections, every clip's caption and prompt) and so the producer can resume it.
// The documents are value-compatible with the ones the Python writer produced: the producer resumes from
// them and History reads them.
//
// "Characters" are code points (utf8.RuneCountInString), as Python's len() counted them, so the dialogue
// budgets match. The regexps spell whitespace as ws because RE2's \s is ASCII and the LLM writes full-width
// spaces (U+3000) in Japanese text.
package script

import (
	"bytes"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/llm"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/paths"
	"github.com/firemio/generate-local-H3/internal/programs"
	"github.com/firemio/generate-local-H3/internal/sources"
)

// LangTag is the dialogue language tag every <d> must carry.
const LangTag = "[Japanese]"

// CharsPerS: H3 speaks Japanese at roughly this rate; more and it rushes or cuts the line.
const CharsPerS = 5.5

// PlaceholderNames are cast names the LLM uses when it has none; they are replaced from CastNames.
var PlaceholderNames = map[string]bool{"不明": true, "未定": true, "N/A": true, "n/a": true, "なし": true, "TBD": true, "未設定": true, "名無し": true,
	"Anchor": true, "Host": true, "Presenter": true, "Narrator": true, "Chef": true, "Reporter": true}

// CastNames is the pool of Japanese names for unnamed cast (verbatim from aitv/script.py CAST_NAMES).
var CastNames = []string{"佐藤 美咲", "高橋 悠真", "中村 千夏", "小林 蓮", "加藤 陽菜", "吉田 大和", "山本 結衣", "松本 拓海", "井上 澪",
	"木村 颯太", "林 さくら", "清水 隼人", "森 ひなた", "池田 航", "橋本 葵", "石川 湊"}

const timeLayout = "2006-01-02T15:04:05"

// Seams for the tests: where the scripts live, how the index is read, and the wall clock.
var (
	scriptsDir = paths.Scripts
	loadIndex  = func() []index.Item { return index.Load() }
	now        = time.Now
)

// Segment is one clip.
type Segment struct {
	Caption string `json:"caption"`
	Seconds int    `json:"seconds"`
	Prompt  string `json:"prompt"`
}

// Section is one corner of a programme.
type Section struct {
	Name string `json:"name"`
	// Plan is the outline's plan for the section; nil (saved as null) when the LLM gave none.
	Plan     *string   `json:"plan"`
	Seconds  int       `json:"seconds"`
	Segments []Segment `json:"segments"`
}

// Programme is a written programme (the scripts/<id>.json document).
type Programme struct {
	ID     string `json:"id"`
	Format string `json:"format"`
	Slot   *int   `json:"slot"`
	Title  string `json:"title"`
	// Theme, Summary and NextTime are written for a programme (empty ones included) and left out of a
	// break document (see IsBreak), as the Python writers did.
	Theme    string          `json:"theme"`
	Summary  string          `json:"summary"`
	NextTime string          `json:"next_time"`
	Cast     []programs.Cast `json:"cast"`
	// Minutes is written as Python's float repr ("6.0", "0.1"), see pyFloat.
	Minutes float64  `json:"minutes"`
	Sources []string `json:"sources"`
	// Written is local time "2006-01-02T15:04:05".
	Written       string    `json:"written"`
	Sections      []Section `json:"sections"`
	SegmentsTotal int       `json:"segments_total"`
	SecondsTotal  int       `json:"seconds_total"`
	// Done holds the segment indices already rendered when the producer resumes a programme (not persisted).
	Done map[int]bool `json:"-"`
}

// IsBreak reports whether the programme is a commercial or an ident (programs.BreakFormats).
func (p *Programme) IsBreak() bool { return slices.Contains(programs.BreakFormats, p.Format) }

// document is the on-disk shape of a Programme: theme / summary / next_time absent for a break, minutes as
// Python's float repr, empty lists as [] (never null).
type document struct {
	ID            string          `json:"id"`
	Format        string          `json:"format"`
	Slot          *int            `json:"slot"`
	Title         string          `json:"title"`
	Theme         *string         `json:"theme,omitempty"`
	Summary       *string         `json:"summary,omitempty"`
	NextTime      *string         `json:"next_time,omitempty"`
	Cast          []programs.Cast `json:"cast"`
	Minutes       pyFloat         `json:"minutes"`
	Sources       []string        `json:"sources"`
	Written       string          `json:"written"`
	Sections      []Section       `json:"sections"`
	SegmentsTotal int             `json:"segments_total"`
	SecondsTotal  int             `json:"seconds_total"`
}

// MarshalJSON writes the document shape (see document); HTML is not escaped (prompts contain <d> tags).
func (p Programme) MarshalJSON() ([]byte, error) {
	d := document{ID: p.ID, Format: p.Format, Slot: p.Slot, Title: p.Title, Cast: p.Cast, Minutes: pyFloat(p.Minutes),
		Sources: p.Sources, Written: p.Written, Sections: p.Sections, SegmentsTotal: p.SegmentsTotal, SecondsTotal: p.SecondsTotal}
	if !p.IsBreak() {
		d.Theme, d.Summary, d.NextTime = &p.Theme, &p.Summary, &p.NextTime
	}
	if d.Cast == nil {
		d.Cast = []programs.Cast{}
	}
	if d.Sources == nil {
		d.Sources = []string{}
	}
	if d.Sections == nil {
		d.Sections = []Section{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// pyFloat marshals like Python's float repr ("6.0", "0.1"), which is what the Python writer put in "minutes".
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	s := strconv.FormatFloat(float64(f), 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return []byte(s), nil
}

// IndexedSegment is one clip with its position in air order.
type IndexedSegment struct {
	Index   int
	Section string
	Segment Segment
}

// Segments lists every clip of the programme in air order with a running 0-based index.
func (p *Programme) Segments() []IndexedSegment {
	var out []IndexedSegment
	i := 0
	for _, sec := range p.Sections {
		for _, seg := range sec.Segments {
			out = append(out, IndexedSegment{Index: i, Section: sec.Name, Segment: seg})
			i++
		}
	}
	return out
}

// ws is one whitespace character, Unicode separators (the full-width space U+3000) included.
const ws = `[\s\p{Z}\x{85}]`

var (
	reFence     = regexp.MustCompile("^```\\w*" + ws + "*|" + ws + "*```$")
	reCloseTag  = regexp.MustCompile(`\[/d\]|</d` + ws + `*>`)
	reDupClose  = regexp.MustCompile(`</d>(?:` + ws + `*</d>)+`)
	reLangAlias = regexp.MustCompile(`(?i)<d>` + ws + `*\[(?:ja|jp|japanese|日本語)\]` + ws + `*`)
	// the three field names; the word boundary before them is checked by replaceField
	reFieldIMD  = regexp.MustCompile(`(?i)integrated[-_ ]multimodal[-_ ]description` + ws + `*:`)
	reFieldOS   = regexp.MustCompile(`(?i)overall[-_ ]soundscape` + ws + `*:`)
	reFieldNDM  = regexp.MustCompile(`(?i)non[-_ ]diegetic[-_ ]music` + ws + `*:`)
	reD         = regexp.MustCompile(`(?s)<d>\[Japanese\]` + ws + `*(.*?)</d>`)
	reSemicolon = regexp.MustCompile(`[;；]` + ws + `*`)
	reSentence  = regexp.MustCompile(`[^。！？!?]+[。！？!?]?`)
)

// SanitizePrompt makes LLM output safe for H3 (aitv/script.py sanitize_prompt, in this order):
//  1. strip a leading ```lang fence and a trailing ``` (after trimming whitespace);
//  2. "[/d]" and "</d >" -> "</d>"; runs of "</d>" (with whitespace between) -> one;
//  3. "<d>[ja]" / "[jp]" / "[japanese]" / "[日本語]" (case-insensitive, optional whitespace) -> "<d>[Japanese] ";
//  4. a "<d>" not followed (after optional whitespace) by "[" -> "<d>[Japanese] ";
//  5. the three field names spelled with -, _ or space and any case -> integrated_multimodal_description: /
//     overall_soundscape: / non_diegetic_music:;
//  6. inside every <d>[Japanese] ... </d>, drop ";" and "；" (and following whitespace) and trim, re-emitting
//     "<d>[Japanese] <text></d>".
func SanitizePrompt(prompt string) string {
	p := reFence.ReplaceAllString(strings.TrimSpace(prompt), "")
	p = reCloseTag.ReplaceAllString(p, "</d>")
	p = reDupClose.ReplaceAllString(p, "</d>")
	p = reLangAlias.ReplaceAllString(p, "<d>"+LangTag+" ")
	p = tagBareDialogue(p)
	p = replaceField(p, reFieldIMD, "integrated_multimodal_description:")
	p = replaceField(p, reFieldOS, "overall_soundscape:")
	p = replaceField(p, reFieldNDM, "non_diegetic_music:")
	p = reD.ReplaceAllStringFunc(p, func(m string) string {
		text := reD.FindStringSubmatch(m)[1]
		return "<d>" + LangTag + " " + strings.TrimSpace(reSemicolon.ReplaceAllString(text, "")) + "</d>"
	})
	return p
}

// tagBareDialogue gives every <d> that is not followed (after optional whitespace) by "[" the language tag,
// consuming that whitespace (RE2 has no lookahead for `<d>(?!\s*\[)\s*`).
func tagBareDialogue(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "<d>")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i+3])
		rest := strings.TrimLeftFunc(s[i+3:], unicode.IsSpace)
		if strings.HasPrefix(rest, "[") {
			s = s[i+3:]
			continue
		}
		b.WriteString(LangTag + " ")
		s = rest
	}
}

// replaceField replaces every match of re that sits at a word boundary (start of text or a preceding rune
// that is not a letter, digit or underscore — RE2's \b is ASCII-only) with repl.
func replaceField(s string, re *regexp.Regexp, repl string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		if r, _ := utf8.DecodeLastRuneInString(s[:loc[0]]); loc[0] > 0 && (r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)) {
			b.WriteString(s[loc[0]:loc[1]])
		} else {
			b.WriteString(repl)
		}
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// SpeechChars counts the runes of all <d>[Japanese] ... </d> bodies (trimmed).
func SpeechChars(prompt string) int {
	n := 0
	for _, m := range reD.FindAllStringSubmatch(prompt, -1) {
		n += utf8.RuneCountInString(strings.TrimSpace(m[1]))
	}
	return n
}

// FitSpeech gives the dialogue room: lengthen the clip up to SegmentMaxS (ceil(chars / CharsPerS)), then
// drop trailing sentences (split on 。！？!? keeping the punctuation) until what is left fits
// int(CharsPerS*seconds) runes; the very first sentence of the clip is always kept even if too long; a
// tag whose text becomes empty is removed entirely. Returns the prompt and the (possibly longer) seconds.
func FitSpeech(prompt string, seconds int) (string, int) {
	chars := SpeechChars(prompt)
	if float64(chars) <= CharsPerS*float64(seconds) {
		return prompt, seconds
	}
	seconds = max(seconds, min(programs.SegmentMaxS, int(math.Ceil(float64(chars)/CharsPerS))))
	budget := int(CharsPerS * float64(seconds))
	if chars <= budget {
		return prompt, seconds
	}
	var keptParts []string
	anyKept := false
	for _, m := range reD.FindAllStringSubmatch(prompt, -1) {
		kept := ""
		keptLen := 0
		for _, s := range reSentence.FindAllString(strings.TrimSpace(m[1]), -1) {
			n := utf8.RuneCountInString(s)
			if keptLen+n > budget && (anyKept || kept != "") {
				break // the very first sentence of the clip is always kept, even if it is too long
			}
			kept += s
			keptLen += n
			anyKept = true
		}
		budget = max(0, budget-keptLen)
		keptParts = append(keptParts, strings.TrimSpace(kept))
	}
	i := 0
	out := reD.ReplaceAllStringFunc(prompt, func(string) string {
		t := keptParts[i]
		i++
		if t == "" {
			return ""
		}
		return "<d>" + LangTag + " " + t + "</d>"
	})
	return out, seconds
}

// ---- lenient views of the LLM's JSON values ----
//
// A reply is decoded untyped (map[string]any / []any); the model sometimes answers with "60" for 60, "true"
// for true, a number for a string or no key at all. These helpers read what is there and fall back to a
// zero value instead of failing the programme.

// asString is the value as text: a string as is, a number or bool printed, "" for null or a missing key.
func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// strOr is asString(v), or def when that is empty.
func strOr(v any, def string) string {
	if s := asString(v); s != "" {
		return s
	}
	return def
}

// asFloat is the value as a number: a JSON number, a numeric string (" 9.5 "), or a bool (1 / 0). Anything
// else — text, a list, null, NaN, ±Inf — is not a number.
func asFloat(v any) (float64, bool) {
	var f float64
	var err error
	switch x := v.(type) {
	case float64:
		f = x
	case json.Number:
		f, err = x.Float64()
	case string:
		f, err = strconv.ParseFloat(strings.TrimSpace(x), 64)
	case bool:
		if x {
			f = 1
		}
	default:
		return 0, false
	}
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// asBool is the value's truth: a bool as is, a string by strconv.ParseBool ("true", "0") or else by being
// non-blank, a number by being non-zero, a list or object by being non-empty, null false.
func asBool(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(x)); err == nil {
			return b
		}
		return strings.TrimSpace(x) != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	f, ok := asFloat(v)
	return !ok || f != 0
}

// asList is the value as a list (nil for anything else).
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// segments is aitv/script.py _segments: the usable clips of one section reply — the objects with a prompt,
// seconds read leniently (default 8, rounded half to even, clamped to [SegmentMinS, SegmentMaxS]), the
// prompt through SanitizePrompt and FitSpeech, a blank caption replaced by defaultCaption.
func segments(raw any, defaultCaption string) []Segment {
	var out []Segment
	for _, s := range asList(raw) {
		m, ok := s.(map[string]any)
		if !ok || !asBool(m["prompt"]) {
			continue
		}
		secs := 8.0
		if f, ok := asFloat(m["seconds"]); ok && f != 0 {
			secs = f
		}
		secs = max(programs.SegmentMinS, min(programs.SegmentMaxS, math.RoundToEven(secs)))
		prompt, n := FitSpeech(SanitizePrompt(asString(m["prompt"])), int(secs))
		out = append(out, Segment{Caption: strings.TrimSpace(strOr(m["caption"], defaultCaption)), Seconds: n, Prompt: prompt})
	}
	return out
}

// castOf reads one cast entry of the outline; false for a value that is not an object.
func castOf(v any) (programs.Cast, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return programs.Cast{}, false
	}
	return programs.Cast{Role: asString(m["role"]), Name: asString(m["name"]), Look: asString(m["look"]),
		Voice: asString(m["voice"]), Regular: asBool(m["regular"]), Kana: strings.TrimSpace(asString(m["kana"]))}, true
}

// scaleSections turns the outline's sections into OutlineSections whose seconds add up to minutes*60: each
// section's share of the LLM's total (a section without seconds gets an equal share), rounded half to even,
// at least 20 s.
func scaleSections(secs []map[string]any, minutes float64) []programs.OutlineSection {
	target := minutes * 60
	total := 0.0
	for _, s := range secs {
		f, _ := asFloat(s["seconds"])
		total += f
	}
	if total == 0 {
		total = target
	}
	if total == 0 {
		total = 1 // minutes 0: every section gets the 20 s floor
	}
	out := make([]programs.OutlineSection, len(secs))
	for i, s := range secs {
		v, ok := asFloat(s["seconds"])
		if !ok || v == 0 {
			v = target / float64(len(secs))
		}
		out[i] = programs.OutlineSection{Name: asString(s["name"]), Seconds: max(20, int(math.RoundToEven(v*target/total))), Plan: asString(s["plan"])}
	}
	return out
}

// newID is "20060102-150405-" + 6 random hex digits (Python: time.strftime + uuid4().hex[:6]).
func newID() string {
	var b [3]byte
	_, _ = crand.Read(b[:]) // never fails (Go 1.24+ crashes the process instead)
	return now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// encode renders a programme as the file holds it: one-space indent, no HTML escaping (see MarshalJSON).
func encode(p *Programme) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes scripts/<id>.json atomically (tmp + rename). Returns the path.
func Save(p *Programme) (string, error) {
	if p == nil || p.ID == "" {
		return "", errors.New("script without an id")
	}
	dir := scriptsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := encode(p)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, p.ID+".json")
	if err := os.WriteFile(path+".tmp", data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return "", err
	}
	return path, nil
}

// Decode reads a scripts/<id>.json document.
func Decode(data []byte) (*Programme, error) {
	var p Programme
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Load reads scripts/<id>.json.
func Load(id string) (*Programme, error) {
	id = strings.TrimSuffix(id, ".json")
	data, err := os.ReadFile(filepath.Join(scriptsDir(), id+".json"))
	if err != nil {
		return nil, err
	}
	p, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("script %s: %w", id, err)
	}
	return p, nil
}

// episodeOf summarises a stored programme for the writer.
func episodeOf(p *Programme) programs.Episode {
	parts := []string{p.Title, p.Theme, p.Summary}
	for _, s := range p.Sections {
		plan := ""
		if s.Plan != nil {
			plan = *s.Plan
		}
		parts = append(parts, s.Name+" "+plan)
	}
	return programs.Episode{Written: p.Written, Title: p.Title, Theme: p.Theme, Summary: p.Summary, NextTime: p.NextTime,
		Cast: p.Cast, Sources: p.Sources, Text: strings.Join(parts, " ")}
}

// History returns the last n episodes of a format (n <= 0: all), oldest first (sorted by Written): only
// scripts with sections whose id appears as a programme_id in the index (a script nobody rendered never
// aired). Episode.Text is title, theme, summary and every "section name section plan" joined by single
// spaces. A scripts/*.json that cannot be read or decoded is skipped; a missing scripts directory is no
// history at all.
func History(fmtKey string, n int) ([]programs.Episode, error) {
	entries, err := os.ReadDir(scriptsDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rendered := map[string]bool{}
	for _, it := range loadIndex() {
		rendered[it.ProgrammeID] = true
	}
	var eps []programs.Episode
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p, err := Load(e.Name())
		if err != nil || p.Format != fmtKey || len(p.Sections) == 0 || !rendered[p.ID] {
			continue
		}
		eps = append(eps, episodeOf(p))
	}
	sort.SliceStable(eps, func(i, j int) bool { return eps[i].Written < eps[j].Written })
	if n > 0 && len(eps) > n {
		eps = eps[len(eps)-n:]
	}
	return eps, nil
}

// themeKeys is THEME_KEYS.get(theme, (theme,)).
func themeKeys(keys map[string][]string, theme string) []string {
	if k, ok := keys[theme]; ok {
		return k
	}
	return []string{theme}
}

// PickThemes returns the least recently used themes of a format (aitv/script.py pick_themes): themes never
// seen in recent[].Text (case-insensitive substring of any ThemeKeys entry, or the theme itself) shuffled
// first, then the seen ones by last use ascending. A teaser (recent[-1].NextTime) that names listed themes
// promotes them to the front (unless it repeats recent[-2].NextTime, or the theme was used in that very
// episode); a teaser that names no listed theme returns nil for single-theme formats (the writer follows
// the teaser instead). At most ThemeCount[fmt] (default 1) are returned; nil for formats without themes.
func PickThemes(fmtKey string, recent []programs.Episode, rng *rand.Rand) []string {
	pool := programs.Themes[fmtKey]
	if len(pool) == 0 {
		return nil
	}
	count, ok := programs.ThemeCount[fmtKey]
	if !ok {
		count = 1
	}
	return pickThemes(pool, programs.ThemeKeys, count, recent, rng)
}

// pickThemes is PickThemes with the tables passed in.
func pickThemes(pool []string, keys map[string][]string, count int, recent []programs.Episode, rng *rand.Rand) []string {
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	mentions := func(hay, theme string) bool {
		for _, k := range themeKeys(keys, theme) {
			if strings.Contains(hay, strings.ToLower(k)) {
				return true
			}
		}
		return false
	}
	lastUsed := map[string]int{}
	var seen []string // themes in the order they were first seen
	for i, e := range recent {
		hay := strings.ToLower(e.Text)
		for _, t := range pool {
			if mentions(hay, t) {
				if _, ok := lastUsed[t]; !ok {
					seen = append(seen, t)
				}
				lastUsed[t] = i
			}
		}
	}
	var never []string
	for _, t := range pool {
		if _, ok := lastUsed[t]; !ok {
			never = append(never, t)
		}
	}
	rng.Shuffle(len(never), func(i, j int) { never[i], never[j] = never[j], never[i] })
	sort.SliceStable(seen, func(i, j int) bool { return lastUsed[seen[i]] < lastUsed[seen[j]] })
	ranked := append(never, seen...)
	// a teaser at the end of the last episode is a promise: whatever it named comes first; a teaser that
	// names no listed theme still wins over the rotation (the writer follows it instead), unless it is
	// just the previous teaser repeated (the promise was already kept or ignored)
	teaser := ""
	if len(recent) > 0 {
		teaser = strings.ToLower(recent[len(recent)-1].NextTime)
	}
	repeated := len(recent) > 1 && teaser != "" && teaser == strings.ToLower(recent[len(recent)-2].NextTime)
	var promised []string
	if teaser != "" && !repeated {
		for _, t := range pool {
			if lu, ok := lastUsed[t]; mentions(teaser, t) && !(ok && lu == len(recent)-1) {
				promised = append(promised, t)
			}
		}
	}
	if teaser != "" && !repeated && len(promised) == 0 && count == 1 {
		return nil
	}
	out := slices.Clone(promised)
	for _, t := range ranked {
		if !slices.Contains(promised, t) {
			out = append(out, t)
		}
	}
	out = out[:max(0, min(count, len(out)))]
	if len(out) == 0 {
		return nil
	}
	return out
}

// chatFunc is llm.(*Client).ChatJSON: the decoded reply (a map[string]any when the model answered with an
// object).
type chatFunc func(system, user string) (any, error)

// WriteProgramme writes outline + every section and saves the script (aitv/script.py write_programme).
// material nil = none; slot nil = not a lineup slot; log nil = logx.Printf. Steps:
//   - History(8); regulars = the last episode's Regular cast; themes = PickThemes;
//   - news: Reported = titles from episodes written within the last 36 h, deduplicated, last 40;
//   - outline via ChatJSON: a reply that is not an object or has no named section is an error; each
//     section's seconds is scaled so they add up to minutes*60 (see scaleSections);
//   - cast: the entries that are objects, read leniently (castOf); a blank or placeholder name gets an
//     unused CastNames entry (random order); a regular missing from the cast is re-inserted at the front,
//     a present one gets look/voice/role restored from the previous episode and Regular=true;
//   - id = "20060102-150405-" + 6 random hex; log "[aitv] outline: <title> [<theme>] — name(secs) / ... — next: ...";
//   - each section via SectionPrompt + ChatJSON (a failing or empty section is logged and skipped; the previous
//     clip's caption is passed on); segments through SanitizePrompt/FitSpeech (see segments);
//   - no section at all -> error "script without any section"; totals; Save; log "[aitv] script: ...".
func WriteProgramme(client *llm.Client, fmtKey string, material *sources.Material, minutes float64, slot *int, log logx.Logger, rng *rand.Rand) (*Programme, error) {
	if client == nil {
		return nil, errors.New("no LLM client")
	}
	chat := func(system, user string) (any, error) { return client.ChatJSON(system, user) }
	return writeProgramme(chat, fmtKey, material, minutes, slot, log, rng)
}

func writeProgramme(chat chatFunc, fmtKey string, material *sources.Material, minutes float64, slot *int, log logx.Logger, rng *rand.Rand) (*Programme, error) {
	f, ok := programs.Formats[fmtKey]
	if !ok {
		return nil, fmt.Errorf("unknown format %q", fmtKey)
	}
	if log == nil {
		log = logx.Printf
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	mtext := ""
	refs := []string{}
	if material != nil {
		mtext = material.Text
		refs = append(refs, material.Refs...)
	}
	recent, err := History(fmtKey, 8)
	if err != nil {
		return nil, err
	}
	var regulars []programs.Cast
	if len(recent) > 0 {
		for _, c := range recent[len(recent)-1].Cast {
			if c.Regular {
				regulars = append(regulars, c)
			}
		}
	}
	themes := PickThemes(fmtKey, recent, rng)
	var reported []string
	if fmtKey == "news" {
		cutoff := now().Add(-36 * time.Hour).Format(timeLayout)
		for _, e := range recent {
			if e.Written < cutoff {
				continue
			}
			for _, t := range e.Sources {
				if !slices.Contains(reported, t) {
					reported = append(reported, t)
				}
			}
		}
		if len(reported) > 40 {
			reported = reported[len(reported)-40:]
		}
	}
	system, user := programs.OutlinePrompt(fmtKey, programs.OutlineInput{Minutes: minutes, Material: mtext, Regulars: regulars,
		Recent: recent[max(0, len(recent)-5):], Themes: themes, Reported: reported})
	reply, err := chat(system, user)
	if err != nil {
		return nil, err
	}
	outline, ok := reply.(map[string]any)
	if !ok {
		return nil, errors.New("outline is not a JSON object")
	}

	// sections: the named ones, seconds scaled to the programme length
	var raw []map[string]any
	for _, s := range asList(outline["sections"]) {
		if m, ok := s.(map[string]any); ok && asString(m["name"]) != "" {
			raw = append(raw, m)
		}
	}
	if len(raw) == 0 {
		return nil, errors.New("outline without sections")
	}
	sections := scaleSections(raw, minutes)

	// cast: blank or placeholder names get a real one (a regular needs one to be carried from day to day);
	// the regulars are put back canonically (the LLM sometimes drops or rewrites one)
	var cast []programs.Cast
	used := map[string]bool{}
	for _, v := range asList(outline["cast"]) {
		if c, ok := castOf(v); ok {
			cast = append(cast, c)
			used[c.Name] = true
		}
	}
	for i, c := range cast {
		if n := strings.TrimSpace(c.Name); n == "" || PlaceholderNames[n] {
			pick := CastNames[0]
			for _, j := range rng.Perm(len(CastNames)) {
				if !used[CastNames[j]] {
					pick = CastNames[j]
					break
				}
			}
			cast[i].Name = pick
			used[pick] = true
		}
	}
	for _, r := range regulars {
		found := false
		for i, c := range cast {
			if c.Name != r.Name {
				continue
			}
			found = true
			if r.Look != "" {
				cast[i].Look = r.Look
			}
			if r.Voice != "" {
				cast[i].Voice = r.Voice
			}
			if r.Role != "" {
				cast[i].Role = r.Role
			}
			if r.Kana != "" {
				cast[i].Kana = r.Kana
			}
			cast[i].Regular = true
		}
		if !found {
			cast = append([]programs.Cast{r}, cast...)
		}
	}

	prog := &Programme{
		ID:       newID(),
		Format:   fmtKey,
		Slot:     slot,
		Title:    strings.TrimSpace(strOr(outline["title"], f.Title)),
		Theme:    strings.TrimSpace(strOr(outline["theme"], strings.Join(themes, " / "))),
		Summary:  strings.TrimSpace(asString(outline["summary"])),
		NextTime: strings.TrimSpace(asString(outline["next_time"])),
		Cast:     cast,
		Minutes:  minutes,
		Sources:  refs,
		Written:  now().Format(timeLayout),
	}
	plan := make([]string, len(sections))
	for i, s := range sections {
		plan[i] = fmt.Sprintf("%s(%ds)", s.Name, s.Seconds)
	}
	line := fmt.Sprintf("[aitv] outline: %s [%s] — %s", prog.Title, prog.Theme, strings.Join(plan, " / "))
	if prog.NextTime != "" {
		line += " — next: " + prog.NextTime
	}
	log("%s", line)

	// the section writer sees the outline as the LLM gave it (title, theme and teaser as written)
	ol := programs.Outline{Title: asString(outline["title"]), Theme: asString(outline["theme"]), Summary: asString(outline["summary"]),
		NextTime: asString(outline["next_time"]), Cast: cast, Sections: sections}
	prev := ""
	for i, sec := range sections {
		system, user := programs.SectionPrompt(fmtKey, ol, sec, prev, mtext, time.Time{})
		reply, err := chat(system, user)
		if err != nil {
			log("[aitv] section '%s' failed (%v); skipped", sec.Name, err)
			continue
		}
		res, _ := reply.(map[string]any)
		segs := segments(res["segments"], sec.Name)
		if len(segs) == 0 {
			log("[aitv] section '%s' came back empty; skipped", sec.Name)
			continue
		}
		var plan *string // null when the LLM gave none
		if v := raw[i]["plan"]; v != nil {
			plan = new(asString(v))
		}
		prog.Sections = append(prog.Sections, Section{Name: sec.Name, Plan: plan, Seconds: sec.Seconds, Segments: segs})
		prev = segs[len(segs)-1].Caption
	}
	if len(prog.Sections) == 0 {
		return nil, errors.New("script without any section")
	}
	for _, s := range prog.Sections {
		prog.SegmentsTotal += len(s.Segments)
		for _, g := range s.Segments {
			prog.SecondsTotal += g.Seconds
		}
	}
	if _, err := Save(prog); err != nil {
		return nil, err
	}
	log("[aitv] script: %s: %d sections, %d clips, %.1f min -> scripts/%s.json", prog.Title, len(prog.Sections), prog.SegmentsTotal,
		float64(prog.SecondsTotal)/60, prog.ID)
	return prog, nil
}

// WriteBreak writes one commercial or ident (aitv/script.py write_break): BreakPrompt with the titles of the
// last 10 episodes, first segment only, seconds forced to 4 (ident) / 6 (cm), one section named after the
// format's title, minutes = seconds/60. Saved like a programme (without theme / summary / next_time).
func WriteBreak(client *llm.Client, fmtKey string) (*Programme, error) {
	if client == nil {
		return nil, errors.New("no LLM client")
	}
	chat := func(system, user string) (any, error) { return client.ChatJSON(system, user) }
	return writeBreak(chat, fmtKey)
}

func writeBreak(chat chatFunc, fmtKey string) (*Programme, error) {
	f, ok := programs.Formats[fmtKey]
	if !ok {
		return nil, fmt.Errorf("unknown format %q", fmtKey)
	}
	recent, err := History(fmtKey, 10)
	if err != nil {
		return nil, err
	}
	var titles []string
	for _, e := range recent {
		if e.Title != "" {
			titles = append(titles, e.Title)
		}
	}
	system, user := programs.BreakPrompt(fmtKey, titles)
	reply, err := chat(system, user)
	if err != nil {
		return nil, err
	}
	res, _ := reply.(map[string]any)
	segs := segments(res["segments"], f.Title)
	if len(segs) == 0 {
		return nil, errors.New("no segment for " + fmtKey)
	}
	secs := 6
	if fmtKey == "ident" {
		secs = 4
	}
	seg := segs[0]
	seg.Seconds = secs
	prog := &Programme{
		ID:            newID(),
		Format:        fmtKey,
		Title:         strings.TrimSpace(strOr(res["title"], f.Title)),
		Minutes:       float64(secs) / 60,
		Written:       now().Format(timeLayout),
		Sections:      []Section{{Name: f.Title, Plan: new(""), Seconds: secs, Segments: []Segment{seg}}},
		SegmentsTotal: 1,
		SecondsTotal:  secs,
	}
	if _, err := Save(prog); err != nil {
		return nil, err
	}
	return prog, nil
}
