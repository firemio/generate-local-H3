// Package programs holds the station identity, the programme formats, the daily lineup, the theme rotation
// tables and the writer prompts (port of aitv/programs.py).
//
// A programme is written in two passes by the local LLM: an outline (title, regular cast, sections with a
// time budget) and then, section by section, the H3 prompts for each 6-10 second clip
// (integrated_multimodal_description / overall_soundscape / non_diegetic_music, Japanese dialogue in
// <d>[Japanese] ...</d> tags). The cast descriptions from the outline are repeated verbatim in every clip so
// the same host looks and sounds the same across the whole programme.
//
// The prompt strings produced here are fed to the LLM and therefore shape every programme: they are kept
// identical to what aitv/programs.py produced (outline_prompt / section_prompt / break_prompt / _system /
// date_line), and the golden tests pin them. Formatting notes: `{minutes:g}` is formatG ("6", "0.1", "4.5"),
// the written date is the first 10 runes of Episode.Written, `round(secs / 8)` rounds half to even.
package programs

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Station is the channel identity (served as "station" in /playlist.json).
type Station struct {
	Name    string `json:"name"`
	Tagline string `json:"tagline"`
	Lang    string `json:"lang"`
}

// STATION is the channel: EVO-X2 TV / ローカルAIが24時間つくり続ける放送局 / Japanese.
var STATION = Station{Name: "EVO-X2 TV", Tagline: "ローカルAIが24時間つくり続ける放送局", Lang: "Japanese"}

// Format is one programme format (an entry of aitv/programs.py FORMATS).
type Format struct {
	Key   string
	Title string
	// Minutes is the format's own length (news 6, weather 4, variety 6, music_show 6, nature 6, cooking 5,
	// travel 6, cm 0.1, ident 0.07).
	Minutes float64
	// NeedsMaterial: the writer refuses to invent content for this format (news, weather, variety).
	NeedsMaterial bool
	// MinGapHours: two programmes of this format closer than this would read the same material (news: 5).
	MinGapHours float64
	Brief       string
}

// Lineup is one broadcast day: eight programmes, produced in this order and aired in this order.
var Lineup = []string{"news", "weather", "variety", "music_show", "nature", "cooking", "travel", "news"}

// BreakFormats alternate after each programme.
var BreakFormats = []string{"cm", "ident"}

// Clip length bounds in seconds.
const (
	SegmentMinS = 6
	SegmentMaxS = 10
)

// ThemeCount is how many themes to suggest at once (music_show: 3; every other format 1).
var ThemeCount = map[string]int{"music_show": 3}

// WeekdaysJA indexes Monday..Sunday.
const WeekdaysJA = "月火水木金土日"

// The four tables below (H3FormatRules, Formats, Themes, ThemeKeys) were emitted mechanically from the Python
// module, not retyped; TestTablesMatchPython compares them with testdata/tables.json from the same Python.

// H3FormatRules is the H3 prompt-structure instruction shared by every writer prompt (verbatim from Python).
const H3FormatRules = `You write prompts for MiniMax H3, a video+audio generation model. Follow this exact structure for every prompt:

integrated_multimodal_description: [Shot 1] <style words>, <shot size> frames <subject and setting>. <camera motion written as 'The camera pushes in with small amplitude at slow speed'>. <who speaks (Sx) and what they say>. [Shot 2] At 00:0X.000, the camera cuts to ... (optional, timestamps must be inside the clip length)
overall_soundscape: 1-3 sentences of ambient and action sounds only (no dialogue, no music).
non_diegetic_music: 1-2 sentences describing background score in instrumental terms, or N/A.

Dialogue rules: EVERY <d> tag MUST start with the language tag exactly like <d>[Japanese] ...</d> (never omit it). On-screen speech is written as ` + "`" + `The <person description> with a <voice description> (S1) says: <d>[Japanese] 日本語のセリフ。</d>` + "`" + `. Narration is ` + "`" + `... says in an off-screen voiceover: <d>[Japanese] ...</d>` + "`" + ` and note that lips stay closed. Speech takes about 6 Japanese characters per second: a 10-second clip holds at most ~50 characters, an 8-second clip ~40. Never put text, captions, logos or subtitles on screen. No real people, brands or copyrighted characters.
Camera vocabulary: Zoom In/Out, Push In/Out, Pan Left/Right, Truck Left/Right, Tilt Up/Down, Arc Shot, Tracking Shot, Static Shot, with small/large amplitude, at slow/fast speed.`

// Formats are the nine formats keyed by Key (news, weather, variety, music_show, nature, cooking, travel, cm, ident),
// with the briefs copied verbatim from aitv/programs.py.
var Formats = map[string]Format{
	"news": {
		Key:           "news",
		Title:         "ニュース",
		Minutes:       6,
		NeedsMaterial: true,
		MinGapHours:   5,
		Brief:         `A Japanese TV news programme built ONLY from the real headlines provided. One anchor at a modern news desk carries the whole programme; sections follow the material's categories (top stories, society, economy, world, sports, science & culture) and end with a short sign-off. The viewer must always know WHEN this is: the title starts with the date and the edition (pattern: '10月3日(土) 昼のニュース'), the anchor's very first sentence states the date, the day of the week and the edition exactly as given in the plan line above (pattern: '10月3日、土曜日、昼のニュースです' — use the real date and weekday, never the pattern's), stories are placed in time ('きょう午前', 'きのう'), and the sign-off repeats the date. Each story: the anchor reads it in one or two sentences, or a voiceover reads it over a generic symbolic scene that fits the subject (a city skyline, a stadium, a laboratory, a harbour, a field, a trading floor...). Condense; never add facts, names or numbers that are not in the material. Never show real people, real logos, flags or recognisable landmarks. The material comes from a news agency but this channel is not that agency: never put its name in the title or the speech.`,
	},
	"weather": {
		Key:           "weather",
		Title:         "お天気",
		Minutes:       4,
		NeedsMaterial: true,
		Brief:         `A friendly Japanese weather programme built ONLY from the real JMA forecast provided: today, tonight, tomorrow, the week ahead, temperatures and rain chance for the named area, plus practical advice (umbrella, heat, clothing). The title starts with the date (pattern: '10月3日(土) お天気'), and the presenter opens by saying the date, the day of the week and the time of day exactly as given in the plan line above (use the real date and weekday), then names each day explicitly ('きょう14日', 'あす15日'). One presenter in front of a large stylised weather map, cut with outdoor scenes that match the forecast under a voiceover. State only what the material says.`,
	},
	"variety": {
		Key:           "variety",
		Title:         "バラエティー",
		Minutes:       6,
		NeedsMaterial: true,
		Brief:         `A Japanese TV variety show in a bright, colourful studio with two regular hosts (one lively, one deadpan; fictional people) and one guest. Corners: an opening chat about today's real light topics from the material (sports, science, culture — refer to them by subject, never by a real person's name), a quiz corner with questions built from those topics, a small physical challenge or game with big reactions, and a warm sign-off. Studio laughter and applause; silly but never mean.`,
	},
	"music_show": {
		Key:           "music_show",
		Title:         "ミュージックステージ",
		Minutes:       6,
		NeedsMaterial: false,
		Brief:         `A live music show on a neon-lit stage with one stylish host and three fictional acts (invented Japanese or English names, never a real artist), each in a different genre (city pop, jazz trio, idol pop, rock, acoustic folk, electronic...). For each act: the host's short introduction, then several performance shots (wide shot of the band, close-up of the singer, an instrument, the crowd). The music is the performance itself: describe it precisely in non_diegetic_music (instruments, tempo, mood) and keep it consistent across the shots of one act; one or two short sung Japanese lines per act inside <d>[Japanese] ...</d>, otherwise no speech during performances. Applause and crowd in the soundscape.`,
	},
	"nature": {
		Key:           "nature",
		Title:         "自然ドキュメンタリー",
		Minutes:       6,
		NeedsMaterial: false,
		Brief:         `A nature documentary with a warm, slow Japanese narration (off-screen voiceover, one narrator voice for the whole programme). Pick one ecosystem (deep sea, rainforest canopy, arctic tundra, desert night, mountain river, mangrove, alpine meadow...) and tell it as a day or a season: sections for dawn, the hunt, the young, weather arriving, dusk... Rich ambient sound, sparse music. No people.`,
	},
	"cooking": {
		Key:           "cooking",
		Title:         "3分クッキング",
		Minutes:       5,
		NeedsMaterial: false,
		Brief:         `A cooking show in a bright kitchen with one cheerful chef who carries the whole programme. Pick one simple dish and go step by step: ingredients, prep, the key cooking action with sizzling close-ups, plating, tasting, and a closing tip. The chef talks to the viewer in short friendly Japanese sentences.`,
	},
	"travel": {
		Key:           "travel",
		Title:         "ぶらり旅",
		Minutes:       6,
		NeedsMaterial: false,
		Brief:         `A relaxed travel programme: one reporter (fictional) wanders through an invented but believable place of a chosen type (a harbour town, a mountain village, a hot-spring town, an old merchant street, an island...). Sections: arrival, a walk, a local craft or shop, food, a viewpoint, and a farewell. The reporter speaks in short warm Japanese sentences, sometimes as a voiceover over scenery. Never a real named place or landmark; no on-screen text.`,
	},
	"cm": {
		Key:           "cm",
		Title:         "CM",
		Minutes:       0.1,
		NeedsMaterial: false,
		Brief:         `A 6-second TV commercial for a fictional, harmless product (a drink, a gadget, a travel destination, a snack). Playful and visual, with a short Japanese slogan spoken by a narrator or a character. No on-screen text.`,
	},
	"ident": {
		Key:           "ident",
		Title:         "ステーションID",
		Minutes:       0.07,
		NeedsMaterial: false,
		Brief:         `A 4-second station ident for the channel: abstract, colourful motion graphics or a whimsical mascot creature, with a short spoken channel jingle in Japanese by a bright voice, e.g. the station name and 'ローカルAI放送局'. No on-screen text.`,
	},
}

// Themes are subjects rotated day by day per format (nature, cooking, travel, music_show, variety): the lists
// from aitv/programs.py THEMES, verbatim and in order.
var Themes = map[string][]string{
	"nature":     {"深海", "熱帯雨林の樹冠", "北極のツンドラ", "砂漠の夜", "山の渓流", "マングローブ", "高山の草原", "サンゴ礁", "サバンナ", "竹林", "湿原", "火山島", "氷河", "干潟", "夜の森", "海氷の縁"},
	"cooking":    {"和食の煮物", "麺料理", "丼もの", "パンとサンドイッチ", "スープ", "サラダ", "スイーツ", "イタリアン", "中華", "エスニック", "朝ごはん", "お弁当", "鍋料理", "揚げ物", "焼き菓子", "魚料理"},
	"travel":     {"港町", "山あいの村", "温泉街", "古い商人の町並み", "離島", "湖畔の町", "雪国の集落", "棚田の里", "運河の町", "高原の町", "城下町", "漁村", "茶畑の丘", "夜の屋台街", "灯台のある岬"},
	"music_show": {"シティポップ", "ジャズトリオ", "アイドルポップ", "ロック", "アコースティックフォーク", "エレクトロニカ", "R&B", "ボサノバ", "ヒップホップ", "ブラスバンド", "エレクトロスウィング", "演歌風バラード", "ファンク", "アンビエント", "パンク", "ピアノソロ"},
	"variety":    {"早押しクイズ", "ジェスチャーゲーム", "利き味対決", "なぞなぞ", "○×クイズ", "記憶力ゲーム", "お絵かき当てゲーム", "早口言葉", "バランスゲーム", "百人一首風かるた", "しりとり", "モノマネ"},
}

// ThemeKeys maps a theme to the strings that reveal it in a written script (aitv/programs.py THEME_KEYS,
// verbatim). A theme missing here matches on its own name.
var ThemeKeys = map[string][]string{
	"シティポップ":       {"シティポップ", "シティ・ポップ", "city pop", "city-pop"},
	"ジャズトリオ":       {"ジャズ", "jazz"},
	"アイドルポップ":      {"アイドル", "idol"},
	"ロック":          {"ロック", "rock"},
	"アコースティックフォーク": {"フォーク", "folk", "アコースティック", "acoustic"},
	"エレクトロニカ":      {"エレクトロニカ", "electronic", "electronica", "synth"},
	"R&B":          {"r&b", "rnb", "ソウル", "soul"},
	"ボサノバ":         {"ボサノバ", "bossa"},
	"ヒップホップ":       {"ヒップホップ", "hip hop", "hip-hop", "rap"},
	"ブラスバンド":       {"ブラス", "brass"},
	"エレクトロスウィング":   {"スウィング", "swing"},
	"演歌風バラード":      {"演歌", "enka", "バラード", "ballad"},
	"ファンク":         {"ファンク", "funk"},
	"アンビエント":       {"アンビエント", "ambient"},
	"パンク":          {"パンク", "punk"},
	"ピアノソロ":        {"ピアノ", "piano"},
	"深海":           {"深海", "deep sea", "deep-sea"},
	"熱帯雨林の樹冠":      {"熱帯雨林", "rainforest", "ジャングル"},
	"北極のツンドラ":      {"北極", "ツンドラ", "arctic", "tundra"},
	"砂漠の夜":         {"砂漠", "desert"},
	"山の渓流":         {"渓流", "mountain river", "清流"},
	"マングローブ":       {"マングローブ", "mangrove"},
	"高山の草原":        {"高山", "alpine"},
	"サンゴ礁":         {"サンゴ", "coral"},
	"サバンナ":         {"サバンナ", "savanna"},
	"竹林":           {"竹林", "bamboo"},
	"湿原":           {"湿原", "wetland", "marsh"},
	"火山島":          {"火山", "volcan"},
	"氷河":           {"氷河", "glacier"},
	"干潟":           {"干潟", "tidal flat", "mudflat"},
	"夜の森":          {"夜の森", "night forest"},
	"海氷の縁":         {"海氷", "sea ice"},
	"港町":           {"港町", "harbour", "harbor", "港"},
	"山あいの村":        {"山あい", "山里", "mountain village"},
	"温泉街":          {"温泉", "hot spring", "onsen"},
	"古い商人の町並み":     {"商人", "merchant", "町並み"},
	"離島":           {"離島", "island", "島"},
	"湖畔の町":         {"湖", "lake"},
	"雪国の集落":        {"雪国", "snow"},
	"棚田の里":         {"棚田", "rice terrace"},
	"運河の町":         {"運河", "canal"},
	"高原の町":         {"高原", "highland", "plateau"},
	"城下町":          {"城下町", "castle town"},
	"漁村":           {"漁村", "漁港", "fishing"},
	"茶畑の丘":         {"茶畑", "tea"},
	"夜の屋台街":        {"屋台", "street food", "stall"},
	"灯台のある岬":       {"灯台", "lighthouse", "岬", "cape"},
}

// Edition is the part of the day a programme written now belongs to: 朝 (< 11), 昼 (< 15), 夕方 (< 19), 夜.
func Edition(now time.Time) string {
	switch h := now.Hour(); {
	case h < 11:
		return "朝"
	case h < 15:
		return "昼"
	case h < 19:
		return "夕方"
	}
	return "夜"
}

// DateLine is "2006-01-02 15:04 = M月D日 X曜日 <edition>" for the writer prompts (zero now = time.Now()).
func DateLine(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	weekday := []rune(WeekdaysJA)[(int(now.Weekday())+6)%7] // Python weekday(): Monday = 0
	return fmt.Sprintf("%s = %d月%d日 %c曜日 %s", now.Format("2006-01-02 15:04"), int(now.Month()), now.Day(), weekday, Edition(now))
}

// Cast is one person in a programme, with the five fields the outline prompt asks for (and scripts/*.json
// stores).
type Cast struct {
	Role  string `json:"role"`
	Name  string `json:"name"`
	Look  string `json:"look"`
	Voice string `json:"voice"`
	// Regular marks the fixed presenter / narrator / chef / reporter carried from episode to episode.
	Regular bool `json:"regular"`
	// Kana is the hiragana reading of Name (family and given name separated like Name), so the dialogue
	// converter (internal/yomi) reads the name the way the writer meant it. Empty for scripts written
	// before it was asked for: such a name is left in kanji for H3 to read.
	Kana string `json:"kana,omitempty"`
}

// Episode summarises a past programme of a format for the writer (script.History builds these).
type Episode struct {
	Written  string
	Title    string
	Theme    string
	Summary  string
	NextTime string
	Cast     []Cast
	Sources  []string
	// Text is title + theme + summary + every section's "name plan", space-joined: what PickThemes searches.
	Text string
}

// OutlineSection is one section of an outline.
type OutlineSection struct {
	Name    string
	Seconds int
	Plan    string
}

// Outline is the LLM's programme plan after the script package normalised it.
type Outline struct {
	Title    string
	Theme    string
	Summary  string
	NextTime string
	Cast     []Cast
	Sections []OutlineSection
}

// OutlineInput is everything the outline request may mention.
type OutlineInput struct {
	Minutes float64
	// Material is the real-material block ("" when none).
	Material string
	// Now is the wall clock for the date line (zero = time.Now()).
	Now time.Time
	// Regulars are the fixed presenters carried over from the last episode (nil = none).
	Regulars []Cast
	// Recent are the last episodes (title, theme, summary, next_time), oldest first (nil = none).
	Recent []Episode
	// Themes are today's rotation suggestion (nil = none).
	Themes []string
	// Reported are headlines already read in recent news programmes (nil = none).
	Reported []string
}

// lookup returns the format for key. An unknown key is a programming error (the keys come from Lineup and
// BreakFormats); it panics as the Python raised KeyError, which the producer loop recovers from.
func lookup(key string) Format {
	f, ok := Formats[key]
	if !ok {
		panic("programs: unknown format " + strconv.Quote(key))
	}
	return f
}

// systemPrompt is the system message shared by every writer request (aitv/programs.py _system).
func systemPrompt() string {
	return "You are the head writer of '" + STATION.Name + "', " + STATION.Tagline + ". " + H3FormatRules +
		"\nAll titles, captions, names and dialogue are Japanese. Return ONLY JSON."
}

// formatG renders a float the way Python's f"{x:g}" does: 6 significant digits, trailing zeros dropped.
func formatG(x float64) string { return strconv.FormatFloat(x, 'g', 6, 64) }

// segmentCount is how many clips a section of secs seconds asks for: Python's `max(1, round(secs / 8))`, where
// round() is banker's rounding (36 s → 4, 44 s → 6, 60 s → 8).
func segmentCount(secs int) int {
	n := int(math.RoundToEven(float64(secs) / 8))
	if n < 1 {
		n = 1
	}
	return n
}

// firstRunes is Python's s[:n] (code points, not bytes).
func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// castLine is the "- role: name — look Voice: voice" line used by both the outline and the section prompt.
func castLine(c Cast) string {
	return fmt.Sprintf("- %s: %s — %s Voice: %s", c.Role, c.Name, c.Look, c.Voice)
}

func castLines(cast []Cast) string {
	lines := make([]string, len(cast))
	for i, c := range cast {
		lines[i] = castLine(c)
	}
	return strings.Join(lines, "\n")
}

// OutlinePrompt returns the (system, user) pair of the outline request (aitv/programs.py outline_prompt).
func OutlinePrompt(fmtKey string, in OutlineInput) (system, user string) {
	f := lookup(fmtKey)
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	total := int(in.Minutes * 60)
	parts := []string{
		fmt.Sprintf("Plan a %s-minute %s programme (%s). Now: %s. ", formatG(in.Minutes), f.Title, fmtKey, DateLine(now)) +
			"Mention the date/time only if natural — except where the concept requires it.",
		"Concept: " + f.Brief,
	}
	if len(in.Regulars) > 0 {
		parts = append(parts, "Regular cast — this programme's fixed people. Keep their names, looks and voices EXACTLY as written; add today's guests or acts as new cast entries with \"regular\": false:\n"+
			castLines(in.Regulars))
	}
	if len(in.Recent) > 0 {
		lines := make([]string, len(in.Recent))
		for i, e := range in.Recent {
			theme := e.Theme
			if theme == "" {
				theme = "-"
			}
			line := fmt.Sprintf("- %s 『%s』 theme: %s — %s", firstRunes(e.Written, 10), e.Title, theme, e.Summary)
			if e.NextTime != "" {
				line += " / next time: " + e.NextTime
			}
			lines[i] = line
		}
		parts = append(parts, "Recent episodes of this programme (do NOT repeat their subjects or titles; if the last one left a thread or a teaser, pick it up naturally):\n"+
			strings.Join(lines, "\n"))
	}
	if len(in.Themes) > 0 {
		s := "Today's theme — the episode MUST be built on this (it has not been used recently; the recent episodes' " +
			"subjects are off-limits): " + strings.Join(in.Themes, " / ")
		if fmtKey == "music_show" {
			s += ". These are the genres of the three acts, one each."
		}
		parts = append(parts, s)
	}
	if len(in.Reported) > 0 {
		lines := make([]string, len(in.Reported))
		for i, t := range in.Reported {
			lines[i] = "- " + t
		}
		parts = append(parts, "Already reported in recent programmes — skip these unless the material shows a new development, and lead with what is new:\n"+
			strings.Join(lines, "\n"))
	}
	if in.Material != "" {
		parts = append(parts, "Material:\n"+in.Material)
	}
	parts = append(parts,
		`Return JSON: {"title": str (Japanese, distinctive for today), "theme": str (Japanese, the subject of today's episode in a few words), `+
			`"cast": [{"role": str, "name": str (Japanese), "look": str (English, one precise sentence: age, hair, clothes, setting), `+
			`"voice": str (English, a few words), "kana": str (the name's reading in hiragana, spaced like the name, e.g. さとう みさき), "regular": bool (true for the fixed presenter/narrator/chef/reporter, false for today's guests or acts)}], `+
			`"sections": [{"name": str (Japanese), "seconds": int, "plan": str (English, 2-3 sentences: what happens, which material items are used, how it ends)}], `+
			`"summary": str (Japanese, one line: what this episode was about), "next_time": str (Japanese, one line said at the end as a teaser for the next episode, or empty)}`+"\n"+
			fmt.Sprintf("4 to 7 sections including an opening and a closing; the seconds must add up to about %d. ", total)+
			"Cast descriptions will be pasted verbatim into every shot, so make them specific enough to keep the same person recognisable.")
	return systemPrompt(), strings.Join(parts, "\n\n")
}

// SectionPrompt returns the (system, user) pair for writing one section (aitv/programs.py section_prompt).
// prevCaption "" means this is the first clip; material "" means none; now zero = time.Now().
// A section with Seconds 0 is budgeted at 60 s (Python: `section.get("seconds") or 60`).
func SectionPrompt(fmtKey string, outline Outline, section OutlineSection, prevCaption, material string, now time.Time) (system, user string) {
	f := lookup(fmtKey)
	if now.IsZero() {
		now = time.Now()
	}
	secs := section.Seconds
	if secs == 0 {
		secs = 60
	}
	n := segmentCount(secs)
	isLast := len(outline.Sections) > 0 && section.Name == outline.Sections[len(outline.Sections)-1].Name
	names := make([]string, len(outline.Sections))
	for i, s := range outline.Sections {
		names[i] = s.Name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Programme: %s (%s, %s). Concept: %s\n", outline.Title, f.Title, fmtKey, f.Brief)
	fmt.Fprintf(&b, "Now: %s — whenever the date or weekday is spoken, it is this one.\n", DateLine(now))
	if outline.Theme != "" {
		fmt.Fprintf(&b, "Today's theme: %s\n", outline.Theme)
	}
	fmt.Fprintf(&b, "Cast (repeat the look and voice verbatim whenever they appear; give each speaker a stable Sx number):\n%s\n", castLines(outline.Cast))
	b.WriteString("Sections in order: " + strings.Join(names, " / ") + "\n")
	if material != "" {
		fmt.Fprintf(&b, "Material:\n%s\n", material)
	}
	fmt.Fprintf(&b, "\nWrite section '%s' (%d s): %s\n", section.Name, secs, section.Plan)
	if prevCaption != "" {
		fmt.Fprintf(&b, "The previous clip ended with: %s\n", prevCaption)
	} else {
		b.WriteString("This is the first clip of the programme.\n")
	}
	if isLast && outline.NextTime != "" {
		fmt.Fprintf(&b, "This is the closing section: its last clip ends with this teaser, spoken by a regular: %s\n", outline.NextTime)
	}
	fmt.Fprintf(&b, `Return JSON: {"segments": [{"caption": str (Japanese, one short ticker line), "seconds": int (%d-%d), `+
		`"prompt": str (the full H3 prompt)}]} with about %d segments whose seconds add up to about %d. `, SegmentMinS, SegmentMaxS, n, secs)
	b.WriteString("Every clip is generated on its own: re-describe the setting and the cast in each prompt and keep continuity of action. " +
		"Speech: ONE spoken sentence per clip, at most 45 Japanese characters (a 10-second clip cannot hold more); " +
		"if a story needs more words, spread it over more clips instead of writing longer lines.")
	return systemPrompt(), b.String()
}

// BreakPrompt returns the (system, user) pair for one commercial ("cm", 6 s) or ident ("ident", 4 s);
// recentTitles lists recent ones to avoid (nil = none).
func BreakPrompt(fmtKey string, recentTitles []string) (system, user string) {
	f := lookup(fmtKey)
	seconds := 6
	if fmtKey == "ident" {
		seconds = 4
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Write one %d-second %s (%s). Concept: %s\n", seconds, f.Title, fmtKey, f.Brief)
	if len(recentTitles) > 0 {
		b.WriteString("Recent ones (make something clearly different): " + strings.Join(recentTitles, " / ") + "\n")
	}
	fmt.Fprintf(&b, `Return JSON: {"title": str (Japanese), "segments": [{"caption": str (Japanese), "seconds": %d, "prompt": str (the full H3 prompt)}]} with exactly one segment.`, seconds)
	return systemPrompt(), b.String()
}
