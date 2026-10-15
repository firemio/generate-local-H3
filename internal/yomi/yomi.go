// Package yomi turns the Japanese dialogue of an H3 prompt into kana so the model reads it right.
//
// H3 infers the reading of kanji from context and gets heteronyms wrong often enough to hear
// (火加減 read かかげん instead of ひかげん). The writer keeps writing natural kanji text — that is what
// the scripts store and what the ticker shows — and the producer converts the <d>[Japanese] ... </d>
// bodies to hiragana right before the render, with the cast's own readings for their names.
//
// Tokenisation and dictionary readings come from kagome (github.com/ikawaha/kagome/v2) with the IPA
// dictionary. Rules on top of the dictionary:
//
//   - only tokens that contain kanji are converted; hiragana, katakana (loanwords), ASCII and
//     punctuation keep their surface, so は / へ / を stay as written and カメラ stays katakana. A
//     loanword inside a dictionary compound stays katakana too (生クリーム → なまクリーム, テレビ局 →
//     テレビきょく);
//   - a name from the cast (Names) is read as given wherever it stands as a word of its own, family
//     and given name separately (佐藤 美咲 → さとう / みさき). A name is matched against the tokeniser's
//     word boundaries, never inside another word (a cast member 東 leaves 東京 alone). A cast name
//     without a reading is left as kanji rather than guessed (dictionary readings of given names are
//     wrong far more often than H3's own);
//   - numbers written in Arabic digits or kanji numerals are read with their counter: 3分 さんぷん,
//     9月17日 くがつじゅうしちにち, 4時 よじ, 28℃ にじゅうはちど, 2人 ふたり, 3本 さんぼん, 10回 じゅっかい,
//     28.5度 にじゅうはってんごど, 1万人 いちまんにん, 30% さんじゅっパーセント, 3.5億円 さんてんごおくえん,
//     数万人 すうまんにん, 1/2本 にぶんのいっぽん, 3～5人 さんにんからごにん, 7、8日 なのか、ようか. Days of
//     the month and the 4/7/9 readings of hours and months follow the standard readings (ついたち…とおか,
//     じゅうよっか, はつか, にじゅうよっか; よじ, しちじ, くじ; しがつ, しちがつ, くがつ); 1日 is ついたち only
//     as a calendar date (9月1日, 1日午前, 1日、) and いちにち as a length of time (1日3回, 1日おき, 一日).
//     Zero is れい (午前0時 ごぜんれいじ, 0.5度 れいてんごど); a run of 〇 used as a blank is まる (〇〇さん).
//     Native counters take ひと/ふた for 1/2 (一袋 ひとふくろ, 2組 ふたくみ), 20歳 is はたち, and 何 before a
//     counter is なん with the counter's sound change (何分 なんぷん, 何本 なんぼん). A number followed by
//     no known counter is read as a plain number (にじゅうはち); a number followed by a token that is not
//     in the counter table keeps that token's dictionary reading; digits glued to an unknown Latin
//     suffix are left as written (3D, 4K), except the English ordinals 1st/2nd/3rd (ファースト…);
//   - a small table of broadcast readings overrides the dictionary where it picks the wrong homograph
//     for a weather, cooking or travel word (雨が降ります ふります, 辛い からい, 鶏もも肉 とりももにく,
//     晴れ後曇り のち, 寒気 かんき, 港町 みなとまち, 干物 ひもの); see override.go. Words the dictionary
//     reads defensibly either way (明日, 市場, 日本人) are left to it — write them in kana when it matters.
//
// The conversion is applied only inside the dialogue tags; the rest of the prompt (the English scene
// description) is untouched.
package yomi

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
)

// Names maps a written name part to its hiragana reading (e.g. "佐藤" → "さとう", "美咲" → "みさき").
// An empty reading means "no reading known": the part is left exactly as written instead of being
// read from the dictionary.
type Names map[string]string

// Reader converts text. Create one with New (the dictionary takes a moment to load) and reuse it.
type Reader struct {
	mu  sync.Mutex
	tok *tokenizer.Tokenizer
}

// New loads the IPA dictionary and returns a Reader.
func New() (*Reader, error) {
	t, err := tokenizer.New(ipa.Dict(), tokenizer.OmitBosEos())
	if err != nil {
		return nil, err
	}
	return &Reader{tok: t}, nil
}

// tokenKind tells the conversion loop how a token came about.
type tokenKind uint8

const (
	tokPlain     tokenKind = iota
	tokName                // a cast name with a reading: reading is already hiragana
	tokProtected           // a cast name without a reading: the surface is emitted as written
)

// token is the part of a kagome token the conversion needs.
type token struct {
	surface string
	reading string // katakana reading, "" when the dictionary has none ("*")
	pos     string // part of speech, the first three levels joined by "-" (名詞-接尾-助数詞)
	numeral bool   // part of speech 名詞,数
	kind    tokenKind
}

// isPOS reports whether the token's part of speech starts with prefix (e.g. "名詞-接尾").
func (t token) isPOS(prefix string) bool {
	return t.pos == prefix || strings.HasPrefix(t.pos, prefix+"-")
}

// tokenize runs kagome under the lock; the tokens are plain values afterwards.
func (r *Reader) tokenize(text string) []token {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw := r.tok.Tokenize(text)
	toks := make([]token, 0, len(raw))
	for _, t := range raw {
		tk := token{surface: t.Surface}
		if f, ok := t.FeatureAt(ipa.Reading); ok && f != "*" {
			tk.reading = f
		}
		pos := t.POS()
		if len(pos) > 3 {
			pos = pos[:3]
		}
		var kept []string
		for _, p := range pos {
			if p == "*" {
				break
			}
			kept = append(kept, p)
		}
		tk.pos = strings.Join(kept, "-")
		tk.numeral = len(pos) >= 2 && pos[0] == "名詞" && pos[1] == "数"
		toks = append(toks, tk)
	}
	return toks
}

// Kana converts one Japanese sentence to kana per the package rules.
func (r *Reader) Kana(text string, names Names) string {
	if text == "" {
		return text
	}
	text = applyTextOverrides(text)
	toks := markNames(splitGlued(r.tokenize(text)), names)
	var b strings.Builder
	b.Grow(len(text) * 2)
	var prev *counter // the counter of the number written just before, nil after anything else
	for i := 0; i < len(toks); {
		t := toks[i]
		switch t.kind {
		case tokName:
			b.WriteString(t.reading)
			i++
			prev = nil
			continue
		case tokProtected:
			b.WriteString(t.surface)
			i++
			prev = nil
			continue
		}
		if kana, n := matchOverride(toks, i); n > 0 {
			b.WriteString(kana)
			i += n
			prev = nil
			continue
		}
		if s, next, c, ok := readNumberAt(toks, i, prev); ok {
			b.WriteString(s)
			i = next
			prev = c
			continue
		}
		if s, next, ok := applyContext(toks, i, prev); ok {
			b.WriteString(s)
			i = next
			prev = nil
			continue
		}
		i++
		prev = nil
		if !hasKanji(t.surface) {
			b.WriteString(t.surface)
			continue
		}
		if t.reading == "" {
			if k, ok := kanjiFallback[t.surface]; ok {
				b.WriteString(k)
			} else {
				b.WriteString(t.surface)
			}
			continue
		}
		b.WriteString(readingKana(t.surface, t.reading))
	}
	return b.String()
}

// reDialogue matches one <d>[Japanese] ... </d>: the tag with the whitespace after it, the body and
// the closing tag.
var reDialogue = regexp.MustCompile(`(?s)(<d>\[Japanese\][\s\p{Z}\x{85}]*)(.*?)(</d>)`)

// Dialogue rewrites every <d>[Japanese] ... </d> body of an H3 prompt with Kana and returns the prompt;
// everything outside the tags is returned unchanged. The tag itself is kept verbatim.
func (r *Reader) Dialogue(prompt string, names Names) string {
	return reDialogue.ReplaceAllStringFunc(prompt, func(m string) string {
		sub := reDialogue.FindStringSubmatch(m)
		return sub[1] + r.Kana(sub[2], names) + sub[3]
	})
}

// CastNames builds Names from cast entries: each entry's name is split on spaces (ASCII or ideographic)
// into parts and each part maps to the corresponding part of its kana (also split on spaces). An entry
// whose kana is empty or whose part counts differ contributes its parts with an empty reading, so the
// name stays kanji (Kana leaves such a part as written instead of reading it from the dictionary). A
// reading given by one entry is never blanked by another entry without one.
func CastNames(names, kanas []string) Names {
	out := Names{}
	for i, name := range names {
		parts := strings.Fields(name)
		if len(parts) == 0 {
			continue
		}
		var kana []string
		if i < len(kanas) {
			kana = strings.Fields(kanas[i])
		}
		if len(kana) != len(parts) {
			kana = make([]string, len(parts))
		}
		for j, p := range parts {
			if cur, ok := out[p]; !ok || cur == "" {
				out[p] = kana[j]
			}
		}
	}
	return out
}

// markNames merges every run of tokens whose surfaces spell a cast name into one name token (or a
// protected token when the name has no reading). Names are tried longest first at every token, and a
// name only matches where the tokeniser put a word boundary at both ends, so a name that is also an
// ordinary morpheme (東, 明, 学, 林) never changes the words that contain it (東京, 明日, 学校, 林檎).
func markNames(toks []token, names Names) []token {
	if len(names) == 0 {
		return toks
	}
	keys := make([]string, 0, len(names))
	for k := range names {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return toks
	}
	sort.Slice(keys, func(i, j int) bool {
		ni, nj := utf8.RuneCountInString(keys[i]), utf8.RuneCountInString(keys[j])
		if ni != nj {
			return ni > nj
		}
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	out := make([]token, 0, len(toks))
	for i := 0; i < len(toks); {
		matched := false
		for _, k := range keys {
			n := spanFor(toks, i, k)
			if n == 0 {
				continue
			}
			nt := token{surface: k, kind: tokProtected}
			if reading := names[k]; reading != "" {
				nt.kind, nt.reading = tokName, reading
			}
			out = append(out, nt)
			i += n
			matched = true
			break
		}
		if !matched {
			out = append(out, toks[i])
			i++
		}
	}
	return out
}

// spanFor returns how many tokens from i spell exactly s, or 0 when the token boundaries do not fit.
func spanFor(toks []token, i int, s string) int {
	for j := i; j < len(toks); j++ {
		if !strings.HasPrefix(s, toks[j].surface) {
			return 0
		}
		s = s[len(toks[j].surface):]
		if s == "" {
			return j - i + 1
		}
	}
	return 0
}

// hasKanji reports whether s contains a Han character (々 and 〆 included).
func hasKanji(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// isKatakana reports whether r is in the katakana block (ー and ・ included).
func isKatakana(r rune) bool {
	return r >= 0x30A0 && r <= 0x30FF
}

// hiragana converts the katakana of s (U+30A1..U+30F6) to hiragana; ー and everything else are kept.
func hiragana(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x30A1 && r <= 0x30F6 {
			r -= 0x60
		}
		b.WriteRune(r)
	}
	return b.String()
}

// readingKana converts a dictionary reading to hiragana while keeping the katakana that the surface
// itself is written with: 生クリーム/ナマクリーム → なまクリーム, テレビ局/テレビキョク → テレビきょく,
// バス停/バステイ → バスてい. Each katakana run of the surface is looked for, in order, in the reading;
// when one is not there the whole reading is converted as usual.
func readingKana(surface, reading string) string {
	if !strings.ContainsFunc(surface, isKatakana) {
		return hiragana(reading)
	}
	var b strings.Builder
	rest := reading
	for _, run := range katakanaRuns(surface) {
		idx := strings.Index(rest, run)
		if idx < 0 {
			return hiragana(reading)
		}
		b.WriteString(hiragana(rest[:idx]))
		b.WriteString(run)
		rest = rest[idx+len(run):]
	}
	b.WriteString(hiragana(rest))
	return b.String()
}

// katakanaRuns returns the maximal runs of katakana in s, in order.
func katakanaRuns(s string) []string {
	var runs []string
	start := -1
	for i, r := range s {
		if isKatakana(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			runs = append(runs, s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		runs = append(runs, s[start:])
	}
	return runs
}
