package yomi

import (
	"strings"
	"unicode/utf8"
)

// This file holds what sits between the dictionary and the output: broadcast readings for words the IPA
// dictionary reads wrong (or does not know) in the station's three registers — weather and disaster
// lines, cooking segments, travel narration — and the context rules for homographs whose reading a
// newsreader picks from the words around them.

// textOverrides are replaced in the text before it is tokenised: the few words the dictionary cannot
// segment (予熱 has no entry and 予 no reading; the verb 予熱する conjugates, so the word never ends on a
// token boundary). Only substrings that are not part of another word belong here.
var textOverrides = [][2]string{
	{"予熱", "よねつ"},
}

func applyTextOverrides(text string) string {
	for _, o := range textOverrides {
		if strings.Contains(text, o[0]) {
			text = strings.ReplaceAll(text, o[0], o[1])
		}
	}
	return text
}

// kanjiFallback reads single kanji the dictionary has no reading for (kagome leaves them as unknown
// words), so that no kanji leaks into the kana output.
var kanjiFallback = map[string]string{
	"予": "よ",
	"雹": "ひょう",
	"霰": "あられ",
	"霙": "みぞれ",
	"〆": "しめ",
}

// overrides maps a word — the concatenated surfaces of one to maxOverrideTokens adjacent tokens — to
// its broadcast reading. Matching on whole tokens keeps the dictionary's segmentation: 高潮 → たかしお
// never touches 最高潮. Katakana in a value stays katakana in the output.
var overrides = map[string]string{
	// weather, disaster
	"日中":    "にっちゅう",
	"高潮":    "たかしお",
	"平野部":   "へいやぶ",
	"避難所":   "ひなんじょ",
	"数日":    "すうじつ",
	"数日間":   "すうじつかん",
	"数日来":   "すうじつらい",
	"明後日":   "あさって",
	"雹":     "ひょう",
	"霰":     "あられ",
	"霙":     "みぞれ",
	"風向き":   "かざむき",
	"波高":    "はこう",
	"警報級":   "けいほうきゅう",
	"線状降水帯": "せんじょうこうすいたい",
	"何分":    "なんぷん",
	// cooking
	"鶏肉":   "とりにく",
	"鶏挽肉":  "とりひきにく",
	"鶏挽き肉": "とりひきにく",
	"長ねぎ":  "ながねぎ",
	"長ネギ":  "ながネギ",
	"長葱":   "ながねぎ",
	"茄子":   "なす",
	"蓮根":   "れんこん",
	"半月切り": "はんげつぎり",
	"細切り":  "ほそぎり",
	"小口切り": "こぐちぎり",
	"落とし蓋": "おとしぶた",
	"汁気":   "しるけ",
	"湯切り":  "ゆきり",
	"中辛":   "ちゅうから",
	"大辛":   "おおから",
	"大トロ":  "おおトロ",
	"炒め物":  "いためもの",
	"焼き色":  "やきいろ",
	"揚げ色":  "あげいろ",
	"焦げ色":  "こげいろ",
	"下味":   "したあじ",
	"薄力粉":  "はくりきこ",
	"強力粉":  "きょうりきこ",
	"片栗粉":  "かたくりこ",
	"生地":   "きじ",
	"唐揚げ":  "からあげ",
	"唐揚":   "からあげ",
	"何色":   "なにいろ",
	// travel narration
	"港町":   "みなとまち",
	"干物":   "ひもの",
	"山道":   "やまみち",
	"一本道":  "いっぽんみち",
	"秘湯":   "ひとう",
	"花畑":   "はなばたけ",
	"内海":   "うちうみ",
	"外海":   "そとうみ",
	"門前町":  "もんぜんまち",
	"木立":   "こだち",
	"干潟":   "ひがた",
	"山門":   "さんもん",
	"灯り":   "あかり",
	"川下":   "かわしも",
	"川上":   "かわかみ",
	"瀬":    "せ",
	"河原":   "かわら",
	"手水舎":  "ちょうずや",
	"手水":   "ちょうず",
	"十人十色": "じゅうにんといろ",
	"四つ葉":  "よつば",
	"百人一首": "ひゃくにんいっしゅ",
	"億万長者": "おくまんちょうじゃ",
}

// maxOverrideTokens is how many adjacent tokens an override key may span.
const maxOverrideTokens = 4

// matchOverride looks the tokens at i up in overrides, longest run first, and returns the reading and
// the number of tokens it covers (0 when none matches).
func matchOverride(toks []token, i int) (string, int) {
	var key string
	best, n := "", 0
	for k := 0; k < maxOverrideTokens && i+k < len(toks); k++ {
		t := toks[i+k]
		if t.kind != tokPlain {
			break
		}
		key += t.surface
		if v, ok := overrides[key]; ok {
			best, n = v, k+1
		}
	}
	return best, n
}

// weatherNoun reports whether a token names something that falls from the sky (what 降り after it means
// ふり): 雨, 大雨, 雪, みぞれ, 雹, 火山灰.
func weatherNoun(s string) bool {
	switch s {
	case "みぞれ", "あられ", "ひょう", "雨粒", "雪片", "雷雨", "灰":
		return true
	}
	for _, suf := range []string{"雨", "雪", "雹", "霰", "霙", "火山灰"} {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// skyWord reports whether a token is weather wording that 後 sits between in a forecast (晴れ後曇り,
// 南の風後北の風): 後 there is のち.
func skyWord(s string) bool {
	switch s {
	case "晴れ", "晴", "曇り", "曇", "雨", "雪", "霧", "雷", "みぞれ", "雷雨", "風",
		"北", "南", "東", "西", "北東", "北西", "南東", "南西", "所", "ところ", "時々", "一時":
		return true
	}
	return false
}

// meatPart is what follows 鶏 / 牛 when they are an ingredient (とり / ぎゅう) rather than the animal.
var meatPart = []string{
	"肉", "もも", "モモ", "むね", "ムネ", "胸", "手羽", "ひき", "挽", "ささみ", "ささ身", "ささ", "ササミ", "皮", "がら", "ガラ", "団子",
	"だんご", "レバー", "軟骨", "薄切り", "切り落とし", "こま", "細切れ", "小間", "バラ", "ばら", "ロース", "ヒレ", "ひれ",
	"タン", "たん", "ホルモン", "カツ", "串", "スジ", "すじ", "筋", "ミンチ", "骨", "脂", "テール", "ハラミ", "カルビ",
	"ステーキ", "肩", "赤身", "霜降り", "ハツ", "砂肝", "せせり", "つくね", "そぼろ", "ハム", "ガラスープ", "丼",
	"唐揚げ", "唐揚", "唐", "から揚げ", "からあげ", "照り焼き", "煮込み", "鍋", "スープ", "水炊き", "竜田揚げ", "南蛮", "塩焼き",
	"丸焼き", "香草焼き", "焼き", "チャーシュー", "ミートボール", "ハンバーグ", "しゃぶしゃぶ", "すき焼き", "たたき", "刺身",
}

func meatFollows(s string) bool {
	for _, p := range meatPart {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// suffixKun are single kanji the dictionary reads with their on-yomi as a suffix (水 スイ, 色 ショク) when
// they are really a word of their own after a counter or a verb stem: 一晩水に ひとばんみず, 焼き色 やきいろ,
// 炒め物 いためもの, 雪解け水 ゆきどけみず.
var suffixKun = map[string]string{
	"水": "みず", "油": "あぶら", "湯": "ゆ", "塩": "しお", "卵": "たまご", "皮": "かわ", "粉": "こな", "汁": "しる",
	"骨": "ほね", "葉": "は", "実": "み", "種": "たね", "芯": "しん", "身": "み", "米": "こめ", "火": "ひ", "色": "いろ",
	"物": "もの", "肉": "にく",
}

// furiNext are the verb tails only 降る (ふる) takes: 降り始め, 降り出し, 降り続き.
var furiNext = map[string]bool{
	"始め": true, "はじめ": true, "出し": true, "だし": true, "続き": true, "つづき": true, "積もっ": true, "積もり": true,
	"止ま": true, "止み": true, "止ん": true, "やま": true, "やみ": true, "やん": true, "しきっ": true, "しきり": true,
	"注ぎ": true, "そそぎ": true, "込め": true, "頻っ": true, "頻り": true,
}

// tsuraiNext are the words after which 辛い keeps the reading つらい (辛い思い, 辛い経験) rather than the
// kitchen's からい.
var tsuraiNext = map[string]bool{
	"思い": true, "経験": true, "時": true, "とき": true, "日々": true, "こと": true, "気持ち": true, "立場": true,
	"過去": true, "別れ": true, "状況": true, "現実": true, "記憶": true, "出来事": true, "時期": true, "仕事": true,
	"人生": true, "思い出": true, "練習": true, "治療": true, "毎日": true, "日": true, "夜": true, "冬": true,
	"目": true, "作業": true, "時間": true, "生活": true, "決断": true, "選択": true, "闘病": true, "訓練": true,
}

// weekdayKanji reads the one-kanji weekday abbreviations written in brackets after a date.
var weekdayKanji = map[string]string{"月": "げつ", "火": "か", "水": "すい", "木": "もく", "金": "きん", "土": "ど", "日": "にち", "祝": "しゅく"}

func isOpenBracket(s string) bool  { return s == "（" || s == "(" || s == "［" || s == "[" }
func isCloseBracket(s string) bool { return s == "）" || s == ")" || s == "］" || s == "]" }

// endsHiragana reports whether s ends with a hiragana letter.
func endsHiragana(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r >= 0x3041 && r <= 0x309F
}

// isKatakanaWord reports whether s is katakana only.
func isKatakanaWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isKatakana(r) {
			return false
		}
	}
	return true
}

// applyContext reads the token at i from its neighbours when a rule applies: the returned kana, the
// index after what was consumed and true; false leaves the token to the dictionary. prev is the counter
// of the number written just before i, nil otherwise.
func applyContext(toks []token, i int, prev *counter) (string, int, bool) {
	t := toks[i]
	at := func(k int) token {
		if k >= 0 && k < len(toks) {
			return toks[k]
		}
		return token{}
	}
	switch {
	// 雨が降ります: the 連用形 降り is ふり after something that falls from the sky, おり after を/から.
	case t.surface == "降り" && t.reading == "オリ" && t.isPOS("動詞"):
		if furiNext[at(i+1).surface] {
			return "ふり", i + 1, true
		}
		for k := i - 1; k >= 0 && i-k <= 12; k-- {
			s := toks[k].surface
			if s == "を" || s == "から" || s == "。" || s == "！" || s == "？" {
				break
			}
			if weatherNoun(s) {
				return "ふり", i + 1, true
			}
		}
	// 晴れ後曇り, 南の風、後北の風: 後 between weather words is のち.
	case t.surface == "後" && (t.reading == "ゴ" || t.reading == "コウ" || t.reading == "アト") &&
		(skyWord(at(i-1).surface) || skyWord(at(i+1).surface)):
		return "のち", i + 1, true
	// 数 before a number or counter is すう (数十人, 数日), never かず.
	case t.surface == "数" && t.reading == "カズ" && i+1 < len(toks):
		if c, _ := matchCounter(toks, i+1, number{whole: 3}); c != nil || toks[i+1].numeral {
			return "すう", i + 1, true
		}
	// 何 + counter: なんぷん, なんぼん, なんがつ, なんにん.
	case t.surface == "何" && i+1 < len(toks) && toks[i+1].surface != "つ":
		if c, after := matchCounter(toks, i+1, number{whole: 3}); c != nil && !c.wholeOnly {
			return c.nan(), after, true
		}
	// 鶏もも肉, 鶏の唐揚げ, 牛薄切り肉: the animal read as the ingredient.
	case t.surface == "鶏" && (meatFollows(at(i+1).surface) || at(i+1).surface == "の" && meatFollows(at(i+2).surface)):
		return "とり", i + 1, true
	case t.surface == "牛" && (meatFollows(at(i+1).surface) || at(i+1).surface == "の" && meatFollows(at(i+2).surface)):
		return "ぎゅう", i + 1, true
	// 雨後雪 in a forecast is あめのちゆき; 雨後の筍 keeps うご.
	case t.surface == "雨後" && skyWord(at(i+1).surface):
		return "あめのち", i + 1, true
	// a weekday in brackets after a date: 9月17日（水） すい.
	case weekdayKanji[t.surface] != "" && isOpenBracket(at(i-1).surface) && isCloseBracket(at(i+1).surface):
		return weekdayKanji[t.surface], i + 1, true
	// 辛い on a cooking show is からい; つらい only before the words that make it hardship.
	case t.isPOS("形容詞") && strings.HasPrefix(t.surface, "辛") && strings.HasPrefix(t.reading, "ツラ") && !tsuraiNext[at(i+1).surface]:
		return "から" + hiragana(strings.TrimPrefix(t.reading, "ツラ")), i + 1, true
	// 寒気 is かんき (cold air) in a forecast; さむけ only when someone has the chills (寒気がする).
	case t.surface == "寒気":
		if p, q := at(i+1).surface, at(i+2).surface; (p == "が" || p == "も") && (strings.HasPrefix(q, "し") || strings.HasPrefix(q, "す")) {
			return "さむけ", i + 1, true
		}
		return "かんき", i + 1, true
	// 人気のない通り: ひとけ.
	case t.surface == "人気" && at(i+1).surface == "の" && (at(i+2).surface == "ない" || at(i+2).surface == "無い"):
		return "ひとけ", i + 1, true
	// 連休の中日: なかび.
	case t.surface == "中日" && at(i-1).surface == "の":
		return "なかび", i + 1, true
	// 中火の間: 間 after の is あいだ.
	case t.surface == "間" && t.reading == "マ" && at(i-1).surface == "の":
		return "あいだ", i + 1, true
	// rendaku after a noun: 行楽日和 びより, 貸切風呂 ぶろ, 海鮮丼 どん.
	case t.surface == "日和" && t.reading == "ヒヨリ" && at(i-1).isPOS("名詞"):
		return "びより", i + 1, true
	case t.surface == "風呂" && t.reading == "フロ" && at(i-1).isPOS("名詞") && !at(i-1).isPOS("名詞-接尾"):
		return "ぶろ", i + 1, true
	case t.surface == "丼" && t.reading == "ドンブリ" && at(i-1).isPOS("名詞"):
		return "どん", i + 1, true
	// 雨 / 雪 split off as a suffix (一時雨 → 雨/ウ) are still あめ / ゆき.
	case t.isPOS("名詞-接尾") && t.surface == "雨":
		return "あめ", i + 1, true
	case t.isPOS("名詞-接尾") && t.surface == "雪":
		return "ゆき", i + 1, true
	// 一晩水に, 3分油で, 焼き色, 炒め物: a suffix reading where the word stands on its own.
	case t.isPOS("名詞-接尾") && suffixKun[t.surface] != "":
		p := at(i - 1)
		if prev != nil || endsHiragana(p.surface) || (isKatakanaWord(p.surface) && (t.surface == "色" || t.surface == "物")) {
			return suffixKun[t.surface], i + 1, true
		}
	}
	return "", i, false
}
