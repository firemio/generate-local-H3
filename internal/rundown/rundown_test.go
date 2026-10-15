package rundown

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/firemio/generate-local-H3/internal/index"
)

func intp(v int) *int { return &v }

// realIndex loads the real aitv/library/index.json, found by walking up from the package directory to the
// repository root (go test runs in the package directory, where paths.Root() cannot look).
func realIndex(t *testing.T) []index.Item {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			b, err := os.ReadFile(filepath.Join(dir, "aitv", "library", "index.json"))
			if err != nil {
				t.Skipf("real index.json not available: %v", err)
			}
			var items []index.Item
			if err := json.Unmarshal(b, &items); err != nil {
				t.Fatal(err)
			}
			return items
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("repository root not found")
		}
		dir = parent
	}
}

// goldenProgramme is one entry of testdata/programmes_golden.json, written by aitv.rundown (Python) for the
// first 1020 items of the real library.
type goldenProgramme struct {
	ID       string   `json:"id"`
	Format   string   `json:"format"`
	Program  string   `json:"program"`
	Slot     *int     `json:"slot"`
	Created  string   `json:"created"`
	Seconds  float64  `json:"seconds"`
	Total    int      `json:"total"`
	Clips    []string `json:"clips"`
	Complete bool     `json:"complete"`
}

type golden struct {
	Items             int               `json:"items"`
	FirstID           string            `json:"first_id"`
	LastID            string            `json:"last_id"`
	AirableIDs        []string          `json:"airable_ids"`
	ProgrammesAll     []goldenProgramme `json:"programmes_all"`
	ProgrammesAirable []goldenProgramme `json:"programmes_airable"`
}

func compareProgrammes(t *testing.T, name string, got []*Programme, want []goldenProgramme) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d programmes, want %d", name, len(got), len(want))
	}
	for i := 0; i < len(got) && i < len(want); i++ {
		g, w := got[i], want[i]
		ids := make([]string, len(g.Clips))
		for j, c := range g.Clips {
			ids[j] = c.ID
		}
		if g.ID != w.ID || g.Format != w.Format || g.Program != w.Program || g.Created != w.Created ||
			g.Total != w.Total || g.Complete != w.Complete || !reflect.DeepEqual(g.Slot, w.Slot) ||
			!reflect.DeepEqual(ids, w.Clips) || math.Abs(g.Seconds-w.Seconds) > 1e-9 {
			t.Errorf("%s[%d]: got {%s %s %q slot=%v %s %.6f total=%d clips=%v complete=%v}\n want {%s %s %q slot=%v %s %.6f total=%d clips=%v complete=%v}",
				name, i, g.ID, g.Format, g.Program, fmtSlot(g.Slot), g.Created, g.Seconds, g.Total, ids, g.Complete,
				w.ID, w.Format, w.Program, fmtSlot(w.Slot), w.Created, w.Seconds, w.Total, w.Clips, w.Complete)
		}
	}
}

func fmtSlot(s *int) any {
	if s == nil {
		return nil
	}
	return *s
}

// Python: aitv.rundown.airable / programmes on the first 1020 items of the real library (see testdata).
func TestGoldenRealIndex(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "programmes_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	items := realIndex(t)
	if len(items) < g.Items || items[0].ID != g.FirstID || items[g.Items-1].ID != g.LastID {
		t.Skipf("the real index.json no longer starts with the %d items the golden was computed on", g.Items)
	}
	items = items[:g.Items]

	air := Airable(items)
	ids := make([]string, len(air))
	for i, it := range air {
		ids[i] = it.ID
	}
	if !reflect.DeepEqual(ids, g.AirableIDs) {
		t.Errorf("Airable: %d items, want %d (ids differ)", len(ids), len(g.AirableIDs))
	}
	compareProgrammes(t, "Programmes(all)", Programmes(items), g.ProgrammesAll)
	compareProgrammes(t, "Programmes(airable)", Programmes(air), g.ProgrammesAirable)
}

func TestAirable(t *testing.T) {
	items := []index.Item{
		{ID: "n0", Format: "news"},
		{ID: "n1", Format: "news", Sources: []string{"h"}},
		{ID: "w0", Format: "weather", Sources: []string{}},
		{ID: "v0", Format: "variety"},
		{ID: "c0", Format: "cooking"},
		{ID: "cm", Format: "cm"},
	}
	got := Airable(items)
	want := []string{"n1", "c0", "cm"}
	ids := make([]string, len(got))
	for i, it := range got {
		ids[i] = it.ID
	}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("Airable = %v, want %v", ids, want)
	}
	if got := Airable(nil); got == nil || len(got) != 0 {
		t.Errorf("Airable(nil) = %#v", got)
	}
}

func TestProgrammesSynthetic(t *testing.T) {
	items := []index.Item{
		// old library: adjacent same format+program group; id falls back to file when empty
		{File: "a.mp4", Format: "news", Program: "N", Seconds: 6, Created: "2026-09-10T18:00:00"},
		{ID: "a2", File: "a2.mp4", Format: "news", Program: "N", Seconds: 6, Created: "2026-09-10T18:01:00"},
		{ID: "b", File: "b.mp4", Format: "news", Program: "M", Seconds: 6, Created: "2026-09-10T18:02:00"},
		// programme_id groups, interleaved; clips out of order
		{ID: "p1-1", Format: "cooking", Program: "C", ProgrammeID: "p1", Slot: intp(5), SegmentIndex: intp(1),
			SegmentsTotal: 3, Seconds: 7, Created: "2026-09-11T10:00:10"},
		{ID: "p2-0", Format: "cm", Program: "CM", ProgrammeID: "p2", SegmentIndex: intp(0), SegmentsTotal: 1,
			Seconds: 6.5, Created: "2026-09-11T10:00:20"},
		{ID: "p1-0", Format: "cooking", Program: "C", ProgrammeID: "p1", Slot: intp(5), SegmentIndex: intp(0),
			SegmentsTotal: 3, Seconds: 7, Created: "2026-09-11T10:00:30"},
		// a non-programme clip after a programme_id group with total > 0 starts its own group even when the
		// format and program match
		{ID: "c", File: "c.mp4", Format: "cooking", Program: "C", Seconds: 6, Created: "2026-09-11T10:01:00"},
		// same segment index twice: ordered by created
		{ID: "p3-b", Format: "nature", Program: "Na", ProgrammeID: "p3", Slot: intp(4), SegmentIndex: intp(0),
			SegmentsTotal: 2, Seconds: 8, Created: "2026-09-11T11:00:00"},
		{ID: "p3-a", Format: "nature", Program: "Na", ProgrammeID: "p3", Slot: intp(4), SegmentIndex: intp(0),
			SegmentsTotal: 2, Seconds: 8, Created: "2026-09-11T10:59:00"},
	}
	got := Programmes(items)
	type row struct {
		id, format, program string
		slot                *int
		created             string
		seconds             float64
		total               int
		clips               []string
		complete            bool
	}
	rows := make([]row, len(got))
	for i, p := range got {
		ids := make([]string, len(p.Clips))
		for j, c := range p.Clips {
			ids[j] = c.ID
			if c.ID == "" {
				ids[j] = c.File
			}
		}
		rows[i] = row{p.ID, p.Format, p.Program, p.Slot, p.Created, p.Seconds, p.Total, ids, p.Complete}
	}
	want := []row{
		{"a.mp4", "news", "N", nil, "2026-09-10T18:00:00", 12, 0, []string{"a.mp4", "a2"}, true},
		{"b", "news", "M", nil, "2026-09-10T18:02:00", 6, 0, []string{"b"}, true},
		{"p1", "cooking", "C", intp(5), "2026-09-11T10:00:10", 14, 3, []string{"p1-0", "p1-1"}, false},
		{"p2", "cm", "CM", nil, "2026-09-11T10:00:20", 6.5, 1, []string{"p2-0"}, true},
		{"c", "cooking", "C", nil, "2026-09-11T10:01:00", 6, 0, []string{"c"}, true},
		{"p3", "nature", "Na", intp(4), "2026-09-11T11:00:00", 16, 2, []string{"p3-a", "p3-b"}, true},
	}
	if !reflect.DeepEqual(rows, want) {
		for i := range rows {
			if i < len(want) && !reflect.DeepEqual(rows[i], want[i]) {
				t.Errorf("[%d] got  %+v\n    want %+v", i, rows[i], want[i])
			}
		}
		if len(rows) != len(want) {
			t.Errorf("%d programmes, want %d", len(rows), len(want))
		}
	}
	if got := Programmes(nil); got == nil || len(got) != 0 {
		t.Errorf("Programmes(nil) = %#v", got)
	}
}

// ---- NextBlock -------------------------------------------------------------------------------------------

// lib builds a synthetic library.
type lib struct{ items []index.Item }

// full adds a complete programme of n clips made for slot.
func (l *lib) full(pid, format string, slot int, n int, created string, secs float64) {
	for i := 0; i < n; i++ {
		l.items = append(l.items, index.Item{ID: pid + "-" + string(rune('a'+i)), File: pid + ".mp4", Format: format,
			Program: "P " + pid, ProgrammeID: pid, Slot: intp(slot), Section: "s", SegmentIndex: intp(i),
			SegmentsTotal: n, Seconds: secs, Created: created, Sources: []string{"real"}})
	}
}

// short adds an old-library single clip (no programme id, no slot).
func (l *lib) short(id, format string, created string, secs float64) {
	l.items = append(l.items, index.Item{ID: id, File: id + ".mp4", Format: format, Program: "P " + id,
		Seconds: secs, Created: created, Sources: []string{"real"}})
}

// brk adds a one-clip break programme (cm / ident).
func (l *lib) brk(pid, format string, created string) {
	l.items = append(l.items, index.Item{ID: pid + "-00", File: pid + ".mp4", Format: format, Program: "B " + pid,
		ProgrammeID: pid, Section: "CM", SegmentIndex: intp(0), SegmentsTotal: 1, Seconds: 6.5, Created: created})
}

func ids(clips []index.Item) []string {
	out := make([]string, len(clips))
	for i, c := range clips {
		out[i] = c.ID
	}
	return out
}

// splitBlock separates the main part of a block from its break and checks the break shape: 1–2 distinct
// commercials followed by exactly one ident (when the library has them).
func splitBlock(t *testing.T, block []index.Item, wantBreak bool) (mains []index.Item) {
	t.Helper()
	i := 0
	for i < len(block) && block[i].Format != "cm" && block[i].Format != "ident" {
		i++
	}
	mains, tail := block[:i], block[i:]
	if !wantBreak {
		if len(tail) != 0 {
			t.Errorf("unexpected break %v", ids(tail))
		}
		return mains
	}
	if len(tail) < 2 || len(tail) > 3 {
		t.Fatalf("break should be 1-2 cm + 1 ident, got %v", ids(tail))
	}
	cms := tail[:len(tail)-1]
	seen := map[string]bool{}
	for _, c := range cms {
		if c.Format != "cm" {
			t.Errorf("expected cm, got %s %s", c.Format, c.ID)
		}
		if seen[c.ProgrammeID] {
			t.Errorf("commercial %s aired twice in one break", c.ProgrammeID)
		}
		seen[c.ProgrammeID] = true
	}
	if last := tail[len(tail)-1]; last.Format != "ident" {
		t.Errorf("break must end with an ident, got %s %s", last.Format, last.ID)
	}
	return mains
}

func TestNextBlockEmpty(t *testing.T) {
	r := New(rand.New(rand.NewSource(1)), nil)
	if got := r.NextBlock(nil); got == nil || len(got) != 0 {
		t.Errorf("NextBlock(nil) = %#v", got)
	}
	// nothing complete: an unfinished programme and an invented news clip (no sources)
	var l lib
	l.items = append(l.items, index.Item{ID: "n", Format: "news", Program: "N", Seconds: 6, Created: "2026-09-10T18:00:00"})
	l.items = append(l.items, index.Item{ID: "u-a", Format: "cooking", Program: "U", ProgrammeID: "u", Slot: intp(5),
		SegmentIndex: intp(0), SegmentsTotal: 2, Seconds: 6, Created: "2026-09-10T18:00:00"})
	if got := r.NextBlock(l.items); len(got) != 0 {
		t.Errorf("NextBlock = %v, want empty", ids(got))
	}
	// as in the Python, an empty answer does not advance the slot
	if r.slot != 0 {
		t.Errorf("slot advanced to %d on an empty block", r.slot)
	}
	// breaks alone are not a programme: the break airs after a filler of... nothing (two commercials, so a
	// draw of two cannot hit the single-commercial crash path)
	l.brk("cm1", "cm", "2026-09-10T18:00:00")
	l.brk("cm2", "cm", "2026-09-10T18:00:01")
	l.brk("id1", "ident", "2026-09-10T18:00:00")
	got := r.NextBlock(l.items)
	splitBlock(t, got, true)
	if r.slot != 1 {
		t.Errorf("slot = %d after a block, want 1", r.slot)
	}
}

// A complete full-length programme for the slot beats the filler; the block is its clips in order + break.
func TestNextBlockFullProgrammeBeatsFiller(t *testing.T) {
	var l lib
	l.full("news1", "news", 0, 4, "2026-09-12T08:00:00", 7)
	l.full("wx1", "weather", 1, 3, "2026-09-12T08:10:00", 7)
	l.full("news7", "news", 7, 2, "2026-09-12T09:00:00", 7)
	l.full("newsX", "news", 3, 2, "2026-09-12T09:30:00", 7) // wrong slot for a news programme
	for i := 0; i < 12; i++ {
		l.short("sn"+string(rune('a'+i)), "news", "2026-09-10T18:0"+string(rune('0'+i%10))+":00", 6.5)
	}
	l.brk("cm1", "cm", "2026-09-10T18:00:00")
	l.brk("cm2", "cm", "2026-09-10T18:00:01")
	l.brk("id1", "ident", "2026-09-10T18:00:02")

	r := New(rand.New(rand.NewSource(1)), nil) // programs.Lineup: news, weather, variety, ..., news
	block := r.NextBlock(l.items)
	mains := splitBlock(t, block, true)
	if want := []string{"news1-a", "news1-b", "news1-c", "news1-d"}; !reflect.DeepEqual(ids(mains), want) {
		t.Errorf("slot 0: %v, want %v", ids(mains), want)
	}
	block = r.NextBlock(l.items)
	mains = splitBlock(t, block, true)
	if want := []string{"wx1-a", "wx1-b", "wx1-c"}; !reflect.DeepEqual(ids(mains), want) {
		t.Errorf("slot 1: %v, want %v", ids(mains), want)
	}
	// slot 2 (variety): nothing made for it, no variety shorts -> any short (the news shorts), >= 60 s
	block = r.NextBlock(l.items)
	mains = splitBlock(t, block, true)
	secs := 0.0
	for _, c := range mains {
		if c.Format != "news" || c.ProgrammeID != "" {
			t.Errorf("filler should be the short news clips, got %s %s", c.Format, c.ID)
		}
		secs += c.Seconds
	}
	if secs < FillerTargetS || secs-6.5 >= FillerTargetS {
		t.Errorf("filler gathered %.1f s, want the first total >= %.0f", secs, FillerTargetS)
	}
	if !r.aired["news1"] || !r.aired["wx1"] || r.aired["news7"] || r.aired["newsX"] {
		t.Errorf("aired = %v", r.aired)
	}
}

// Fresh first: the newest programme not yet aired this session wins; after all have aired, the rotation avoids
// the most recent ones.
func TestNextBlockFreshFirst(t *testing.T) {
	var l lib
	l.full("d1", "news", 0, 1, "2026-09-11T08:00:00", 7)
	l.full("d3", "news", 0, 1, "2026-09-13T08:00:00", 7)
	l.full("d2", "news", 0, 1, "2026-09-12T08:00:00", 7)
	l.brk("cm1", "cm", "2026-09-10T18:00:00")
	l.brk("cm2", "cm", "2026-09-10T18:00:01")
	l.brk("id1", "ident", "2026-09-10T18:00:02")
	r := New(rand.New(rand.NewSource(1)), []string{"news"}) // every call is slot 0
	var order []string
	for i := 0; i < 3; i++ {
		mains := splitBlock(t, r.NextBlock(l.items), true)
		order = append(order, mains[0].ProgrammeID)
	}
	if want := []string{"d3", "d2", "d1"}; !reflect.DeepEqual(order, want) {
		t.Errorf("fresh-first order = %v, want %v", order, want)
	}
	// all aired: the next pick avoids the last max(1, min(2, 1)) = 1 recent id (d1)
	for i := 0; i < 20; i++ {
		mains := splitBlock(t, r.NextBlock(l.items), true)
		last := r.recent[len(r.recent)-2]
		if mains[0].ProgrammeID == last {
			t.Errorf("round %d: %s repeated straight away", i, last)
		}
	}
	if n := len(r.recent); n < 23*3 || n > 80 { // 23 rounds: programme + 1-2 commercials + ident, capped at 80
		t.Errorf("recent has %d entries", n)
	}
}

// Break: 1 or 2 distinct commercials then one ident; with a single commercial it never repeats; with no
// commercials only the ident; with no idents only commercials.
func TestNextBlockBreakShapes(t *testing.T) {
	base := func() lib {
		var l lib
		l.full("d1", "news", 0, 1, "2026-09-11T08:00:00", 7)
		return l
	}
	rng := rand.New(rand.NewSource(1))

	l := base()
	l.brk("cm1", "cm", "2026-09-10T18:00:00")
	l.brk("cm2", "cm", "2026-09-10T18:00:01")
	l.brk("cm3", "cm", "2026-09-10T18:00:02")
	l.brk("id1", "ident", "2026-09-10T18:00:03")
	l.brk("id2", "ident", "2026-09-10T18:00:04")
	r := New(rng, []string{"news"})
	lens := map[int]int{}
	for i := 0; i < 40; i++ {
		block := r.NextBlock(l.items)
		splitBlock(t, block, true)
		lens[len(block)-1]++
	}
	if lens[2] == 0 || lens[3] == 0 {
		t.Errorf("break lengths never varied: %v", lens)
	}

	// A single commercial airs once whatever the draw.
	l = base()
	l.brk("cm1", "cm", "2026-09-10T18:00:00")
	l.brk("id1", "ident", "2026-09-10T18:00:03")
	for seed := int64(0); seed < 20; seed++ {
		r = New(rand.New(rand.NewSource(seed)), []string{"news"})
		block := r.NextBlock(l.items)
		if want := []string{"d1-a", "cm1-00", "id1-00"}; !reflect.DeepEqual(ids(block), want) {
			t.Fatalf("seed %d: single commercial: %v, want %v", seed, ids(block), want)
		}
	}

	l = base()
	l.brk("id1", "ident", "2026-09-10T18:00:03")
	r = New(rng, []string{"news"})
	if block := r.NextBlock(l.items); !reflect.DeepEqual(ids(block), []string{"d1-a", "id1-00"}) {
		t.Errorf("no commercials: %v", ids(block))
	}

	l = base()
	l.brk("cm1", "cm", "2026-09-10T18:00:00")
	l.brk("cm2", "cm", "2026-09-10T18:00:01")
	r = New(rng, []string{"news"})
	if block := r.NextBlock(l.items); len(block) < 2 || len(block) > 3 || block[1].Format != "cm" {
		t.Errorf("no idents: %v", ids(block))
	}

	l = base()
	r = New(rng, []string{"news"})
	if block := r.NextBlock(l.items); !reflect.DeepEqual(ids(block), []string{"d1-a"}) {
		t.Errorf("no breaks at all: %v", ids(block))
	}
}

// Filler: short programmes of the slot's format first (Slot == nil), else any short, else anything; gathered
// one by one until >= 60 s, fresh first.
func TestNextBlockFiller(t *testing.T) {
	var l lib
	for i := 0; i < 8; i++ {
		l.short("cook"+string(rune('a'+i)), "cooking", "2026-09-1"+string(rune('0'+i))+"T10:00:00", 10)
	}
	for i := 0; i < 8; i++ {
		l.short("nat"+string(rune('a'+i)), "nature", "2026-09-1"+string(rune('0'+i))+"T11:00:00", 10)
	}
	l.full("nat9", "nature", 4, 1, "2026-09-19T11:00:00", 100) // made for slot 4, not a short
	l.brk("cm1", "cm", "2026-09-10T18:00:00")
	l.brk("cm2", "cm", "2026-09-10T18:00:01")
	l.brk("id1", "ident", "2026-09-10T18:00:03")

	r := New(rand.New(rand.NewSource(1)), []string{"cooking"})
	mains := splitBlock(t, r.NextBlock(l.items), true)
	// fresh first, newest first: cookh, cookg, ... until 60 s -> six 10 s clips
	if want := []string{"cookh", "cookg", "cookf", "cooke", "cookd", "cookc"}; !reflect.DeepEqual(ids(mains), want) {
		t.Errorf("cooking filler = %v, want %v", ids(mains), want)
	}
	// second block: the two fresh ones first, then the rotation fills the rest (no format mixing while
	// cooking shorts exist)
	mains = splitBlock(t, r.NextBlock(l.items), true)
	if len(mains) != 6 || mains[0].ID != "cookb" || mains[1].ID != "cooka" {
		t.Errorf("second cooking filler = %v", ids(mains))
	}
	for _, c := range mains {
		if c.Format != "cooking" {
			t.Errorf("format mixed in: %s", c.ID)
		}
	}
	// a format with no shorts of its own: any short (never the slot-4 nature programme)
	r = New(rand.New(rand.NewSource(1)), []string{"travel"})
	mains = splitBlock(t, r.NextBlock(l.items), true)
	secs := 0.0
	for _, c := range mains {
		if c.ProgrammeID != "" {
			t.Errorf("full-length programme %s used as filler while shorts exist", c.ID)
		}
		secs += c.Seconds
	}
	if secs < FillerTargetS || secs-10 >= FillerTargetS {
		t.Errorf("gathered %.0f s", secs)
	}
	// no shorts at all: anything complete (the slot-4 nature programme), once it reaches 60 s
	var l2 lib
	l2.full("nat9", "nature", 4, 1, "2026-09-19T11:00:00", 100)
	l2.full("cook9", "cooking", 5, 1, "2026-09-19T12:00:00", 30)
	r = New(rand.New(rand.NewSource(1)), []string{"travel"})
	mains = splitBlock(t, r.NextBlock(l2.items), false)
	if want := []string{"cook9-a", "nat9-a"}; !reflect.DeepEqual(ids(mains), want) {
		t.Errorf("anything filler = %v, want %v", ids(mains), want)
	}
}

// The slot wraps around the lineup.
func TestNextBlockSlotWraps(t *testing.T) {
	var l lib
	l.full("a0", "news", 0, 1, "2026-09-11T08:00:00", 7)
	l.full("b1", "weather", 1, 1, "2026-09-11T08:00:00", 7)
	l.full("c2", "cooking", 2, 1, "2026-09-11T08:00:00", 7)
	r := New(rand.New(rand.NewSource(1)), []string{"news", "weather", "cooking"})
	var order []string
	for i := 0; i < 7; i++ {
		block := r.NextBlock(l.items)
		if len(block) != 1 {
			t.Fatalf("round %d: %v", i, ids(block))
		}
		order = append(order, block[0].ProgrammeID)
	}
	if want := []string{"a0", "b1", "c2", "a0", "b1", "c2", "a0"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if r.slot != 1 {
		t.Errorf("slot = %d, want 1", r.slot)
	}
	// nil lineup = programs.Lineup (8 slots), nil rng = time-seeded
	r = New(nil, nil)
	if len(r.lineup) != 8 || r.rng == nil {
		t.Errorf("New(nil, nil): %d slots, rng %v", len(r.lineup), r.rng)
	}
}

// pick without fresh candidates: weights 1 + i/(n-1)*2 over Created ascending (newer = heavier), avoiding the
// last max(1, min(n-1, n/2)) recent ids.
func TestPickWeights(t *testing.T) {
	pool := []*Programme{{ID: "old", Created: "2026-09-11"}, {ID: "mid", Created: "2026-09-12"}, {ID: "new", Created: "2026-09-13"}}
	r := New(rand.New(rand.NewSource(1)), nil)
	for _, p := range pool {
		r.aired[p.ID] = true
	}
	counts := map[string]int{}
	const n = 30000
	for i := 0; i < n; i++ {
		counts[r.pick(pool, true).ID]++
	}
	for id, want := range map[string]float64{"old": 1.0 / 6, "mid": 2.0 / 6, "new": 3.0 / 6} {
		if got := float64(counts[id]) / n; math.Abs(got-want) > 0.02 {
			t.Errorf("%s picked %.3f, want %.3f", id, got, want)
		}
	}
	// recent avoidance: with 3 in the pool the last 1 recent id is skipped
	r.recent = []string{"x", "new"}
	for i := 0; i < 200; i++ {
		if got := r.pick(pool, true).ID; got == "new" {
			t.Fatal("recent id picked")
		}
	}
	// with 5 in the pool the last 2 are skipped
	pool5 := append(pool, &Programme{ID: "p4", Created: "2026-09-14"}, &Programme{ID: "p5", Created: "2026-09-15"})
	r.aired["p4"], r.aired["p5"] = true, true
	r.recent = []string{"p5", "p4"}
	for i := 0; i < 200; i++ {
		if got := r.pick(pool5, true).ID; got == "p4" || got == "p5" {
			t.Fatal("recent id picked")
		}
	}
	// when avoidance empties the pool, the whole pool is used
	one := pool[:1]
	r.recent = []string{"old"}
	if got := r.pick(one, true); got == nil || got.ID != "old" {
		t.Errorf("pick(one) = %v", got)
	}
	if got := r.pick(nil, true); got != nil {
		t.Errorf("pick(nil) = %v", got)
	}
	// fresh: the newest not yet aired, first one on ties
	r.aired = map[string]bool{}
	tie := []*Programme{{ID: "t1", Created: "2026-09-13"}, {ID: "t2", Created: "2026-09-13"}, {ID: "t0", Created: "2026-09-12"}}
	if got := r.pick(tie, true).ID; got != "t1" {
		t.Errorf("fresh pick = %s, want t1", got)
	}
	// recent is capped at 80
	for i := 0; i < 100; i++ {
		r.take(pool[0])
	}
	if len(r.recent) != 80 {
		t.Errorf("recent = %d entries", len(r.recent))
	}
}
