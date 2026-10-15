package yomi

import (
	"strings"
	"unicode/utf8"
)

// number is a run of numeric tokens: an integer part and, when a decimal point followed, its digits.
type number struct {
	whole    int64   // integer part (the numerator when denom is set)
	decimals string  // ASCII digits after the decimal point, "" when there is none
	digits   string  // every digit as written (ASCII), read one by one when whole overflowed
	overflow bool    // the integer part does not fit maxWhole
	arabic   bool    // written in Arabic or full-width digits rather than kanji numerals
	bigUnit  int64   // a 万/億/兆 right after the decimals (3.5億): spoken after them
	bare     []int64 // big units with no digit before them (数万, 億万): read without いち
	juxta    []byte  // two or three kanji digits with no unit (二三日, 七五三): read digit by digit
	maru     int     // a run of 〇 standing for a blank (〇〇さん): read まる per character
	denom    int64   // fraction: whole over denom (1/2 → にぶんのいち)
	oneSen   bool    // 千 written with an explicit 一 (一千円 → いっせんえん)
	bareSen  uint8   // groups (bit 0 ones, 1 万, 2 億, 3 兆) whose 千 had no digit before it: せん, not いっせん
}

// maxWhole bounds the integer part: 9999兆 is as far as the readings go.
const maxWhole = 1_0000_0000_0000_0000

var (
	digitKana = [10]string{"れい", "いち", "に", "さん", "よん", "ご", "ろく", "なな", "はち", "きゅう"}
	// digitKanaSpelled is for digit-by-digit strings (overflow): ぜろ, and the one-syllable digits as in
	// phone numbers.
	digitKanaSpelled = [10]string{"ぜろ", "いち", "に", "さん", "よん", "ご", "ろく", "なな", "はち", "きゅう"}
	// juxtaKana reads kanji digits set side by side (二三日 にさんにち, 七五三 しちごさん, 四五人 しごにん).
	juxtaKana  = [10]string{"れい", "いち", "に", "さん", "し", "ご", "ろく", "しち", "はち", "く"}
	kanjiDigit = map[rune]int64{'〇': 0, '零': 0, '一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	kanjiSmall = map[rune]int64{'十': 10, '百': 100, '千': 1000}
	kanjiBig   = map[rune]int64{'万': 1_0000, '億': 1_0000_0000, '兆': 1_0000_0000_0000}
	bigKana    = map[int64]string{1_0000: "まん", 1_0000_0000: "おく", 1_0000_0000_0000: "ちょう"}
)

// scanNumber gathers the numeric tokens starting at i: Arabic or full-width digit runs, kanji numerals
// (positional digits and the units 十百千万億兆), a thousands separator between digits followed by exactly
// three digits, a decimal point between digits (after which only digits and one big unit are taken) and
// a slash between digits making a small fraction. It returns the value, the index of the first token
// not consumed and whether anything numeric was found.
func scanNumber(toks []token, i int) (number, int, bool) {
	var n number
	var acc accumulator
	seen := false
	afterDigits := false // the previous atom was a digit run (separators are only valid there)
	units := false       // a kanji unit (十百千万億兆) was seen
	kanjiDigits := 0     // kanji digits seen (〇 and 零 included)
	zeros := 0           // 〇 seen
	j := i
	// Big units with nothing numeric before them (数万人, 何億円, 億万長者) are read bare: まん, おく.
	for j < len(toks) && toks[j].numeral && isBigUnits(toks[j].surface) {
		for _, r := range toks[j].surface {
			n.bare = append(n.bare, kanjiBig[r])
			n.whole += kanjiBig[r]
		}
		j++
	}
	if len(n.bare) > 0 {
		return n, j, true
	}
	for j < len(toks) {
		t := toks[j]
		if _, ok := digitRun(t.surface); ok {
			var d string
			d, j = collectDigits(toks, j)
			acc.digits(&n, d)
			seen, afterDigits, n.arabic = true, true, true
			continue
		}
		if t.numeral && isKanjiNumeral(t.surface) {
			for _, r := range t.surface {
				if _, ok := kanjiDigit[r]; ok {
					kanjiDigits++
					if r == '〇' {
						zeros++
					}
				} else {
					units = true
				}
				acc.kanji(&n, r)
			}
			seen, afterDigits = true, false
			j++
			continue
		}
		if afterDigits && (t.surface == "." || t.surface == "．") && j+1 < len(toks) {
			if _, ok := digitRun(toks[j+1].surface); ok {
				n.decimals, j = collectDigits(toks, j+1)
				if j < len(toks) && toks[j].numeral && isBigUnits(toks[j].surface) && utf8.RuneCountInString(toks[j].surface) == 1 {
					r, _ := utf8.DecodeRuneInString(toks[j].surface)
					n.bigUnit = kanjiBig[r]
					j++
				}
				break
			}
		}
		if afterDigits && (t.surface == "," || t.surface == "，") && j+1 < len(toks) {
			if d, k := collectDigits(toks, j+1); len(d) == 3 {
				acc.digits(&n, d)
				j = k
				continue
			}
		}
		if afterDigits && (t.surface == "/" || t.surface == "／") && j+1 < len(toks) && !n.overflow {
			if d, k := collectDigits(toks, j+1); d != "" && len(d) <= 2 {
				var den int64
				for _, c := range d {
					den = den*10 + int64(c-'0')
				}
				num := acc.value(&n)
				if num > 0 && num < den && den <= 10 {
					n.denom = den
					j = k
					break
				}
			}
		}
		break
	}
	if !seen {
		return number{}, i, false
	}
	n.whole = acc.value(&n)
	if !n.arabic && !units && n.decimals == "" {
		switch {
		case zeros == kanjiDigits && zeros >= 2:
			n.maru = zeros
		case zeros == 0 && (kanjiDigits == 2 || kanjiDigits == 3):
			n.juxta = []byte(n.digits)
		}
	}
	return n, j, true
}

// accumulator builds an integer from Japanese numeral atoms: cur collects positional digits, section the
// part below the next big unit (万億兆), total everything already multiplied by a big unit.
type accumulator struct {
	total, section, cur int64
	curExplicit         bool // cur came from written digits (一千 rather than 千)
	bareSen             bool // the section's 千 had no digit before it (千五百万, 何千万)
}

// digits appends the ASCII digits d to the value being collected.
func (a *accumulator) digits(n *number, d string) {
	n.digits += d
	for i := 0; i < len(d); i++ {
		a.cur = n.add(n.mul(a.cur, 10), int64(d[i]-'0'))
	}
	a.curExplicit = true
}

// kanji takes one numeral kanji: a digit, a small unit (十百千) or a big unit (万億兆).
func (a *accumulator) kanji(n *number, r rune) {
	if v, ok := kanjiDigit[r]; ok {
		a.digits(n, string('0'+byte(v)))
		return
	}
	if u, ok := kanjiSmall[r]; ok {
		if a.cur == 0 {
			a.cur = 1
			if u == 1000 {
				a.bareSen = true
			}
		} else if u == 1000 && a.cur == 1 && a.curExplicit {
			n.oneSen = true
		}
		a.section = n.add(a.section, n.mul(a.cur, u))
		a.cur, a.curExplicit = 0, false
		return
	}
	if u, ok := kanjiBig[r]; ok {
		a.section = n.add(a.section, a.cur)
		if a.section == 0 {
			a.section = 1
		}
		a.total = n.add(a.total, n.mul(a.section, u))
		if a.bareSen {
			n.bareSen |= groupBit(u)
		}
		a.section, a.cur, a.curExplicit, a.bareSen = 0, 0, false, false
	}
}

func (a *accumulator) value(n *number) int64 {
	if a.bareSen {
		n.bareSen |= groupBit(1)
	}
	return n.add(a.total, n.add(a.section, a.cur))
}

// groupBit is the bareSen bit of the group a big unit (or 1 for the ones) closes.
func groupBit(unit int64) uint8 {
	switch unit {
	case 1_0000:
		return 1 << 1
	case 1_0000_0000:
		return 1 << 2
	case 1_0000_0000_0000:
		return 1 << 3
	}
	return 1
}

// isKanjiNumeral reports whether s is made only of numeral kanji.
func isKanjiNumeral(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if _, ok := kanjiDigit[r]; ok {
			continue
		}
		if _, ok := kanjiSmall[r]; ok {
			continue
		}
		if _, ok := kanjiBig[r]; ok {
			continue
		}
		return false
	}
	return true
}

// isBigUnits reports whether s is made only of 万, 億 and 兆.
func isBigUnits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if _, ok := kanjiBig[r]; !ok {
			return false
		}
	}
	return true
}

func (n *number) mul(a, b int64) int64 {
	if a != 0 && b > maxWhole/a {
		n.overflow = true
		return 0
	}
	return a * b
}

func (n *number) add(a, b int64) int64 {
	if a > maxWhole-b {
		n.overflow = true
		return 0
	}
	return a + b
}

// digitRun returns s as ASCII digits when it consists only of Arabic or full-width digits.
func digitRun(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= '０' && r <= '９':
			b.WriteRune(r - '０' + '0')
		default:
			return "", false
		}
	}
	return b.String(), true
}

// collectDigits concatenates the digit-run tokens starting at i (full-width digits come one per token)
// and returns the index after them.
func collectDigits(toks []token, i int) (string, int) {
	var b strings.Builder
	for i < len(toks) {
		d, ok := digitRun(toks[i].surface)
		if !ok {
			break
		}
		b.WriteString(d)
		i++
	}
	return b.String(), i
}

// isASCIILetters reports whether s is one or more ASCII letters.
func isASCIILetters(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// piece is one unit of a number reading: its kana and the unit it stands for (1..9 for a ones digit,
// 10, 100, 1000, 万, 億, 兆; 0 for れい and てん). The last piece of a reading is where a counter's sound
// change applies.
type piece struct {
	kana string
	unit int64
}

// intPieces reads a non-negative integer; oneSen reads a leading 1000 of the ones group as いっせん and
// bareSen marks the groups whose 千 stays せん (see number).
func intPieces(n int64, oneSen bool, bareSen uint8) []piece {
	if n == 0 {
		return []piece{{"れい", 0}}
	}
	var out []piece
	for _, g := range []struct {
		unit int64
		kana string
	}{{1_0000_0000_0000, "ちょう"}, {1_0000_0000, "おく"}, {1_0000, "まん"}, {1, ""}} {
		v := (n / g.unit) % 10000
		if v == 0 {
			continue
		}
		// いっせん before a big unit and after one (1万1千 いちまんいっせん); せん only when the number is
		// just the thousands (1000 せん, 3500 さんぜんごひゃく).
		gp := groupPieces(int(v), (g.unit > 1 || n >= 1_0000 || oneSen) && bareSen&groupBit(g.unit) == 0)
		if g.kana == "ちょう" {
			// いっちょう, はっちょう, じゅっちょう, ひゃっちょう
			switch last := &gp[len(gp)-1]; last.unit {
			case 1, 8, 10, 100:
				last.kana = sokuon(last.kana)
			}
		}
		out = append(out, gp...)
		if g.unit > 1 {
			out = append(out, piece{g.kana, g.unit})
		}
	}
	return out
}

// groupPieces reads 1..9999; with issen a leading 1000 is いっせん rather than せん.
func groupPieces(v int, issen bool) []piece {
	var out []piece
	if d := v / 1000; d > 0 {
		k := digitKana[d] + "せん"
		switch d {
		case 1:
			k = "せん"
			if issen {
				k = "いっせん"
			}
		case 3:
			k = "さんぜん"
		case 8:
			k = "はっせん"
		}
		out = append(out, piece{k, 1000})
	}
	if d := v / 100 % 10; d > 0 {
		k := digitKana[d] + "ひゃく"
		switch d {
		case 1:
			k = "ひゃく"
		case 3:
			k = "さんびゃく"
		case 6:
			k = "ろっぴゃく"
		case 8:
			k = "はっぴゃく"
		}
		out = append(out, piece{k, 100})
	}
	if d := v / 10 % 10; d > 0 {
		k := digitKana[d] + "じゅう"
		if d == 1 {
			k = "じゅう"
		}
		out = append(out, piece{k, 10})
	}
	if d := v % 10; d > 0 {
		out = append(out, piece{digitKana[d], int64(d)})
	}
	return out
}

// sokuon replaces the last kana of a piece with っ (いち → いっ, ろく → ろっ, じゅう → じゅっ, ひゃく → ひゃっ).
func sokuon(kana string) string {
	_, size := utf8.DecodeLastRuneInString(kana)
	return kana[:len(kana)-size] + "っ"
}

// numCtx is what a counter's context rule sees: the tokens, where the number starts, where the counter
// ends and the counter of the number written just before (9月 before 1日).
type numCtx struct {
	toks       []token
	start, end int
	prev       *counter
}

// next returns the token after the counter, or a zero token at the end of the text.
func (c numCtx) next(k int) (token, bool) {
	if c.end+k < len(c.toks) {
		return c.toks[c.end+k], true
	}
	return token{}, false
}

// before returns the token before the number, or a zero token at the start of the text.
func (c numCtx) before() (token, bool) {
	if c.start > 0 {
		return c.toks[c.start-1], true
	}
	return token{}, false
}

// listSeparator returns the spoken form of a separator between two numbers that share a counter
// (7、8日, 3～5人) and whether the token is one.
func listSeparator(s string) (string, bool) {
	switch s {
	case "、", "，", ",":
		return "、", true
	case "～", "〜", "~":
		return "から", true
	}
	return "", false
}

// readNumberAt reads the number starting at token i with its counter and returns the kana, the index
// after what was consumed and the counter used (nil for a plain number).
func readNumberAt(toks []token, i int, prev *counter) (string, int, *counter, bool) {
	n, next, ok := scanNumber(toks, i)
	if !ok {
		return "", i, nil, false
	}
	c, after := matchCounter(toks, next, n)
	if c == nil && n.arabic && next < len(toks) && isASCIILetters(toks[next].surface) {
		// digits glued to a Latin suffix the table does not know (3D, 4K, 2LDK): left as written
		var b strings.Builder
		for k := i; k <= next; k++ {
			b.WriteString(toks[k].surface)
		}
		return b.String(), next + 1, nil, true
	}
	// A small plain number before a separator and a number with a counter shares the counter (7、8日,
	// 3～5人, 10～20%); anything larger (1000億、1,500万円) is a number of its own.
	if c == nil && next < len(toks) && next == i+1 && n.whole < 100 && n.decimals == "" && n.denom == 0 && n.maru == 0 {
		if sep, ok := listSeparator(toks[next].surface); ok {
			if n2, next2, ok := scanNumber(toks, next+1); ok {
				if c2, after2 := matchCounter(toks, next2, n2); c2 != nil {
					var b strings.Builder
					switch {
					case !n.arabic && !n2.arabic && len(n2.juxta) == 0 && n.whole < 10 && n2.whole < 10 && n.decimals == "" && n2.decimals == "":
						// 二、三日: an approximate pair, read like 二三日
						n2.juxta = []byte(n2.digits)
						b.WriteString(juxtaKana[n.whole])
						b.WriteString(sep)
						b.WriteString(readNumber(n2, c2, numCtx{toks, next + 1, after2, nil}))
					default:
						// 7、8日 なのか、ようか; 3～5人 さんにんからごにん
						b.WriteString(readNumber(n, c2, numCtx{toks, i, after2, prev}))
						b.WriteString(sep)
						b.WriteString(readNumber(n2, c2, numCtx{toks, next + 1, after2, nil}))
					}
					return b.String(), after2, c2, true
				}
			}
		}
	}
	return readNumber(n, c, numCtx{toks, i, after, prev}), after, c, true
}

// readNumber reads n, with counter c when not nil.
func readNumber(n number, c *counter, ctx numCtx) string {
	base := ""
	if c != nil {
		base = c.base
	}
	if n.maru > 0 {
		return strings.Repeat("まる", n.maru) + base
	}
	plain := n.decimals == "" && !n.overflow && n.denom == 0 && len(n.bare) == 0 && len(n.juxta) == 0
	if c != nil && plain {
		if c.resolve != nil {
			if w, ok := c.resolve(n, ctx); ok {
				return w
			}
		}
		if w, ok := c.whole[n.whole]; ok {
			return w
		}
	}
	var pieces []piece
	switch {
	case len(n.bare) > 0:
		for _, u := range n.bare {
			pieces = append(pieces, piece{bigKana[u], u})
		}
	case len(n.juxta) > 0:
		for _, d := range n.juxta {
			pieces = append(pieces, piece{juxtaKana[d-'0'], int64(d - '0')})
		}
	case n.overflow && n.digits != "":
		for i := 0; i < len(n.digits); i++ {
			d := int64(n.digits[i] - '0')
			pieces = append(pieces, piece{digitKanaSpelled[d], d})
		}
	default:
		pieces = intPieces(n.whole, n.oneSen, n.bareSen)
	}
	if n.decimals != "" {
		last := &pieces[len(pieces)-1]
		switch last.unit {
		case 1, 8, 10:
			last.kana = sokuon(last.kana)
		}
		pieces = append(pieces, piece{"てん", 0})
		for i := 0; i < len(n.decimals); i++ {
			d := int64(n.decimals[i] - '0')
			pieces = append(pieces, piece{digitKana[d], d})
		}
		if n.bigUnit != 0 {
			pieces = append(pieces, piece{bigKana[n.bigUnit], n.bigUnit})
		}
	}
	var b strings.Builder
	if n.denom > 0 {
		for _, p := range intPieces(n.denom, false, 0) {
			b.WriteString(p.kana)
		}
		b.WriteString("ぶんの")
	}
	for _, p := range pieces[:len(pieces)-1] {
		b.WriteString(p.kana)
	}
	last := pieces[len(pieces)-1]
	if c == nil {
		b.WriteString(last.kana)
		return b.String()
	}
	if ch, ok := c.changes[last.unit]; ok {
		switch ch.piece {
		case "":
		case "っ":
			last.kana = sokuon(last.kana)
		default:
			last.kana = ch.piece
		}
		if ch.counter != "" {
			base = ch.counter
		}
	}
	b.WriteString(last.kana)
	b.WriteString(base)
	return b.String()
}
