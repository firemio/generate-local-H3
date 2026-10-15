// Package rundown decides what the channel airs next (port of aitv/rundown.py).
//
// The broadcast day is the lineup, slot by slot, looping: for each slot a *complete* programme made for it
// (all of its clips rendered) airs from start to end — a new one as soon as it exists, and once everything
// has aired the archive rotates, newer days more often — then a short break (a commercial or two and an
// ident). A slot that has no finished programme yet is filled with a block of the older short programmes of
// the same format, so the channel never stalls while a 6-minute programme is being rendered.
package rundown

import (
	"math/rand"
	"slices"
	"sort"
	"time"

	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/programs"
)

// FactFormats only air when they were written from real material (an Item with Sources).
var FactFormats = []string{"news", "weather", "variety"}

// FillerTargetS is how much of the old short programmes a filler block gathers.
const FillerTargetS = 60.0

// Programme is a group of clips that air together.
type Programme struct {
	ID      string
	Format  string
	Program string
	// Slot is the lineup slot the programme was made for (nil = none: breaks and the old library).
	Slot    *int
	Created string
	Seconds float64
	// Total is segments_total (0 for the old library, whose programmes are always complete).
	Total int
	Clips []index.Item
	// Complete: Total == 0 or len(Clips) >= Total.
	Complete bool
}

// Airable keeps everything except news/weather/variety without Sources (they were invented before the
// real sources existed; they stay on disk but off the rundown).
func Airable(items []index.Item) []index.Item {
	out := []index.Item{}
	for _, it := range items {
		if len(it.Sources) > 0 || !slices.Contains(FactFormats, it.Format) {
			out = append(out, it)
		}
	}
	return out
}

// Programmes groups clips into programmes: by ProgrammeID when the producer wrote one (first-seen order),
// else adjacent clips with the same Format and Program (the older one-script-per-clip library; a group is
// only extended while its Total is 0). Clips of a programme are sorted by (SegIndex, Created).
func Programmes(items []index.Item) []*Programme {
	out := []*Programme{}
	byID := map[string]*Programme{}
	for _, it := range items {
		if it.ProgrammeID != "" {
			p := byID[it.ProgrammeID]
			if p == nil {
				p = &Programme{ID: it.ProgrammeID, Format: it.Format, Program: it.Program, Slot: it.Slot,
					Created: it.Created, Total: it.SegmentsTotal}
				byID[it.ProgrammeID] = p
				out = append(out, p)
			}
			p.Clips = append(p.Clips, it)
			p.Seconds += it.Seconds
			continue
		}
		var last *Programme
		if len(out) > 0 {
			last = out[len(out)-1]
		}
		if last != nil && last.Total == 0 && last.Format == it.Format && last.Program == it.Program {
			last.Clips = append(last.Clips, it)
			last.Seconds += it.Seconds
		} else {
			id := it.ID
			if id == "" {
				id = it.File
			}
			out = append(out, &Programme{ID: id, Format: it.Format, Program: it.Program, Slot: nil,
				Created: it.Created, Seconds: it.Seconds, Total: 0, Clips: []index.Item{it}})
		}
	}
	for _, p := range out {
		sort.SliceStable(p.Clips, func(i, j int) bool {
			a, b := p.Clips[i], p.Clips[j]
			if a.SegIndex() != b.SegIndex() {
				return a.SegIndex() < b.SegIndex()
			}
			return a.Created < b.Created
		})
		p.Complete = p.Total == 0 || len(p.Clips) >= p.Total
	}
	return out
}

// Rundown is the per-broadcaster state: the current slot, what has aired this session, and recency.
type Rundown struct {
	rng    *rand.Rand
	lineup []string
	slot   int
	aired  map[string]bool // programme ids aired at least once this session
	recent []string        // programme ids, most recent last (capped at 80)
}

// New creates a rundown; rng nil = a fresh time-seeded source, lineup nil = programs.Lineup.
func New(rng *rand.Rand, lineup []string) *Rundown {
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if len(lineup) == 0 {
		lineup = programs.Lineup
	}
	return &Rundown{rng: rng, lineup: slices.Clone(lineup), aired: map[string]bool{}}
}

// pick chooses one programme of pool (nil when it is empty): with preferFresh, the newest programme not yet
// aired this session; otherwise a weighted random draw (newer = heavier) avoiding the most recent ones.
func (r *Rundown) pick(pool []*Programme, preferFresh bool) *Programme {
	if len(pool) == 0 {
		return nil
	}
	if preferFresh {
		var newest *Programme
		for _, p := range pool {
			if !r.aired[p.ID] && (newest == nil || p.Created > newest.Created) {
				newest = p
			}
		}
		if newest != nil {
			return newest
		}
	}
	k := max(1, min(len(pool)-1, len(pool)/2))
	avoid := map[string]bool{}
	for _, id := range r.recent[max(0, len(r.recent)-k):] {
		avoid[id] = true
	}
	cand := []*Programme{}
	for _, p := range pool {
		if !avoid[p.ID] {
			cand = append(cand, p)
		}
	}
	if len(cand) == 0 {
		cand = pool
	}
	ranked := slices.Clone(cand)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Created < ranked[j].Created })
	n := len(ranked)
	cum := make([]float64, n)
	total := 0.0
	for i := range ranked {
		total += 1.0 + float64(i)/float64(max(1, n-1))*2.0 // newer = heavier
		cum[i] = total
	}
	u := r.rng.Float64() * total
	for i, c := range cum {
		if u < c {
			return ranked[i]
		}
	}
	return ranked[n-1]
}

// take records p as aired and returns a copy of its clips.
func (r *Rundown) take(p *Programme) []index.Item {
	r.aired[p.ID] = true
	r.recent = append(r.recent, p.ID)
	if len(r.recent) > 80 {
		r.recent = r.recent[len(r.recent)-80:]
	}
	return slices.Clone(p.Clips)
}

// breakBlock is one or two distinct commercials (as many as exist) followed by one ident (when any exist).
func (r *Rundown) breakBlock(breaks []*Programme) []index.Item {
	clips := []index.Item{}
	var cms, idents []*Programme
	for _, p := range breaks {
		switch p.Format {
		case "cm":
			cms = append(cms, p)
		case "ident":
			idents = append(idents, p)
		}
	}
	n := 0
	if len(cms) > 0 {
		n = 1 + r.rng.Intn(2)
	}
	for i := 0; i < n && len(cms) > 0; i++ {
		cm := r.pick(cms, true)
		cms = without(cms, cm.ID)
		clips = append(clips, r.take(cm)...)
	}
	if len(idents) > 0 {
		clips = append(clips, r.take(r.pick(idents, true))...)
	}
	return clips
}

func without(pool []*Programme, id string) []*Programme {
	out := make([]*Programme, 0, len(pool))
	for _, p := range pool {
		if p.ID != id {
			out = append(out, p)
		}
	}
	return out
}

// NextBlock returns the ordered clips for the next slot of the day (programme + break), empty when nothing
// is complete. Selection (see aitv/rundown.py Rundown._pick/_take/_break/next_block):
//
//   - pick(pool, preferFresh): with preferFresh, the newest (by Created) programme not yet aired this session
//     wins; otherwise avoid the last max(1, min(len-1, len/2)) recent ids (fall back to the whole pool when
//     that empties everything), rank the rest by Created ascending and choose with weights
//     1 + i/(n-1)*2 (newer = heavier).
//   - a taken programme is recorded in aired and recent (recent capped at 80).
//   - break: 1 or 2 commercials (random, distinct, as many as exist), then one ident (when any exist).
//   - the slot advances (mod len(lineup)) on every call that finds a complete programme (as in the Python,
//     an empty library leaves the slot where it is). A full-length programme for this slot (Slot == slot
//     and Format == lineup[slot]) airs when one exists; otherwise a filler of short programmes of the format
//     (Slot == nil), or of any format with Slot == nil, or of anything, picked one by one until FillerTargetS
//     seconds are gathered.
func (r *Rundown) NextBlock(items []index.Item) []index.Item {
	var progs []*Programme
	for _, p := range Programmes(Airable(items)) {
		if p.Complete {
			progs = append(progs, p)
		}
	}
	if len(progs) == 0 {
		return []index.Item{}
	}
	var breaks, mains []*Programme
	for _, p := range progs {
		if slices.Contains(programs.BreakFormats, p.Format) {
			breaks = append(breaks, p)
		} else {
			mains = append(mains, p)
		}
	}
	slot := r.slot
	r.slot = (slot + 1) % len(r.lineup)
	fmtKey := r.lineup[slot]

	// 1. a full-length programme made for this slot: a new one airs first (newest first); once every one
	//    has aired, older days come back in the mix, newer ones more often — the archive is not wasted
	var full []*Programme
	for _, p := range mains {
		if p.Slot != nil && *p.Slot == slot && p.Format == fmtKey { // p["slot"] == slot
			full = append(full, p)
		}
	}
	if len(full) > 0 {
		return append(r.take(r.pick(full, true)), r.breakBlock(breaks)...)
	}

	// 2. nothing finished for this slot yet: a filler block of the short programmes of the same format
	//    (or of anything, when there are none), the usual fresh-first / weighted-random way
	var short, anyShort []*Programme
	for _, p := range mains {
		if p.Slot == nil {
			anyShort = append(anyShort, p)
			if p.Format == fmtKey {
				short = append(short, p)
			}
		}
	}
	if len(short) == 0 {
		short = anyShort
	}
	if len(short) == 0 {
		short = mains
	}
	clips := []index.Item{}
	seconds := 0.0
	pool := slices.Clone(short)
	for len(pool) > 0 && seconds < FillerTargetS {
		p := r.pick(pool, true)
		pool = without(pool, p.ID)
		clips = append(clips, r.take(p)...)
		seconds += p.Seconds
	}
	return append(clips, r.breakBlock(breaks)...)
}
