package yomi

import (
	"strings"
	"unicode/utf8"
)

// change is a counter's sound change for one last unit of the number: piece replaces the reading of that
// unit ("" keeps it, "っ" turns its last kana into っ) and counter replaces the counter's base reading
// ("" keeps it).
type change struct {
	piece   string
	counter string
}

// counter is a counter word: its base reading, the whole numbers with a reading of their own (ついたち,
// ひとり) and the sound changes by the number's last unit (1..9, 10, 100, 1000, 万).
type counter struct {
	base      string
	whole     map[int64]string
	changes   map[int64]change
	wholeOnly bool // the counter matches only the numbers in whole
	// resolve, when set, reads a plain whole number from its context (1日 as ついたち or いちにち) and
	// is consulted before whole.
	resolve func(n number, ctx numCtx) (string, bool)
}

func newCounter(base string) *counter {
	return &counter{base: base, whole: map[int64]string{}, changes: map[int64]change{}}
}

// with sets the change for one last unit.
func (c *counter) with(unit int64, piece, counterReading string) *counter {
	c.changes[unit] = change{piece, counterReading}
	return c
}

// sokuon gives the units a っ ending (いっ, ろっ, はっ, じゅっ, ひゃっ) with the counter read as counterReading
// ("" keeps the base).
func (c *counter) sokuon(counterReading string, units ...int64) *counter {
	for _, u := range units {
		c.with(u, "っ", counterReading)
	}
	return c
}

// wholes sets the whole-number readings.
func (c *counter) wholes(m map[int64]string) *counter {
	for k, v := range m {
		c.whole[k] = v
	}
	return c
}

// native gives 1 and 2 the native readings ひと / ふた (一袋 ひとふくろ, 2組 ふたくみ).
func (c *counter) native() *counter {
	return c.wholes(map[int64]string{1: "ひと" + c.base, 2: "ふた" + c.base})
}

// suffix returns a copy of c with s appended to every reading (分 → 分間).
func (c *counter) suffix(s string) *counter {
	out := newCounter(c.base + s)
	for k, v := range c.whole {
		out.whole[k] = v + s
	}
	for k, v := range c.changes {
		if v.counter != "" {
			v.counter += s
		}
		out.changes[k] = v
	}
	return out
}

// nan reads 何 + the counter: なん with the sound change of 3 (何分 なんぷん, 何本 なんぼん, 何階 なんがい).
func (c *counter) nan() string {
	base := c.base
	if ch, ok := c.changes[3]; ok && ch.counter != "" {
		base = ch.counter
	}
	return "なん" + base
}

// sRow: いっ / はっ / じゅっ before s, t and p-sounding counters (歳, 点, パーセント).
func sRow(base string) *counter { return newCounter(base).sokuon("", 1, 8, 10) }

// kRow: いっ / ろっ / はっ / じゅっ / ひゃっ before k-row counters (回, 個, 曲).
func kRow(base string) *counter { return newCounter(base).sokuon("", 1, 6, 8, 10, 100) }

// hRow: h-row counters that take the p-sound after っ and the b-sound after 3, 1000 and 万 (本, 杯, 匹).
func hRow(base, p, b string) *counter {
	return newCounter(base).sokuon(p, 1, 6, 8, 10, 100).with(3, "", b).with(1000, "", b).with(1_0000, "", b)
}

// pRow: h-row counters that take the p-sound after っ, after 3 and 4 and after 1000 and 万 (分, 泊, 歩, 発).
func pRow(base, p string) *counter {
	return newCounter(base).sokuon(p, 1, 6, 8, 10, 100).with(3, "", p).with(4, "", p).with(1000, "", p).with(1_0000, "", p)
}

// kilo: キロ and the units built on it: ろっキロ, じゅっキロ, ひゃっキロ but いちキロ, はちキロ.
func kilo(base string) *counter { return newCounter(base).sokuon("", 6, 10, 100) }

// hours: 時 with よじ, しちじ, くじ.
func hours(base string) *counter {
	return newCounter(base).with(4, "よ", "").with(7, "しち", "").with(9, "く", "")
}

// four: counters whose 4 is よ (年, 円, 人).
func four(base string) *counter { return newCounter(base).with(4, "よ", "") }

// people: 人 and its compounds (人組, 人前): ひとり, ふたり, よにん, しちにん.
func people(base string) *counter {
	return four(base).with(7, "しち", "").wholes(map[int64]string{1: "ひと" + strings.Replace(base, "にん", "り", 1), 2: "ふた" + strings.Replace(base, "にん", "り", 1)})
}

// ordinal is an English ordinal suffix read as its katakana word (1st ファースト).
func ordinal(m map[int64]string) *counter {
	return &counter{whole: m, wholeOnly: true}
}

var (
	minutes = pRow("ふん", "ぷん")
	months  = newCounter("がつ").with(4, "し", "").with(7, "しち", "").with(9, "く", "")
	years   = four("ねん")
	days    = newCounter("にち").wholes(map[int64]string{
		1: "ついたち", 2: "ふつか", 3: "みっか", 4: "よっか", 5: "いつか",
		6: "むいか", 7: "なのか", 8: "ようか", 9: "ここのか", 10: "とおか", 20: "はつか",
	}).with(4, "よっ", "か").with(7, "しち", "").with(9, "く", "")
	daysSpan = days.suffix("かん").wholes(map[int64]string{1: "いちにちかん"})
	classes  = kRow("くみ").native()
)

// dateFollowers are what a calendar 1日 is followed by in a news or weather line (1日午前, 1日夜,
// 1日から3日にかけて, 1日、東京で).
var dateFollowers = map[string]bool{
	"午前": true, "午後": true, "未明": true, "朝": true, "昼": true, "夜": true, "夕方": true, "正午": true,
	"深夜": true, "早朝": true, "夜間": true, "昼前": true, "昼過ぎ": true, "夕": true, "から": true, "まで": true,
	"、": true, "，": true, "。": true, "は": true, "も": true, "（": true, "(": true, "）": true, ")": true,
	"生まれ": true, "付": true, "付け": true, "発": true,
}

func init() {
	// 1日 is ついたち after a month (9月1日, 来月1日) and, in Arabic digits, when what follows reads like
	// a date line; otherwise it is a length of time: いちにち (1日3回, 1日おき, 一日).
	days.resolve = func(n number, ctx numCtx) (string, bool) {
		if n.whole != 1 {
			return "", false
		}
		if ctx.prev == months {
			return "ついたち", true
		}
		if t, ok := ctx.before(); ok && strings.HasSuffix(t.surface, "月") {
			return "ついたち", true
		}
		if !n.arabic {
			return "いちにち", true
		}
		t, ok := ctx.next(0)
		if !ok || dateFollowers[t.surface] {
			return "ついたち", true
		}
		if t.surface == "に" {
			if t2, ok := ctx.next(1); !ok || !t2.numeral {
				return "ついたち", true
			}
		}
		return "いちにち", true
	}
	// 1組 / 2組 are ひとくみ / ふたくみ except as a class number after a school year (3年2組 さんねんにくみ).
	classes.resolve = func(n number, ctx numCtx) (string, bool) {
		if ctx.prev == years && (n.whole == 1 || n.whole == 2) {
			return digitKana[n.whole] + "くみ", true
		}
		return "", false
	}
}

// counters maps a counter surface (one token, or two or three adjacent tokens) to its readings.
var counters = map[string]*counter{
	"分":       minutes,
	"分間":      minutes.suffix("かん"),
	"時":       hours("じ"),
	"時半":      hours("じ").suffix("はん"),
	"時間":      newCounter("じかん").with(4, "よ", "").with(9, "く", ""),
	"月":       months,
	"日":       days,
	"日間":      daysSpan,
	"日中":      &counter{whole: map[int64]string{1: "いちにちじゅう"}, wholeOnly: true},
	"人":       people("にん"),
	"人組":      people("にんぐみ"),
	"人前":      four("にんまえ").with(7, "しち", ""),
	"人分":      people("にんぶん"),
	"人暮らし":    people("にんぐらし"),
	"本":       hRow("ほん", "ぽん", "ぼん"),
	"杯":       hRow("はい", "ぱい", "ばい"),
	"匹":       hRow("ひき", "ぴき", "びき"),
	"回":       kRow("かい"),
	"個":       kRow("こ"),
	"階":       kRow("かい").with(3, "", "がい"),
	"軒":       kRow("けん").with(3, "", "げん"),
	"件":       kRow("けん"),
	"箇所":      kRow("かしょ"),
	"か所":      kRow("かしょ"),
	"ヶ所":      kRow("かしょ"),
	"ヵ所":      kRow("かしょ"),
	"カ所":      kRow("かしょ"),
	"ケ所":      kRow("かしょ"),
	"ヶ月":      kRow("かげつ"),
	"ヵ月":      kRow("かげつ"),
	"か月":      kRow("かげつ"),
	"カ月":      kRow("かげつ"),
	"ケ月":      kRow("かげつ"),
	"箇月":      kRow("かげつ"),
	"か国":      kRow("かこく"),
	"ヶ国":      kRow("かこく"),
	"ヵ国":      kRow("かこく"),
	"カ国":      kRow("かこく"),
	"ケ国":      kRow("かこく"),
	"箇国":      kRow("かこく"),
	"冊":       sRow("さつ"),
	"歳":       sRow("さい").wholes(map[int64]string{20: "はたち"}),
	"才":       sRow("さい").wholes(map[int64]string{20: "はたち"}),
	"週":       sRow("しゅう"),
	"週間":      sRow("しゅうかん"),
	"周":       sRow("しゅう"),
	"周年":      sRow("しゅうねん"),
	"度":       newCounter("ど"),
	"℃":       newCounter("ど"),
	"°C":      newCounter("ど"),
	"秒":       newCounter("びょう"),
	"秒間":      newCounter("びょうかん"),
	"年":       years,
	"年間":      four("ねんかん"),
	"年生":      four("ねんせい"),
	"円":       four("えん"),
	"番":       newCounter("ばん"),
	"枚":       newCounter("まい"),
	"台":       newCounter("だい"),
	"曲":       kRow("きょく"),
	"名":       newCounter("めい"),
	"話":       newCounter("わ"),
	"位":       newCounter("い"),
	"点":       sRow("てん"),
	"%":       sRow("パーセント"),
	"％":       sRow("パーセント"),
	"パーセント":   sRow("パーセント"),
	"km":      kilo("キロメートル"),
	"kg":      kilo("キログラム"),
	"kcal":    kilo("キロカロリー"),
	"kW":      kilo("キロワット"),
	"km/h":    kilo("キロメートルまいじ"),
	"cm":      sRow("センチメートル"),
	"mm":      newCounter("ミリメートル"),
	"m":       newCounter("メートル"),
	"g":       newCounter("グラム"),
	"mg":      newCounter("ミリグラム"),
	"ml":      newCounter("ミリリットル"),
	"mL":      newCounter("ミリリットル"),
	"L":       newCounter("リットル"),
	"ℓ":       newCounter("リットル"),
	"dl":      newCounter("デシリットル"),
	"dL":      newCounter("デシリットル"),
	"cc":      sRow("シーシー"),
	"W":       newCounter("ワット"),
	"キロ":      kilo("キロ"),
	"キロメートル":  kilo("キロメートル"),
	"キログラム":   kilo("キログラム"),
	"キロカロリー":  kilo("キロカロリー"),
	"センチ":     sRow("センチ"),
	"センチメートル": sRow("センチメートル"),
	"ミリ":      newCounter("ミリ"),
	"割":       newCounter("わり"),
	"割方":      newCounter("わりがた"),
	"倍":       newCounter("ばい"),
	"つ": newCounter("つ").wholes(map[int64]string{
		1: "ひとつ", 2: "ふたつ", 3: "みっつ", 4: "よっつ", 5: "いつつ",
		6: "むっつ", 7: "ななつ", 8: "やっつ", 9: "ここのつ", 10: "とお",
	}),
	"頭":  sRow("とう"),
	"羽":  newCounter("わ"),
	"通":  sRow("つう"),
	"丁":  sRow("ちょう"),
	"足":  sRow("そく").with(3, "", "ぞく"),
	"世帯": sRow("せたい"),
	"社":  sRow("しゃ"),
	"種":  sRow("しゅ"),
	"種類": sRow("しゅるい"),
	"号":  newCounter("ごう"),
	"号室": newCounter("ごうしつ"),
	"丁目": sRow("ちょうめ"),
	"泊":  pRow("はく", "ぱく"),
	"歩":  pRow("ほ", "ぽ"),
	"発":  pRow("はつ", "ぱつ"),
	"品":  pRow("ひん", "ぴん"),
	"校":  kRow("こう"),
	"席":  sRow("せき"),
	"隻":  sRow("せき"),
	"巻":  kRow("かん"),
	"合":  newCounter("ごう"),
	"小節": sRow("しょうせつ"),
	"首":  sRow("しゅ"),
	"強":  newCounter("きょう").sokuon("", 6), // 震度6強 ろっきょう; 5強 ごきょう, 6弱 ろくじゃく
	"組":  classes,
	"食":  sRow("しょく"),
	// native counters of the kitchen and the market: 一袋 ひとふくろ, 二切れ ふたきれ, 10口 じゅっくち
	"株":   newCounter("かぶ").native(),
	"晩":   newCounter("ばん").native(),
	"つまみ": newCounter("つまみ").native(),
	"摘み":  newCounter("つまみ").native(),
	"切れ":  kRow("きれ").native(),
	"房":   newCounter("ふさ").native(),
	"束":   newCounter("たば").native(),
	"袋":   hRow("ふくろ", "ぷくろ", "ぶくろ").native(),
	"缶":   kRow("かん").native(),
	"箱":   hRow("はこ", "ぱこ", "ばこ").native(),
	"玉":   newCounter("たま").native(),
	"皿":   sRow("さら").native(),
	"振り":  newCounter("ふり").native(),
	"鍋":   newCounter("なべ").native(),
	"口":   kRow("くち").native(),
	"粒":   sRow("つぶ").native(),
	"握り":  newCounter("にぎり").native(),
	"片":   newCounter("かけ").native(), // 生姜一片 ひとかけ (一片 as one word stays いっぺん)
	"かけ":  newCounter("かけ").native(),
	"欠け":  newCounter("かけ").native(),
	"匙":   newCounter("さじ").native(),
	"煮立ち": newCounter("にたち").native(),
	"混ぜ":  newCounter("まぜ").native(),
	"手間":  newCounter("てま").native(),
	"回し":  newCounter("まわし").native(),
	"つかみ": newCounter("つかみ").native(),
	"掴み":  newCounter("つかみ").native(),
	"抱え":  newCounter("かかえ").native(),
	// English ordinals glued to a digit (1stアルバム)
	"st": ordinal(map[int64]string{1: "ファースト"}),
	"nd": ordinal(map[int64]string{2: "セカンド"}),
	"rd": ordinal(map[int64]string{3: "サード"}),
	"th": ordinal(map[int64]string{4: "フォース", 5: "フィフス", 6: "シックスス", 7: "セブンス", 8: "エイス", 9: "ナインス", 10: "テンス"}),
}

// maxCounterTokens is how many adjacent tokens a counter surface may span (km/h).
const maxCounterTokens = 3

// matchCounter looks the token(s) at i up in the counter table, longest run of adjacent surfaces first
// (日中, km/h), and returns the counter and the index after it, or nil and i.
func matchCounter(toks []token, i int, n number) (*counter, int) {
	if i >= len(toks) {
		return nil, i
	}
	for k := maxCounterTokens; k >= 1; k-- {
		if i+k > len(toks) {
			continue
		}
		var key string
		for _, t := range toks[i : i+k] {
			if t.kind != tokPlain {
				key = ""
				break
			}
			key += t.surface
		}
		if key == "" {
			continue
		}
		if c, ok := counters[key]; ok && c.accepts(n) {
			return c, i + k
		}
	}
	return nil, i
}

func (c *counter) accepts(n number) bool {
	if !c.wholeOnly {
		return true
	}
	if n.decimals != "" || n.overflow || n.denom != 0 || len(n.bare) > 0 || len(n.juxta) > 0 || n.maru > 0 {
		return false
	}
	_, ok := c.whole[n.whole]
	return ok
}

// gluedNouns are the kitchen nouns the dictionary glues to a counter into a compound of its own
// (30分水に → 分水 ブンスイ), which hides both the counter and the noun.
var gluedNouns = map[string]bool{"水": true, "油": true, "湯": true, "塩": true, "卵": true, "皮": true, "粉": true, "汁": true, "火": true}

// splitGlued cuts such a compound after a number into the counter and the noun, so that 30分水に reads
// さんじゅっぷんみずに like 一晩水に (which the dictionary already splits).
func splitGlued(toks []token) []token {
	out := make([]token, 0, len(toks))
	for i, t := range toks {
		if i > 0 && toks[i-1].numeral && t.kind == tokPlain && hasKanji(t.surface) {
			if _, ok := counters[t.surface]; !ok {
				if c, rest, ok := gluedSplit(t.surface); ok {
					out = append(out, token{surface: c, pos: "名詞-接尾-助数詞"}, token{surface: rest, pos: "名詞-接尾-一般"})
					continue
				}
			}
		}
		out = append(out, t)
	}
	return out
}

// gluedSplit splits s into a counter surface and a glued noun, longest counter first.
func gluedSplit(s string) (string, string, bool) {
	for i := len(s) - 1; i > 0; i-- {
		if !utf8.RuneStart(s[i]) {
			continue
		}
		if _, ok := counters[s[:i]]; ok && gluedNouns[s[i:]] {
			return s[:i], s[i:], true
		}
	}
	return "", "", false
}
