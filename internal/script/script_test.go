package script

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/llm"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/programs"
)

// ---------- pure functions against the Python goldens (golden_test.go) ----------

func TestSanitizePrompt(t *testing.T) {
	for i, c := range sanitizeGolden {
		if got := SanitizePrompt(c.in); got != c.want {
			t.Errorf("case %d: SanitizePrompt(%q)\n got  %q\n want %q", i, c.in, got, c.want)
		}
	}
	// idempotent on its own output
	for _, c := range sanitizeGolden {
		if got := SanitizePrompt(c.want); got != c.want {
			t.Errorf("not idempotent: %q -> %q", c.want, got)
		}
	}
}

func TestSpeechChars(t *testing.T) {
	for _, c := range speechGolden {
		if got := SpeechChars(c.in); got != c.want {
			t.Errorf("SpeechChars(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFitSpeech(t *testing.T) {
	for i, c := range fitGolden {
		got, secs := FitSpeech(c.in, c.secs)
		if got != c.want || secs != c.wantSecs {
			t.Errorf("case %d: FitSpeech(%q, %d)\n got  (%q, %d)\n want (%q, %d)", i, c.in, c.secs, got, secs, c.want, c.wantSecs)
		}
	}
}

// decodeJSON decodes text the way an LLM reply reaches the writer (untyped: map[string]any / []any / float64).
func decodeJSON(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLenientValues(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{{nil, ""}, {"x", "x"}, {7.0, "7"}, {1.5, "1.5"}, {true, "true"}, {json.Number("60"), "60"}} {
		if got := asString(c.in); got != c.want {
			t.Errorf("asString(%#v) = %q, want %q", c.in, got, c.want)
		}
	}
	if strOr("", "d") != "d" || strOr(nil, "d") != "d" || strOr(" ", "d") != " " || strOr(0.0, "d") != "0" {
		t.Error("strOr")
	}
	for _, c := range []struct {
		in   any
		want float64
		ok   bool
	}{{60.0, 60, true}, {"60", 60, true}, {" 9.5 ", 9.5, true}, {json.Number("8"), 8, true}, {true, 1, true}, {false, 0, true},
		{"abc", 0, false}, {"60s", 0, false}, {"９", 0, false}, {"nan", 0, false}, {"inf", 0, false}, {json.Number("1e400"), 0, false},
		{nil, 0, false}, {[]any{1.0}, 0, false}, {map[string]any{}, 0, false}} {
		if got, ok := asFloat(c.in); got != c.want || ok != c.ok {
			t.Errorf("asFloat(%#v) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		in   any
		want bool
	}{{nil, false}, {true, true}, {false, false}, {"true", true}, {"false", false}, {"0", false}, {"yes", true}, {"", false}, {" ", false},
		{1.0, true}, {0.0, false}, {json.Number("2"), true}, {[]any{}, false}, {[]any{1.0}, true}, {map[string]any{}, false}, {map[string]any{"a": 1}, true}} {
		if got := asBool(c.in); got != c.want {
			t.Errorf("asBool(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSegments(t *testing.T) {
	var want []Segment
	if err := json.Unmarshal([]byte(segmentsWantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if got := segments(decodeJSON(t, segmentsRawJSON), "既定"); !reflect.DeepEqual(got, want) {
		t.Errorf("segments mismatch\n got  %+v\n want %+v", got, want)
	}
	// anything that is not a list of objects yields no clips
	for _, v := range []any{"not a list", nil, false, 5.0, "", []any{}, map[string]any{"prompt": "x"}} {
		if got := segments(v, "x"); got != nil {
			t.Errorf("segments(%#v) = %v; want nothing", v, got)
		}
	}
	// seconds the model spells oddly: a numeric string counts, anything else is the default 8
	for in, want := range map[any]int{"9": 9, " 9 ": 9, "9.5": 10, "abc": 8, "９": 8, "9x": 8, "nan": 8, "inf": 8, 1e300: programs.SegmentMaxS, -4.0: 6, true: 6} {
		got := segments([]any{map[string]any{"caption": "c", "seconds": in, "prompt": "x"}}, "d")
		if len(got) != 1 || got[0].Seconds != want {
			t.Errorf("seconds %#v -> %+v, want %d", in, got, want)
		}
	}
}

// sections builds the outline's section list from a JSON list.
func sections(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, v := range decodeJSON(t, raw).([]any) {
		out = append(out, v.(map[string]any))
	}
	return out
}

func TestScaleSections(t *testing.T) {
	for _, c := range scaleGolden {
		out := scaleSections(sections(t, c.rawJSON), c.minutes)
		got := make([]int, len(out))
		for i, s := range out {
			got[i] = s.Seconds
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("scaleSections(%s, %g) = %v, want %v", c.rawJSON, c.minutes, got, c.want)
		}
	}
	// names and plans are carried (a number printed, a missing plan empty); a seconds the model spelled
	// as "60s" counts as missing and gets an equal share of the target (120 / 2 = 60, scaled by 120 / 60)
	out := scaleSections(sections(t, `[{"name": 5, "seconds": "60s"}, {"name": "b", "plan": "p", "seconds": 60.0}]`), 2)
	if out[0].Name != "5" || out[0].Plan != "" || out[1].Plan != "p" || out[0].Seconds != 120 || out[1].Seconds != 120 {
		t.Errorf("unexpected %+v", out)
	}
	if out := scaleSections(sections(t, `[{"name": "a"}]`), 0); out[0].Seconds != 20 {
		t.Errorf("minutes 0: %+v", out)
	}
}

// ---------- the JSON document ----------

// fixtureScripts is testdata/scripts (copies of real aitv/library/scripts files plus edge cases).
func fixtureScripts(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fixtureIndex decodes testdata/index.json.
func fixtureIndex(t *testing.T) []index.Item {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var items []index.Item
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatal(err)
	}
	return items
}

// useFixtures points the package at a scripts dir and the fixture index, and restores the seams afterwards.
func useFixtures(t *testing.T, scripts string) {
	t.Helper()
	items := fixtureIndex(t)
	oldDir, oldIdx, oldNow := scriptsDir, loadIndex, now
	scriptsDir = func() string { return scripts }
	loadIndex = func() []index.Item { return items }
	t.Cleanup(func() { scriptsDir, loadIndex, now = oldDir, oldIdx, oldNow })
}

func pinClock(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

func scriptFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != "bad.json" {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// TestProgrammeRoundTrip: every Python-written script (the fixtures and, when present, the real library)
// decodes into Programme and re-encodes to a value-equal document: the same keys (theme / summary /
// next_time for a programme, none of them for a break), minutes as "6.0", null plans, the cast entries.
func TestProgrammeRoundTrip(t *testing.T) {
	dirs := []string{fixtureScripts(t)}
	if real, err := filepath.Abs(filepath.Join("..", "..", "aitv", "library", "scripts")); err == nil {
		if st, err := os.Stat(real); err == nil && st.IsDir() {
			dirs = append(dirs, real)
		}
	}
	n := 0
	for _, dir := range dirs {
		for _, path := range scriptFiles(t, dir) {
			orig, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			p, err := Decode(orig)
			if err != nil {
				t.Errorf("%s: %v", path, err)
				continue
			}
			if p.ID != strings.TrimSuffix(filepath.Base(path), ".json") {
				t.Errorf("%s: decoded id %q", path, p.ID)
			}
			data, err := encode(p)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Decode(data)
			if err != nil {
				t.Errorf("%s: re-decode: %v", path, err)
				continue
			}
			if !reflect.DeepEqual(p, back) {
				t.Errorf("%s: round trip changed the programme", path)
			}
			var want, got map[string]any
			if err := json.Unmarshal(orig, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			// the oldest real files predate "regular" and a programme's theme / summary / next_time: those
			// are written with their zero values
			for _, c := range want["cast"].([]any) {
				if m := c.(map[string]any); m["regular"] == nil {
					m["regular"] = false
				}
			}
			if !slices.Contains(programs.BreakFormats, want["format"].(string)) {
				for _, k := range []string{"theme", "summary", "next_time"} {
					if _, ok := want[k]; !ok {
						want[k] = ""
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s: re-encoded document differs\n got  %.400s\n want %.400s", path, data, orig)
			}
			if !strings.HasPrefix(string(data), "{\n \"id\": ") || strings.Contains(string(data), `\u003c`) {
				t.Errorf("%s: not one-space indented / HTML-escaped:\n%.200s", path, data)
			}
			n++
		}
	}
	if n < 7 {
		t.Errorf("only %d scripts round-tripped", n)
	}
}

func TestSaveLoad(t *testing.T) {
	useFixtures(t, t.TempDir())
	one := 3
	p := &Programme{ID: "20260917-120000-abcdef", Format: "nature", Slot: &one, Title: "t", Theme: "th", Minutes: 6,
		Written: "2026-09-17T12:00:00", Sections: []Section{{Name: "s", Plan: new("p"), Seconds: 60, Segments: []Segment{{Caption: "c", Seconds: 8, Prompt: "<d>[Japanese] やあ</d>"}}}},
		SegmentsTotal: 1, SecondsTotal: 8, Done: map[int]bool{0: true}}
	path, err := Save(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(scriptsDir(), p.ID+".json"); path != want {
		t.Errorf("path %q, want %q", path, want)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file left behind")
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	for _, want := range []string{"\"cast\": [],", "\"sources\": [],", "\"slot\": 3,", "\"minutes\": 6.0,", "<d>[Japanese] やあ</d>", "\"theme\": \"th\",",
		"\"summary\": \"\",", "\"next_time\": \"\",", "\"plan\": \"p\","} {
		if !strings.Contains(s, want) {
			t.Errorf("saved document lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Done") {
		t.Errorf("Done must not be written:\n%s", s)
	}
	got, err := Load(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "t" || *got.Slot != 3 || got.Cast == nil || got.Sources == nil || got.Done != nil || got.Sections[0].Segments[0].Prompt != p.Sections[0].Segments[0].Prompt {
		t.Errorf("Load: %+v", got)
	}
	if got2, err := Load(p.ID + ".json"); err != nil || got2.ID != p.ID {
		t.Error("Load tolerates a .json suffix")
	}
	// overwrite in place
	p.Title = "t2"
	if _, err := Save(p); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(p.ID); got.Title != "t2" {
		t.Error("Save did not replace the file")
	}
	if _, err := Load("missing"); err == nil {
		t.Error("Load of a missing script must fail")
	}
	if _, err := Save(&Programme{}); err == nil {
		t.Error("Save without an id must fail")
	}
}

func TestSegmentsIter(t *testing.T) {
	p := &Programme{Sections: []Section{
		{Name: "a", Segments: []Segment{{Caption: "a0"}, {Caption: "a1"}}},
		{Name: "b"},
		{Name: "c", Segments: []Segment{{Caption: "c0"}}},
	}}
	got := p.Segments()
	want := []IndexedSegment{{0, "a", Segment{Caption: "a0"}}, {1, "a", Segment{Caption: "a1"}}, {2, "c", Segment{Caption: "c0"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Segments() = %+v", got)
	}
	if (&Programme{}).Segments() != nil {
		t.Error("empty programme")
	}
}

// ---------- History ----------

// episodeJSON is the dict Python's history() builds, for comparing with the golden.
type episodeJSON struct {
	Written  string          `json:"written"`
	Title    string          `json:"title"`
	Theme    string          `json:"theme"`
	Summary  string          `json:"summary"`
	NextTime string          `json:"next_time"`
	Cast     []programs.Cast `json:"cast"`
	Sources  []string        `json:"sources"`
	Text     string          `json:"text"`
}

func episodesAsJSON(t *testing.T, eps []programs.Episode) string {
	t.Helper()
	out := make([]episodeJSON, len(eps))
	for i, e := range eps {
		out[i] = episodeJSON{e.Written, e.Title, e.Theme, e.Summary, e.NextTime, e.Cast, e.Sources, e.Text}
		if out[i].Cast == nil {
			out[i].Cast = []programs.Cast{}
		}
		if out[i].Sources == nil {
			out[i].Sources = []string{}
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goldenEpisodes(t *testing.T, golden string) string {
	t.Helper()
	var eps []episodeJSON
	if err := json.Unmarshal([]byte(golden), &eps); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(eps)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHistory(t *testing.T) {
	useFixtures(t, fixtureScripts(t))
	cases := []struct {
		fmt    string
		n      int
		golden string
	}{
		{"nature", 8, historyNature8JSON}, {"nature", 2, historyNature2JSON}, {"cooking", 8, historyCooking8JSON},
		{"ident", 10, historyIdent10JSON}, {"cm", 10, historyCm10JSON}, {"news", 8, historyNews8JSON},
	}
	for _, c := range cases {
		got := episodesAsJSON(t, mustHistory(t, c.fmt, c.n))
		want := goldenEpisodes(t, c.golden)
		if got != want {
			t.Errorf("History(%q, %d)\n got  %s\n want %s", c.fmt, c.n, got, want)
		}
	}
	// the rules, spelled out: three nature scripts are rendered and have sections; the synthetic one has no
	// sections; the second cooking script is not in the index; bad.json and README.txt are ignored
	nature := mustHistory(t, "nature", 8)
	if len(nature) != 3 || nature[0].Written > nature[1].Written || nature[1].Written > nature[2].Written {
		t.Errorf("nature history: %d episodes, not oldest first", len(nature))
	}
	for _, e := range nature {
		if e.Title == "空の回" {
			t.Error("a script without sections must not count")
		}
	}
	if cooking := mustHistory(t, "cooking", 8); len(cooking) != 1 || cooking[0].Written != "2026-09-16T01:53:23" {
		t.Errorf("cooking history: %+v", cooking)
	}
	if len(nature[2].Cast) != 3 || !nature[2].Cast[0].Regular || nature[2].Cast[0].Name != "中村 千夏" {
		t.Errorf("cast not carried: %+v", nature[2].Cast)
	}
	if len(mustHistory(t, "nature", 0)) != 3 {
		t.Error("n <= 0 returns everything")
	}
	// a file that is not a script document is skipped
	dir := copyFixtureScripts(t)
	scriptsDir = func() string { return dir }
	for _, content := range []string{"[]", "null", "42", "{}", "not json", "{\"format\": \"nature\", \"sections\": \"x\"}"} {
		if err := os.WriteFile(filepath.Join(dir, "zz.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if eps := mustHistory(t, "nature", 8); len(eps) != 3 {
			t.Errorf("%s: %d episodes, want the 3 good ones", content, len(eps))
		}
	}
	// missing directory
	scriptsDir = func() string { return filepath.Join(t.TempDir(), "nope") }
	if mustHistory(t, "nature", 8) != nil {
		t.Error("missing scripts dir must yield nothing")
	}
}

func mustHistory(t *testing.T, fmtKey string, n int) []programs.Episode {
	t.Helper()
	eps, err := History(fmtKey, n)
	if err != nil {
		t.Fatalf("History(%q, %d): %v", fmtKey, n, err)
	}
	return eps
}

// ---------- PickThemes ----------

// The rotation tables of aitv/programs.py for the formats used below (verbatim), so the rules can be tested
// without depending on the programs package's tables.
var (
	pyNature = []string{"深海", "熱帯雨林の樹冠", "北極のツンドラ", "砂漠の夜", "山の渓流", "マングローブ", "高山の草原", "サンゴ礁",
		"サバンナ", "竹林", "湿原", "火山島", "氷河", "干潟", "夜の森", "海氷の縁"}
	pyMusic = []string{"シティポップ", "ジャズトリオ", "アイドルポップ", "ロック", "アコースティックフォーク", "エレクトロニカ", "R&B",
		"ボサノバ", "ヒップホップ", "ブラスバンド", "エレクトロスウィング", "演歌風バラード", "ファンク", "アンビエント", "パンク", "ピアノソロ"}
	pyCooking = []string{"和食の煮物", "麺料理", "丼もの", "パンとサンドイッチ", "スープ", "サラダ", "スイーツ", "イタリアン", "中華",
		"エスニック", "朝ごはん", "お弁当", "鍋料理", "揚げ物", "焼き菓子", "魚料理"}
	pyKeys = map[string][]string{
		"シティポップ": {"シティポップ", "シティ・ポップ", "city pop", "city-pop"}, "ジャズトリオ": {"ジャズ", "jazz"},
		"アイドルポップ": {"アイドル", "idol"}, "ロック": {"ロック", "rock"}, "アコースティックフォーク": {"フォーク", "folk", "アコースティック", "acoustic"},
		"エレクトロニカ": {"エレクトロニカ", "electronic", "electronica", "synth"}, "R&B": {"r&b", "rnb", "ソウル", "soul"},
		"ボサノバ": {"ボサノバ", "bossa"}, "ヒップホップ": {"ヒップホップ", "hip hop", "hip-hop", "rap"}, "ブラスバンド": {"ブラス", "brass"},
		"エレクトロスウィング": {"スウィング", "swing"}, "演歌風バラード": {"演歌", "enka", "バラード", "ballad"}, "ファンク": {"ファンク", "funk"},
		"アンビエント": {"アンビエント", "ambient"}, "パンク": {"パンク", "punk"}, "ピアノソロ": {"ピアノ", "piano"},
		"深海": {"深海", "deep sea", "deep-sea"}, "熱帯雨林の樹冠": {"熱帯雨林", "rainforest", "ジャングル"}, "北極のツンドラ": {"北極", "ツンドラ", "arctic", "tundra"},
		"砂漠の夜": {"砂漠", "desert"}, "山の渓流": {"渓流", "mountain river", "清流"}, "マングローブ": {"マングローブ", "mangrove"},
		"高山の草原": {"高山", "alpine"}, "サンゴ礁": {"サンゴ", "coral"}, "サバンナ": {"サバンナ", "savanna"}, "竹林": {"竹林", "bamboo"},
		"湿原": {"湿原", "wetland", "marsh"}, "火山島": {"火山", "volcan"}, "氷河": {"氷河", "glacier"}, "干潟": {"干潟", "tidal flat", "mudflat"},
		"夜の森": {"夜の森", "night forest"}, "海氷の縁": {"海氷", "sea ice"},
	}
)

func ep(text, next string) programs.Episode { return programs.Episode{Text: text, NextTime: next} }

func TestPickThemesRules(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	// no history: one theme from the pool
	got := pickThemes(pyNature, pyKeys, 1, nil, rng)
	if len(got) != 1 || !contains(pyNature, got[0]) {
		t.Errorf("no history: %v", got)
	}
	// every theme used: the least recently used one wins, ties by first sighting
	var recent []programs.Episode
	for i := len(pyNature) - 1; i >= 0; i-- { // used in reverse pool order: 海氷の縁 first ... 深海 last
		recent = append(recent, ep("Episode about "+pyNature[i]+" ...", ""))
	}
	if got := pickThemes(pyNature, pyKeys, 1, recent, rng); !reflect.DeepEqual(got, []string{"海氷の縁"}) {
		t.Errorf("least recently used: %v", got)
	}
	// two used in the same episode, the rest never (shuffled first): 3 themes -> the never-used ones only
	recent = []programs.Episode{ep("jazz night with a rock band", "")}
	got = pickThemes(pyMusic, pyKeys, 3, recent, rng)
	if len(got) != 3 || contains(got, "ジャズトリオ") || contains(got, "ロック") {
		t.Errorf("never-used first: %v", got)
	}
	// all used, 3 wanted: ordered by last use, ties in first-seen order
	recent = []programs.Episode{ep("ロック / R&B / punk", ""), ep("jazz, ambient, piano and everything else: シティポップ アイドル フォーク synth ボサノバ hip hop ブラス swing 演歌 ファンク", "")}
	got = pickThemes(pyMusic, pyKeys, 3, recent, rng)
	if !reflect.DeepEqual(got, []string{"ロック", "R&B", "パンク"}) {
		t.Errorf("ordered by last use: %v", got)
	}
	// a teaser naming a theme promotes it, unless it was used in that very episode
	recent = []programs.Episode{ep("深海", ""), ep("竹林", "次回は、氷に覆われた静寂の地、北極の物語をお届けします。")}
	if got := pickThemes(pyNature, pyKeys, 1, recent, rng); !reflect.DeepEqual(got, []string{"北極のツンドラ"}) {
		t.Errorf("promised: %v", got)
	}
	recent = []programs.Episode{ep("深海", ""), ep("北極 episode", "次回も北極です。")}
	if got := pickThemes(pyNature, pyKeys, 1, recent, rng); got != nil {
		t.Errorf("a teaser naming only this episode's own theme promises nothing (got %v)", got)
	}
	// a teaser naming nothing listed: nil for a single-theme format, rotation for music_show
	recent = []programs.Episode{ep("深海", "次回は海へ。")}
	if got := pickThemes(pyNature, pyKeys, 1, recent, rng); got != nil {
		t.Errorf("unlisted teaser: %v", got)
	}
	if got := pickThemes(pyMusic, pyKeys, 3, recent, rng); len(got) != 3 {
		t.Errorf("unlisted teaser, three acts: %v", got)
	}
	// a repeated teaser is void
	recent = []programs.Episode{ep("深海", "次回は海へ。"), ep("竹林", "次回は海へ。")}
	got = pickThemes(pyNature, pyKeys, 1, recent, rng)
	if len(got) != 1 || got[0] == "深海" || got[0] == "竹林" {
		t.Errorf("repeated teaser falls back to the rotation: %v", got)
	}
	recent = []programs.Episode{ep("深海", "次回は北極。"), ep("竹林", "次回は北極。")}
	got = pickThemes(pyNature, pyKeys, 1, recent, rng)
	if len(got) != 1 || got[0] == "北極のツンドラ" {
		t.Errorf("repeated promise is not kept twice: %v", got)
	}
	// case-insensitive keys; a theme without keys matches on its name
	recent = []programs.Episode{ep("A DEEP-SEA story", "")}
	for range 20 {
		if got := pickThemes(pyNature, pyKeys, 1, recent, rng); got[0] == "深海" {
			t.Fatal("deep-sea must be recognised as 深海 (case-insensitively)")
		}
	}
	recent = []programs.Episode{ep("今日はスープ", "来週はパンケーキ")}
	if got := pickThemes(pyCooking, nil, 1, recent, rng); got != nil {
		t.Errorf("cooking teaser naming no theme: %v", got)
	}
	recent = []programs.Episode{ep("今日はスープ", "来週はサラダの日")}
	if got := pickThemes(pyCooking, nil, 1, recent, rng); !reflect.DeepEqual(got, []string{"サラダ"}) {
		t.Errorf("cooking teaser naming a theme: %v", got)
	}
	// the shuffle is really random among the never-used
	seen := map[string]bool{}
	for range 200 {
		seen[pickThemes(pyNature, pyKeys, 1, nil, rng)[0]] = true
	}
	if len(seen) < 10 {
		t.Errorf("shuffle too tame: %d distinct", len(seen))
	}
	// nil rng is fine
	if got := pickThemes(pyNature, pyKeys, 1, nil, nil); len(got) != 1 {
		t.Error("nil rng")
	}
}

// TestPickThemes drives the exported function on the fixture history.
func TestPickThemes(t *testing.T) {
	useFixtures(t, fixtureScripts(t))
	rng := rand.New(rand.NewSource(1))
	// Python: pick_themes("nature", history("nature")) == ["砂漠の夜"] (the last teaser promises the desert night)
	if got := PickThemes("nature", mustHistory(t, "nature", 8), rng); !reflect.DeepEqual(got, []string{"砂漠の夜"}) {
		t.Errorf("nature: %v", got)
	}
	// Python: pick_themes("cooking", ...) == [] (the teaser names a pancake, no listed theme)
	if got := PickThemes("cooking", mustHistory(t, "cooking", 8), rng); got != nil {
		t.Errorf("cooking: %v", got)
	}
	for _, f := range []string{"ident", "cm", "news", "weather", "nope"} {
		if got := PickThemes(f, nil, rng); got != nil {
			t.Errorf("%s has no themes: %v", f, got)
		}
	}
	if got := PickThemes("music_show", nil, rng); len(got) != 3 {
		t.Errorf("music_show suggests three: %v", got)
	}
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

// ---------- WriteProgramme / WriteBreak with an httptest Ollama fake ----------

// fakeLLM is an Ollama (/api/chat) and OpenAI (/v1/chat/completions) look-alike answering from canned replies.
type fakeLLM struct {
	srv   *httptest.Server
	mu    sync.Mutex
	users []string // every user prompt, in order
	reply func(user string) (string, bool)
}

func newFakeLLM(t *testing.T, reply func(user string) (string, bool)) *fakeLLM {
	t.Helper()
	f := &fakeLLM{reply: reply}
	h := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		user := ""
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		f.mu.Lock()
		f.users = append(f.users, user)
		f.mu.Unlock()
		content, ok := f.reply(user)
		if !ok {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "done": true})
	}
	f.srv = httptest.NewServer(http.HandlerFunc(h))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) client() *llm.Client {
	return &llm.Client{BaseURL: f.srv.URL, Model: "fake", Timeout: 20 * time.Second, IsOllama: true}
}

func (f *fakeLLM) prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.users...)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// stubChat is a chatFunc answering the outline request with outline and every section request with section
// (JSON text); it records the user prompts.
type stubChat struct {
	outline, section string
	prompts          []string
}

func (s *stubChat) chat(_, user string) (any, error) {
	s.prompts = append(s.prompts, user)
	text := s.outline
	if strings.Contains(user, "Write section") {
		text = s.section
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		panic(err)
	}
	return v, nil
}

const goodSection = `{"segments": [{"caption": "c", "seconds": 8, "prompt": "<d>やあ</d>"}]}`

// copyFixtureScripts copies testdata/scripts into a temp dir so WriteProgramme can save next to the history.
func copyFixtureScripts(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	for _, path := range scriptFiles(t, fixtureScripts(t)) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, filepath.Base(path)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

const long60 = "深い闇が静けさの中に横たわり、夜明け前の竹林はまるで世界から切り離されたひとつの静かな聖域のように見えてくる。"

func TestWriteProgramme(t *testing.T) {
	dir := copyFixtureScripts(t)
	useFixtures(t, dir)
	pinClock(t, time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local))

	// the LLM's values come back sloppy: a padded title, a null theme, a number for the summary, "true" as a
	// string, "60" as a string, a cast entry that is not an object, a nameless section, a section without a plan
	outline := `{"title": " 竹の夜 ", "theme": null, "summary": 7, "next_time": "次回は海へ。",
  "cast": [{"role": "ナレーター", "name": "Anchor", "look": "L", "voice": "V", "regular": "true"},
           {"role": "ゲスト", "name": "中村 千夏", "look": "other look", "voice": "other voice", "regular": false},
           "not an object"],
  "sections": [{"name": "オープニング", "seconds": "60", "plan": "p1"}, {"name": "本編", "seconds": 120, "plan": "p2"},
               {"name": "", "seconds": 30, "plan": "nameless"}, {"name": "特集", "seconds": 60}, {"name": "エンディング", "seconds": 120, "plan": "p4"}]}`
	sectionOf := regexp.MustCompile(`Write section '([^']*)'`)
	fake := newFakeLLM(t, func(user string) (string, bool) {
		m := sectionOf.FindStringSubmatch(user)
		if m == nil {
			return outline, true
		}
		switch m[1] {
		case "オープニング":
			return mustJSON(map[string]any{"segments": []any{
				map[string]any{"caption": " c1 ", "seconds": "8", "prompt": "```\nintegrated multimodal description: [Shot 1] x <d>やあ；こんにちは</d>\n```"},
				map[string]any{"caption": "", "seconds": 20, "prompt": "<d>[ja] " + long60 + "</d>"},
				map[string]any{"caption": "no prompt", "seconds": 8},
			}}), true
		case "本編":
			return mustJSON(map[string]any{"segments": "nothing"}), true
		case "特集":
			return "this is not json at all", true
		case "エンディング":
			return mustJSON(map[string]any{"segments": []any{
				map[string]any{"caption": "さようなら", "seconds": 6.5, "prompt": "<d>[Japanese] またね。</d>"},
			}}), true
		}
		return "", false
	})
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	slot := 4
	prog, err := WriteProgramme(fake.client(), "nature", nil, 5, &slot, logf, rand.New(rand.NewSource(7)))
	if err != nil {
		t.Fatalf("WriteProgramme: %v (logs %v)", err, logs)
	}

	// id / written / slot / minutes / strings coerced
	if !regexp.MustCompile(`^20260917-120000-[0-9a-f]{6}$`).MatchString(prog.ID) {
		t.Errorf("id %q", prog.ID)
	}
	if prog.Written != "2026-09-17T12:00:00" || prog.Format != "nature" || *prog.Slot != 4 || prog.Minutes != 5 {
		t.Errorf("header: %+v", prog)
	}
	if prog.Title != "竹の夜" || prog.Summary != "7" || prog.NextTime != "次回は海へ。" || prog.Theme != "砂漠の夜" {
		t.Errorf("title %q summary %q next %q theme %q", prog.Title, prog.Summary, prog.NextTime, prog.Theme)
	}
	if len(prog.Sources) != 0 || prog.Sources == nil {
		t.Errorf("sources %v", prog.Sources)
	}

	// cast: the three regulars of the last nature episode (中村 千夏, なし, 森 ひなた) are all present; the
	// missing ones were inserted at the front (last inserted first); 中村 千夏 got look/voice/role restored
	// and Regular; "Anchor" became an unused name from CastNames with "regular": "true" read as true
	names := make([]string, len(prog.Cast))
	for i, c := range prog.Cast {
		names[i] = c.Name
	}
	if len(prog.Cast) != 4 || names[0] != "森 ひなた" || names[1] != "なし" || names[3] != "中村 千夏" {
		t.Fatalf("cast order %v", names)
	}
	if !contains(CastNames, names[2]) || names[2] == "中村 千夏" || PlaceholderNames[names[2]] {
		t.Errorf("placeholder not replaced: %q", names[2])
	}
	if c := prog.Cast[2]; !c.Regular || c.Role != "ナレーター" || c.Look != "L" || c.Voice != "V" {
		t.Errorf("Anchor entry: %+v", c)
	}
	if c := prog.Cast[3]; !c.Regular || c.Role != "ナレーター" || c.Look != "The Narrator is an unseen voice; the visuals feature only natural elements." || c.Voice != "Deep, calm, measured Japanese accent" {
		t.Errorf("regular not restored: %+v", c)
	}
	if !prog.Cast[0].Regular || !prog.Cast[1].Regular {
		t.Error("re-inserted regulars keep Regular")
	}

	// sections: seconds scaled 360 -> 300 (60/120/60/120 -> 50/100/50/100); the nameless one dropped; 本編
	// came back empty and 特集 failed: both skipped and logged
	if len(prog.Sections) != 2 || prog.Sections[0].Name != "オープニング" || prog.Sections[1].Name != "エンディング" {
		t.Fatalf("sections: %+v", prog.Sections)
	}
	if prog.Sections[0].Seconds != 50 || prog.Sections[1].Seconds != 100 || *prog.Sections[0].Plan != "p1" || *prog.Sections[1].Plan != "p4" {
		t.Errorf("section seconds/plan: %+v", prog.Sections)
	}
	segs := prog.Sections[0].Segments
	if len(segs) != 2 {
		t.Fatalf("opening segments: %+v", segs)
	}
	if segs[0].Caption != "c1" || segs[0].Seconds != 8 || segs[0].Prompt != "integrated_multimodal_description: [Shot 1] x <d>[Japanese] やあこんにちは</d>" {
		t.Errorf("segment 0: %+v", segs[0])
	}
	if segs[1].Caption != "オープニング" || segs[1].Seconds != 10 || segs[1].Prompt != "<d>[Japanese] "+long60+"</d>" {
		t.Errorf("segment 1: %+v", segs[1])
	}
	if e := prog.Sections[1].Segments; len(e) != 1 || e[0].Seconds != 6 || e[0].Caption != "さようなら" {
		t.Errorf("ending segments: %+v", e)
	}
	if prog.SegmentsTotal != 3 || prog.SecondsTotal != 24 {
		t.Errorf("totals %d / %d", prog.SegmentsTotal, prog.SecondsTotal)
	}

	// logs
	wantOutline := "[aitv] outline: 竹の夜 [砂漠の夜] — オープニング(50s) / 本編(100s) / 特集(50s) / エンディング(100s) — next: 次回は海へ。"
	if len(logs) < 4 || logs[0] != wantOutline {
		t.Errorf("outline log:\n got  %q\n want %q", logs, wantOutline)
	}
	if !contains(logs, "[aitv] section '本編' came back empty; skipped") {
		t.Errorf("empty-section log missing: %v", logs)
	}
	found := false
	for _, l := range logs {
		if strings.HasPrefix(l, "[aitv] section '特集' failed (") && strings.HasSuffix(l, "); skipped") {
			found = true
		}
	}
	if !found {
		t.Errorf("failed-section log missing: %v", logs)
	}
	if last := logs[len(logs)-1]; last != "[aitv] script: 竹の夜: 2 sections, 3 clips, 0.4 min -> scripts/"+prog.ID+".json" {
		t.Errorf("script log %q", last)
	}

	// saved document
	saved, err := Load(prog.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved, prog) {
		t.Errorf("saved programme differs:\n%+v\n%+v", saved, prog)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, prog.ID+".json"))
	if !strings.Contains(string(raw), "\"slot\": 4,") || !strings.Contains(string(raw), "\"theme\": \"砂漠の夜\",") || !strings.Contains(string(raw), "\"regular\": true") {
		t.Errorf("saved bytes:\n%s", raw)
	}

	// what the LLM was asked: outline first, then one request per named section (the failed one retried);
	// the previous caption is passed on; the theme line is absent because the LLM gave no theme
	prompts := fake.prompts()
	if len(prompts) < 5 || sectionOf.MatchString(prompts[0]) {
		t.Fatalf("prompts: %d", len(prompts))
	}
	if !strings.Contains(prompts[0], "砂漠の夜") || !strings.Contains(prompts[0], "中村 千夏") || !strings.Contains(prompts[0], "森 ひなた") {
		t.Errorf("outline prompt lacks the theme or the regulars:\n%s", prompts[0])
	}
	var ending string
	for _, p := range prompts {
		if m := sectionOf.FindStringSubmatch(p); m != nil && m[1] == "エンディング" {
			ending = p
		}
	}
	if !strings.Contains(ending, "The previous clip ended with: オープニング") || strings.Contains(ending, "Today's theme") {
		t.Errorf("ending prompt:\n%s", ending)
	}
	if !strings.Contains(ending, "(100 s)") || !strings.Contains(ending, "次回は海へ。") {
		t.Errorf("ending prompt lacks the scaled seconds or the teaser:\n%s", ending)
	}
}

func TestWriteProgrammeErrors(t *testing.T) {
	useFixtures(t, copyFixtureScripts(t))
	fake := newFakeLLM(t, func(user string) (string, bool) {
		if strings.Contains(user, "Write section '") {
			return mustJSON(map[string]any{"segments": []any{}}), true
		}
		return mustJSON(map[string]any{"title": "x", "sections": []any{map[string]any{"name": "a", "seconds": 60}}}), true
	})
	if _, err := WriteProgramme(fake.client(), "nature", nil, 6, nil, nil, nil); err == nil || err.Error() != "script without any section" {
		t.Errorf("all sections empty: %v", err)
	}
	if _, err := WriteProgramme(fake.client(), "no-such-format", nil, 6, nil, nil, nil); err == nil {
		t.Error("unknown format must fail")
	}
	fake2 := newFakeLLM(t, func(string) (string, bool) { return mustJSON(map[string]any{"title": "x", "sections": []any{}}), true })
	if _, err := WriteProgramme(fake2.client(), "nature", nil, 6, nil, nil, nil); err == nil || err.Error() != "outline without sections" {
		t.Errorf("no sections: %v", err)
	}
	fake3 := newFakeLLM(t, func(string) (string, bool) { return "", false })
	if _, err := WriteProgramme(fake3.client(), "nature", nil, 6, nil, nil, nil); err == nil {
		t.Error("an LLM failure on the outline must fail")
	}
	if _, err := WriteProgramme(nil, "nature", nil, 6, nil, nil, nil); err == nil {
		t.Error("nil client must fail")
	}
	// values of the wrong shape are tolerated: a string for "cast" is no cast, a string for "segments" an
	// empty section, a non-object section reply likewise
	dir := t.TempDir()
	useFixtures(t, dir)
	chat := &stubChat{outline: `{"title": "T", "cast": "xy", "sections": [{"name": "a", "seconds": 60}, {"name": "b", "seconds": 60}]}`,
		section: `{"segments": "nothing"}`}
	var logs []string
	_, err := writeProgramme(chat.chat, "nature", nil, 6, nil, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }, nil)
	if err == nil || err.Error() != "script without any section" || !contains(logs, "[aitv] section 'a' came back empty; skipped") || len(chat.prompts) != 3 {
		t.Errorf("wrong-shape values: err=%v logs=%q calls=%d", err, logs, len(chat.prompts))
	}
	chat = &stubChat{outline: `{"title": "T", "cast": {"k": "v"}, "sections": [{"name": "a", "seconds": 60}]}`, section: `"just text"`}
	if _, err := writeProgramme(chat.chat, "nature", nil, 6, nil, logx.Discard, nil); err == nil || err.Error() != "script without any section" {
		t.Errorf("non-object section reply: %v", err)
	}
	// a non-object outline reply is an error; nothing is saved
	chat = &stubChat{outline: `[1, 2]`, section: goodSection}
	if _, err := writeProgramme(chat.chat, "nature", nil, 1, nil, logx.Discard, nil); err == nil || err.Error() != "outline is not a JSON object" {
		t.Errorf("outline list: err = %v", err)
	}
	if files := scriptFiles(t, dir); len(files) != 0 {
		t.Errorf("a script was saved: %v", files)
	}
}

func TestWriteBreak(t *testing.T) {
	dir := copyFixtureScripts(t)
	useFixtures(t, dir)
	pinClock(t, time.Date(2026, 9, 17, 12, 0, 1, 0, time.Local))
	fake := newFakeLLM(t, func(user string) (string, bool) {
		return mustJSON(map[string]any{"title": " 新しいCM ", "segments": []any{
			map[string]any{"caption": "c", "seconds": 9, "prompt": "<d>やあ</d>"},
			map[string]any{"caption": "second", "seconds": 6, "prompt": "x"},
		}}), true
	})
	prog, err := WriteBreak(fake.client(), "cm")
	if err != nil {
		t.Fatal(err)
	}
	if prog.Format != "cm" || prog.Slot != nil || prog.Title != "新しいCM" || prog.Minutes != 0.1 || prog.Written != "2026-09-17T12:00:01" || !prog.IsBreak() {
		t.Errorf("header: %+v", prog)
	}
	if len(prog.Sections) != 1 || prog.Sections[0].Name != "CM" || *prog.Sections[0].Plan != "" || prog.Sections[0].Seconds != 6 {
		t.Errorf("section: %+v", prog.Sections)
	}
	if s := prog.Sections[0].Segments; len(s) != 1 || s[0].Seconds != 6 || s[0].Caption != "c" || s[0].Prompt != "<d>[Japanese] やあ</d>" {
		t.Errorf("segments: %+v", s)
	}
	if prog.SegmentsTotal != 1 || prog.SecondsTotal != 6 || prog.Theme != "" || prog.NextTime != "" {
		t.Errorf("totals: %+v", prog)
	}
	raw, err := os.ReadFile(filepath.Join(dir, prog.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\"slot\": null,", "\"cast\": [],", "\"sources\": [],", "\"minutes\": 0.1,", "\"plan\": \"\","} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved break lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "theme") || strings.Contains(string(raw), "summary") || strings.Contains(string(raw), "next_time") {
		t.Errorf("a break has no theme / summary / next_time:\n%s", raw)
	}
	// the recent titles (History("cm", 10)) were passed to the prompt
	if p := fake.prompts(); len(p) != 1 || !strings.Contains(p[0], "フレッシュブリーズミスト") || !strings.Contains(p[0], "6-second") {
		t.Errorf("break prompt: %v", p)
	}
	// ident: 4 s and a non-integral minutes
	ident, err := WriteBreak(fake.client(), "ident")
	if err != nil || ident.Sections[0].Seconds != 4 || ident.Sections[0].Segments[0].Seconds != 4 || ident.Sections[0].Name != "ステーションID" || ident.Minutes != 4.0/60 {
		t.Errorf("ident: %+v (%v)", ident, err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, ident.ID+".json"))
	if !strings.Contains(string(raw), "\"minutes\": 0.06666666666666667,") {
		t.Errorf("ident minutes:\n%s", raw)
	}
	// no segment, a non-object reply, an odd title
	empty := newFakeLLM(t, func(string) (string, bool) { return mustJSON(map[string]any{"title": "x", "segments": []any{}}), true })
	if _, err := WriteBreak(empty.client(), "cm"); err == nil || err.Error() != "no segment for cm" {
		t.Errorf("no segment: %v", err)
	}
	if _, err := writeBreak((&stubChat{outline: `42`}).chat, "cm"); err == nil || err.Error() != "no segment for cm" {
		t.Errorf("non-object reply: %v", err)
	}
	if brk, err := writeBreak((&stubChat{outline: `{"title": 1.5, "segments": [{"caption": "c", "seconds": 6, "prompt": "x"}]}`}).chat, "cm"); err != nil || brk.Title != "1.5" {
		t.Errorf("number title: %+v %v", brk, err)
	}
	if _, err := WriteBreak(nil, "cm"); err == nil {
		t.Error("nil client must fail")
	}
}

// TestNewsReported checks the "already reported" list: sources of news episodes written within 36 h,
// deduplicated in order, the last 40.
func TestNewsReported(t *testing.T) {
	dir := t.TempDir()
	useFixtures(t, dir)
	pinClock(t, time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local))
	// three news scripts: one too old, two recent with overlapping sources; all "rendered" via the index
	var items []index.Item
	mk := func(id, written string, sources []string) {
		p := &Programme{ID: id, Format: "news", Title: "t " + id, Minutes: 6, Written: written, Sources: sources,
			Sections: []Section{{Name: "s", Segments: []Segment{{Caption: "c", Seconds: 8, Prompt: "x"}}}}, SegmentsTotal: 1, SecondsTotal: 8}
		if _, err := Save(p); err != nil {
			t.Fatal(err)
		}
		items = append(items, index.Item{ID: id + "-0", ProgrammeID: id})
	}
	mk("20260915-100000-aaaaaa", "2026-09-15T10:00:00", []string{"old story"})
	var many []string
	for i := range 45 {
		many = append(many, fmt.Sprintf("story %02d", i))
	}
	mk("20260916-100000-bbbbbb", "2026-09-16T10:00:00", append(many, "story 44"))
	mk("20260917-080000-cccccc", "2026-09-17T08:00:00", []string{"story 44", "fresh story"})
	loadIndex = func() []index.Item { return items }

	var outlinePrompt string
	fake := newFakeLLM(t, func(user string) (string, bool) {
		if !strings.Contains(user, "Write section '") {
			outlinePrompt = user
		}
		return mustJSON(map[string]any{"title": "x", "sections": []any{map[string]any{"name": "a", "seconds": 60}},
			"segments": []any{map[string]any{"caption": "c", "seconds": 8, "prompt": "x"}}}), true
	})
	if _, err := WriteProgramme(fake.client(), "news", nil, 6, nil, logx.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outlinePrompt, "old story") {
		t.Error("a story older than 36 h must not be listed")
	}
	if !strings.Contains(outlinePrompt, "fresh story") || !strings.Contains(outlinePrompt, "story 44") {
		t.Error("recent stories must be listed")
	}
	// 45 + 1 unique stories, last 40 kept: "story 00".."story 05" fall off
	if strings.Contains(outlinePrompt, "story 05") || !strings.Contains(outlinePrompt, "story 06") {
		t.Errorf("last 40 rule:\n%s", outlinePrompt)
	}
	if strings.Count(outlinePrompt, "story 44") != 1 {
		t.Error("deduplicated (within and across episodes)")
	}
}

// The saved document has the shape write_programme's dict had: theme / summary / next_time present even
// when empty, minutes as a Python float, a missing section plan as null, the cast with its five fields.
func TestSavedProgrammeShape(t *testing.T) {
	useFixtures(t, t.TempDir())
	pinClock(t, time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local))
	chat := &stubChat{outline: `{"title": "", "theme": " 深海 ", "summary": "", "next_time": " 次回は海へ。 ", "cast": [{"name": "佐藤 美咲"}], "sections": [{"name": "a", "seconds": 60}, {"name": "b", "seconds": 60, "plan": null}]}`,
		section: `{"segments": [{"caption": "c", "seconds": 8, "prompt": "integrated_multimodal_description: x"}]}`}
	prog, err := writeProgramme(chat.chat, "nature", nil, 6, nil, logx.Discard, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(scriptsDir(), prog.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Replace(strings.TrimSpace(string(data)), prog.ID, "ID", 1)
	want := `{
 "id": "ID",
 "format": "nature",
 "slot": null,
 "title": "自然ドキュメンタリー",
 "theme": "深海",
 "summary": "",
 "next_time": "次回は海へ。",
 "cast": [
  {
   "role": "",
   "name": "佐藤 美咲",
   "look": "",
   "voice": "",
   "regular": false
  }
 ],
 "minutes": 6.0,
 "sources": [],
 "written": "2026-09-17T12:00:00",
 "sections": [
  {
   "name": "a",
   "plan": null,
   "seconds": 180,
   "segments": [
    {
     "caption": "c",
     "seconds": 8,
     "prompt": "integrated_multimodal_description: x"
    }
   ]
  },
  {
   "name": "b",
   "plan": null,
   "seconds": 180,
   "segments": [
    {
     "caption": "c",
     "seconds": 8,
     "prompt": "integrated_multimodal_description: x"
    }
   ]
  }
 ],
 "segments_total": 2,
 "seconds_total": 16
}`
	if got != want {
		t.Errorf("saved document\n got  %s\n want %s", got, want)
	}
	// the section prompts saw the outline as the LLM gave it: no title fallback, the theme and the teaser
	// unstripped, a missing plan empty
	if len(chat.prompts) != 3 {
		t.Fatalf("%d prompts", len(chat.prompts))
	}
	for _, want := range []string{"Programme:  (自然ドキュメンタリー, nature).", "\nToday's theme:  深海 \n", "\n- : 佐藤 美咲 —  Voice: \n", "\nWrite section 'a' (180 s): \n"} {
		if !strings.Contains(chat.prompts[1], want) {
			t.Errorf("section prompt lacks %q:\n%s", want, chat.prompts[1])
		}
	}
	if want := "spoken by a regular:  次回は海へ。 \n"; !strings.Contains(chat.prompts[2], want) {
		t.Errorf("closing section prompt lacks %q:\n%s", want, chat.prompts[2])
	}
}

// A regular read back from a stored file is carried into the next episode: re-inserted when the LLM
// dropped it, restored (look / voice / role, Regular) when the LLM rewrote it.
func TestRegularsCarried(t *testing.T) {
	dir := t.TempDir()
	useFixtures(t, dir)
	pinClock(t, time.Date(2026, 9, 17, 12, 0, 0, 0, time.Local))
	first := &Programme{ID: "20260917-110000-aaaaaa", Format: "nature", Title: "T0", Minutes: 6, Written: "2026-09-17T11:00:00",
		Cast: []programs.Cast{{Role: "narrator", Name: "佐藤 美咲", Look: "L", Voice: "V", Regular: true},
			{Role: "guest", Name: "高橋 悠真", Look: "L2", Voice: "V2"},
			{Role: "host", Name: "池田 航", Look: "L3", Voice: "V3", Regular: true}},
		Sections:      []Section{{Name: "a", Plan: new("p"), Seconds: 180, Segments: []Segment{{Caption: "c", Seconds: 8, Prompt: "x"}}}},
		SegmentsTotal: 1, SecondsTotal: 8}
	if _, err := Save(first); err != nil {
		t.Fatal(err)
	}
	loadIndex = func() []index.Item { return []index.Item{{ProgrammeID: first.ID}} }
	chat := &stubChat{outline: `{"title": "T", "theme": "th", "summary": "s", "next_time": "",
  "cast": [{"role": "guest", "name": "池田 航", "look": "other", "voice": "", "regular": false}],
  "sections": [{"name": "a", "seconds": 60, "plan": "p"}]}`, section: goodSection}
	prog, err := writeProgramme(chat.chat, "nature", nil, 6, nil, logx.Discard, nil)
	if err != nil {
		t.Fatalf("writeProgramme: %v", err)
	}
	want := []programs.Cast{{Role: "narrator", Name: "佐藤 美咲", Look: "L", Voice: "V", Regular: true},
		{Role: "host", Name: "池田 航", Look: "L3", Voice: "V3", Regular: true}}
	if !reflect.DeepEqual(prog.Cast, want) {
		t.Errorf("cast\n got  %+v\n want %+v", prog.Cast, want)
	}
	wantRegulars := "Regular cast — this programme's fixed people. Keep their names, looks and voices EXACTLY as written; add today's guests or acts as new cast entries with \"regular\": false:\n" +
		"- narrator: 佐藤 美咲 — L Voice: V\n- host: 池田 航 — L3 Voice: V3\n\n"
	if !strings.Contains(chat.prompts[0], wantRegulars) {
		t.Errorf("outline prompt regulars:\n%s", chat.prompts[0])
	}
	wantBlock := "Cast (repeat the look and voice verbatim whenever they appear; give each speaker a stable Sx number):\n" +
		"- narrator: 佐藤 美咲 — L Voice: V\n- host: 池田 航 — L3 Voice: V3\nSections in order"
	if !strings.Contains(chat.prompts[1], wantBlock) {
		t.Errorf("section prompt cast:\n%s", chat.prompts[1])
	}
}
