package programs

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// now is the fixed clock every golden was captured with: datetime(2026, 9, 17, 8, 30), a Thursday morning.
var now = time.Date(2026, 9, 17, 8, 30, 0, 0, time.Local)

// --- inputs, mirroring the Python dicts in the golden generator exactly ---

var (
	anchor = Cast{Role: "アンカー", Name: "高橋 由紀",
		Look:  "A woman in her 40s with shoulder-length black hair, navy blazer, at a modern glass news desk.",
		Voice: "calm, clear alto", Regular: true}
	weatherman = Cast{Role: "気象キャスター", Name: "森 拓也",
		Look:  "A man in his 30s with short hair and a light grey suit, standing beside a stylised weather map.",
		Voice: "bright tenor", Regular: true}
	chef = Cast{Role: "シェフ", Name: "田中 みどり",
		Look:  "A cheerful woman in her 50s with a white apron and a red bandana, in a bright kitchen.",
		Voice: "warm, lively mezzo", Regular: true}
	guest = Cast{Role: "ゲスト", Name: "佐藤 健二", Look: "A man in his 20s with glasses and a blue apron.",
		Voice: "soft baritone"}

	recentNews = []Episode{
		{Written: "2026-09-16T12:05:44", Title: "9月16日(水) 昼のニュース", Theme: "台風と経済",
			Summary: "台風14号の接近と円相場の動きを伝えた。", NextTime: ""},
		{Written: "2026-09-16T19:30:01", Title: "9月16日(水) 夜のニュース", Theme: "", Summary: "",
			NextTime: "あすは週末の天気を詳しく。"},
	}
	reported = []string{"台風14号が九州に接近", "円相場が一時150円台に"}
	material = "## 主要ニュース\n- 台風14号が九州に接近、あす上陸の恐れ\n- 円相場が一時150円台\n\n" +
		"## スポーツ\n- サッカー日本代表が親善試合で勝利"
	themes3     = []string{"シティポップ", "ジャズトリオ", "演歌風バラード"}
	recentMusic = []Episode{
		{Written: "2026-09-15T20:10:00", Title: "ネオン・ステージ vol.3", Theme: "ロック / R&B / アンビエント",
			Summary: "3組が熱演。", NextTime: "次回はしっとりジャズ。"},
		// Python: theme None -> "-", summary None -> "", next_time None -> no suffix; written[:10] counts runes.
		{Written: "昨日の放送・その前の放送", Title: "ネオン・ステージ vol.4"},
	}

	outlineCook = Outline{
		Title: "3分クッキング きょうは鮭の南蛮漬け", Theme: "魚料理", Summary: "鮭の南蛮漬けを作った。",
		NextTime: "次回は秋野菜のスープをご紹介します。",
		Cast:     []Cast{chef, guest},
		Sections: []OutlineSection{
			{Name: "オープニング", Seconds: 36, Plan: "The chef greets the viewer and shows the salmon."},
			{Name: "調理", Seconds: 0, Plan: "Fry the salmon; sizzling close-ups."},
			{Name: "エンディング", Seconds: 44, Plan: "Plating, tasting, closing tip and the teaser."},
		},
	}
	outlineNews = Outline{
		Title: "9月17日(木) 朝のニュース",
		Cast:  []Cast{anchor},
		Sections: []OutlineSection{
			{Name: "オープニング", Seconds: 20, Plan: "The anchor states the date and the edition."},
			{Name: "主要ニュース", Seconds: 120, Plan: "Typhoon and the yen, over symbolic scenes."},
			{Name: "エンディング", Seconds: 20, Plan: "Sign-off repeating the date."},
		},
	}
)

// expectPair checks a (system, user) pair against a golden byte for byte and reports the first difference.
func expectPair(t *testing.T, name, system, user string) {
	t.Helper()
	g, ok := goldenPrompts[name]
	if !ok {
		t.Fatalf("no golden %q", name)
	}
	expectString(t, name+" system", g[0], system)
	expectString(t, name+" user", g[1], user)
}

func expectString(t *testing.T, what, want, got string) {
	t.Helper()
	if want == got {
		return
	}
	i := 0
	for i < len(want) && i < len(got) && want[i] == got[i] {
		i++
	}
	lo := i - 60
	if lo < 0 {
		lo = 0
	}
	hiW, hiG := i+80, i+80
	if hiW > len(want) {
		hiW = len(want)
	}
	if hiG > len(got) {
		hiG = len(got)
	}
	t.Errorf("%s differs at byte %d (want %d bytes, got %d)\n want: %q\n  got: %q", what, i, len(want), len(got),
		want[lo:hiW], got[lo:hiG])
}

// --- tables ---

type tablesFixture struct {
	Station Station `json:"station"`
	Rules   string  `json:"h3_format_rules"`
	Formats map[string]struct {
		Title         string  `json:"title"`
		Minutes       float64 `json:"minutes"`
		NeedsMaterial bool    `json:"needs_material"`
		MinGapHours   float64 `json:"min_gap_hours"`
		Brief         string  `json:"brief"`
	} `json:"formats"`
	Lineup       []string            `json:"lineup"`
	BreakFormats []string            `json:"break_formats"`
	SegmentMinS  int                 `json:"segment_min_s"`
	SegmentMaxS  int                 `json:"segment_max_s"`
	Themes       map[string][]string `json:"themes"`
	ThemeCount   map[string]int      `json:"theme_count"`
	ThemeKeys    map[string][]string `json:"theme_keys"`
	WeekdaysJA   string              `json:"weekdays_ja"`
	System       string              `json:"system"`
}

// TestTablesMatchPython compares every table with testdata/tables.json, dumped from aitv/programs.py.
func TestTablesMatchPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/tables.json")
	if err != nil {
		t.Fatal(err)
	}
	var want tablesFixture
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if STATION != want.Station {
		t.Errorf("STATION = %+v, want %+v", STATION, want.Station)
	}
	expectString(t, "H3FormatRules", want.Rules, H3FormatRules)
	if strings.Count(H3FormatRules, "`") != 4 {
		t.Errorf("H3FormatRules should contain 4 backticks, has %d", strings.Count(H3FormatRules, "`"))
	}
	if len(Formats) != len(want.Formats) || len(Formats) != 9 {
		t.Errorf("Formats has %d entries, want %d", len(Formats), len(want.Formats))
	}
	for key, wf := range want.Formats {
		f, ok := Formats[key]
		if !ok {
			t.Errorf("Formats[%q] missing", key)
			continue
		}
		if f.Key != key {
			t.Errorf("Formats[%q].Key = %q", key, f.Key)
		}
		if f.Title != wf.Title || f.Minutes != wf.Minutes || f.NeedsMaterial != wf.NeedsMaterial || f.MinGapHours != wf.MinGapHours {
			t.Errorf("Formats[%q] = %q/%v/%v/%v, want %q/%v/%v/%v", key, f.Title, f.Minutes, f.NeedsMaterial, f.MinGapHours,
				wf.Title, wf.Minutes, wf.NeedsMaterial, wf.MinGapHours)
		}
		expectString(t, "Formats["+key+"].Brief", wf.Brief, f.Brief)
	}
	if !reflect.DeepEqual(Lineup, want.Lineup) {
		t.Errorf("Lineup = %v, want %v", Lineup, want.Lineup)
	}
	if !reflect.DeepEqual(BreakFormats, want.BreakFormats) {
		t.Errorf("BreakFormats = %v, want %v", BreakFormats, want.BreakFormats)
	}
	if SegmentMinS != want.SegmentMinS || SegmentMaxS != want.SegmentMaxS {
		t.Errorf("segment bounds = %d-%d, want %d-%d", SegmentMinS, SegmentMaxS, want.SegmentMinS, want.SegmentMaxS)
	}
	if !reflect.DeepEqual(Themes, want.Themes) {
		t.Errorf("Themes differ from Python:\n got %v\nwant %v", Themes, want.Themes)
	}
	if !reflect.DeepEqual(ThemeCount, want.ThemeCount) {
		t.Errorf("ThemeCount = %v, want %v", ThemeCount, want.ThemeCount)
	}
	if !reflect.DeepEqual(ThemeKeys, want.ThemeKeys) {
		t.Errorf("ThemeKeys differ from Python:\n got %v\nwant %v", ThemeKeys, want.ThemeKeys)
	}
	if WeekdaysJA != want.WeekdaysJA {
		t.Errorf("WeekdaysJA = %q, want %q", WeekdaysJA, want.WeekdaysJA)
	}
	expectString(t, "systemPrompt", want.System, systemPrompt())
}

func TestThemeTablesConsistent(t *testing.T) {
	// every rotating format exists; every ThemeKeys entry names a theme of some format (cooking and variety
	// themes have no entry in Python either: they match on their own name)
	all := map[string]bool{}
	for fmtKey, list := range Themes {
		if _, ok := Formats[fmtKey]; !ok {
			t.Errorf("Themes[%q] has no format", fmtKey)
		}
		for _, theme := range list {
			all[theme] = true
		}
	}
	for theme, keys := range ThemeKeys {
		if !all[theme] {
			t.Errorf("ThemeKeys[%q] is not a theme of any format", theme)
		}
		if len(keys) == 0 {
			t.Errorf("ThemeKeys[%q] is empty", theme)
		}
	}
	for _, theme := range append(append([]string{}, Themes["music_show"]...), append(Themes["nature"], Themes["travel"]...)...) {
		if _, ok := ThemeKeys[theme]; !ok {
			t.Errorf("ThemeKeys lacks %q", theme)
		}
	}
	for _, key := range append(append([]string{}, Lineup...), BreakFormats...) {
		if _, ok := Formats[key]; !ok {
			t.Errorf("Lineup/BreakFormats key %q has no format", key)
		}
	}
	if ThemeCount["music_show"] != 3 || ThemeCount["nature"] != 0 {
		t.Errorf("ThemeCount = %v", ThemeCount)
	}
}

// --- Edition / DateLine ---

func TestEdition(t *testing.T) {
	cases := map[int]string{0: "朝", 5: "朝", 10: "朝", 11: "昼", 14: "昼", 15: "夕方", 18: "夕方", 19: "夜", 23: "夜"}
	for h, want := range cases {
		if got := Edition(time.Date(2026, 9, 17, h, 59, 0, 0, time.Local)); got != want {
			t.Errorf("Edition(%02d:59) = %q, want %q", h, got, want)
		}
	}
}

func TestDateLine(t *testing.T) {
	for stamp, want := range goldenDateLines {
		at, err := time.ParseInLocation("2006-01-02T15:04:05", stamp, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		if got := DateLine(at); got != want {
			t.Errorf("DateLine(%s) = %q, want %q", stamp, got, want)
		}
	}
	if got := DateLine(now); got != "2026-09-17 08:30 = 9月17日 木曜日 朝" {
		t.Errorf("DateLine(now) = %q", got)
	}
	// zero time means the wall clock
	if got, want := DateLine(time.Time{})[:10], time.Now().Format("2006-01-02"); got != want {
		t.Errorf("DateLine(zero) starts %q, want today %q", got, want)
	}
}

// --- helpers ---

func TestFormatG(t *testing.T) {
	// values checked against Python format(x, 'g')
	cases := map[float64]string{6: "6", 4: "4", 5: "5", 0.1: "0.1", 0.07: "0.07", 4.5: "4.5", 6.123456789: "6.12346", 1e6: "1e+06", 100000: "100000"}
	for x, want := range cases {
		if got := formatG(x); got != want {
			t.Errorf("formatG(%v) = %q, want %q", x, got, want)
		}
	}
}

func TestSegmentCount(t *testing.T) {
	for secs, want := range goldenSegmentCounts {
		if got := segmentCount(secs); got != want {
			t.Errorf("segmentCount(%d) = %d, want %d (Python round())", secs, got, want)
		}
	}
}

func TestFirstRunes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-09-16T12:05:44", "2026-09-16"},
		{"昨日の放送・その前の放送", "昨日の放送・その前の"},
		{"short", "short"},
		{"", ""},
	}
	for _, c := range cases {
		if got := firstRunes(c.in, 10); got != c.want {
			t.Errorf("firstRunes(%q, 10) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- prompts (byte-exact against the Python output) ---

func TestOutlinePromptParity(t *testing.T) {
	sys, user := OutlinePrompt("news", OutlineInput{Minutes: 6, Material: material, Now: now, Regulars: []Cast{anchor, weatherman},
		Recent: recentNews, Reported: reported})
	expectPair(t, "outline_news", sys, user)

	sys, user = OutlinePrompt("music_show", OutlineInput{Minutes: 6, Now: now, Recent: recentMusic, Themes: themes3})
	expectPair(t, "outline_music", sys, user)

	sys, user = OutlinePrompt("nature", OutlineInput{Minutes: 6, Now: now})
	expectPair(t, "outline_nature", sys, user)

	sys, user = OutlinePrompt("weather", OutlineInput{Minutes: 4.5, Material: material, Now: now, Regulars: []Cast{weatherman}})
	expectPair(t, "outline_weather", sys, user)

	sys, user = OutlinePrompt("cm", OutlineInput{Minutes: 0.1, Now: now})
	expectPair(t, "outline_cm", sys, user)

	sys, user = OutlinePrompt("ident", OutlineInput{Minutes: 0.07, Now: now})
	expectPair(t, "outline_ident", sys, user)

	// empty (non-nil) slices behave like None, and a single theme gets no "three acts" suffix
	sys, user = OutlinePrompt("travel", OutlineInput{Minutes: 6, Now: now, Themes: []string{"港町"}, Regulars: []Cast{},
		Recent: []Episode{}, Reported: []string{}})
	expectPair(t, "outline_travel_theme1", sys, user)
}

func TestOutlinePromptTotals(t *testing.T) {
	// int(minutes * 60) in Python truncates: 0.1 min -> 6, 0.07 min -> 4, 4.5 min -> 270
	for minutes, want := range map[float64]string{6: "about 360.", 0.1: "about 6.", 0.07: "about 4.", 4.5: "about 270."} {
		_, user := OutlinePrompt("nature", OutlineInput{Minutes: minutes, Now: now})
		if !strings.Contains(user, "the seconds must add up to "+want) {
			t.Errorf("minutes %v: total not %q in %q", minutes, want, user[len(user)-200:])
		}
	}
}

func TestSectionPromptParity(t *testing.T) {
	sys, user := SectionPrompt("cooking", outlineCook, outlineCook.Sections[1], "鮭に小麦粉をまぶします", "", now)
	expectPair(t, "section_cook_mid", sys, user)

	sys, user = SectionPrompt("cooking", outlineCook, outlineCook.Sections[2], "皿に盛りつけます", "", now)
	expectPair(t, "section_cook_last", sys, user)

	sys, user = SectionPrompt("cooking", outlineCook, outlineCook.Sections[0], "", "", now)
	expectPair(t, "section_cook_first_36", sys, user)

	sys, user = SectionPrompt("news", outlineNews, outlineNews.Sections[0], "", material, now)
	expectPair(t, "section_news_first_material", sys, user)

	sys, user = SectionPrompt("news", outlineNews, outlineNews.Sections[2], "以上、主要ニュースでした", material, now)
	expectPair(t, "section_news_last_no_teaser", sys, user)
}

func TestSectionPromptRules(t *testing.T) {
	// Seconds 0 falls back to 60 s and 8 segments; the closing teaser appears only on the last section
	_, user := SectionPrompt("cooking", outlineCook, outlineCook.Sections[1], "x", "", now)
	if !strings.Contains(user, "Write section '調理' (60 s)") || !strings.Contains(user, "with about 8 segments whose seconds add up to about 60.") {
		t.Errorf("60 s fallback missing: %q", user)
	}
	if strings.Contains(user, "This is the closing section") {
		t.Errorf("teaser on a middle section")
	}
	_, user = SectionPrompt("cooking", outlineCook, outlineCook.Sections[2], "x", "", now)
	if !strings.Contains(user, "This is the closing section: its last clip ends with this teaser, spoken by a regular: "+outlineCook.NextTime+"\n") {
		t.Errorf("teaser missing on the last section: %q", user)
	}
	// no sections at all: nothing is "last"
	bare := Outline{Title: "t", NextTime: "teaser"}
	_, user = SectionPrompt("nature", bare, OutlineSection{Name: "x", Seconds: 8, Plan: "p"}, "", "", now)
	if strings.Contains(user, "closing section") || !strings.Contains(user, "Sections in order: \n") {
		t.Errorf("empty outline handled wrong: %q", user)
	}
	if !strings.Contains(user, "This is the first clip of the programme.\n") {
		t.Errorf("first-clip line missing: %q", user)
	}
}

func TestBreakPromptParity(t *testing.T) {
	sys, user := BreakPrompt("cm", []string{"ドリンクCM「そらのしずく」", "ガジェットCM「ポケットライト」"})
	expectPair(t, "break_cm_recent", sys, user)

	sys, user = BreakPrompt("ident", nil)
	expectPair(t, "break_ident_none", sys, user)

	sys, user = BreakPrompt("cm", []string{})
	expectPair(t, "break_cm_empty", sys, user)
}

func TestZeroNowUsesWallClock(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	_, user := OutlinePrompt("nature", OutlineInput{Minutes: 6})
	if !strings.Contains(user, "Now: "+today) {
		t.Errorf("OutlinePrompt zero Now: %q", user[:120])
	}
	_, user = SectionPrompt("nature", outlineCook, outlineCook.Sections[0], "", "", time.Time{})
	if !strings.Contains(user, "\nNow: "+today) {
		t.Errorf("SectionPrompt zero now: %q", user[:400])
	}
}

func TestUnknownFormatPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("no panic for an unknown format key")
		}
	}()
	BreakPrompt("bogus", nil)
}
