package rtmp

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tsPkt builds one 188-byte TS packet: pid, payload_unit_start, random_access_indicator (in an adaptation
// field that pads the packet), payload (stuffed with 0xFF when there is no adaptation field).
func tsPkt(pid int, pusi, rai bool, payload []byte) []byte {
	p := make([]byte, tsPacket)
	p[0] = 0x47
	p[1] = byte(pid >> 8)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	off := 4
	if rai {
		p[3] = 0x30 // adaptation field + payload
		alen := tsPacket - 4 - 1 - len(payload)
		p[4] = byte(alen)
		p[5] = 0x40 // random access indicator
		for i := 6; i < 5+alen; i++ {
			p[i] = 0xFF
		}
		off = 5 + alen
	} else {
		p[3] = 0x10 // payload only
		for i := 4 + len(payload); i < tsPacket; i++ {
			p[i] = 0xFF
		}
	}
	copy(p[off:], payload)
	return p
}

// pesHead is a PES header carrying a PTS (90 kHz), followed by a few bytes of elementary stream.
func pesHead(sid byte, pts int64) []byte {
	h := []byte{0, 0, 1, sid, 0, 0, 0x80, 0x80, 5,
		0x21 | byte(pts>>29)&0x0E, byte(pts >> 22), 0x01 | byte(pts>>14)&0xFE, byte(pts >> 7), 0x01 | byte(pts<<1)&0xFE}
	return append(h, 0, 0, 0, 1, 0x09, 0xF0)
}

func TestTSVideo(t *testing.T) {
	const video, audio, pmt = 256, 257, 4096
	var seg []byte
	add := func(p []byte) { seg = append(seg, p...) }
	add(tsPkt(0, true, false, []byte{0, 0, 0xB0, 13}))      // PAT
	add(tsPkt(pmt, true, false, []byte{0, 2, 0xB0, 20}))    // PMT
	add(tsPkt(video, true, true, pesHead(0xE0, 90000*100))) // packet 2: keyframe at the start
	add(tsPkt(video, false, false, []byte{1, 2, 3}))
	add(tsPkt(audio, true, true, pesHead(0xC0, 90000*100))) // audio frames all carry the indicator
	add(tsPkt(video, true, false, pesHead(0xE0, 90000*100+3750)))
	add(tsPkt(video, false, false, []byte{4, 5, 6}))
	add(tsPkt(0, true, false, []byte{0, 0, 0xB0, 13})) // PAT/PMT again before the clip boundary
	add(tsPkt(pmt, true, false, []byte{0, 2, 0xB0, 20}))
	add(tsPkt(video, true, true, pesHead(0xE0, 90000*100+90000/2))) // packet 9: the boundary keyframe
	add(tsPkt(video, false, false, []byte{7, 8, 9}))
	add(tsPkt(video, true, false, pesHead(0xE0, 90000*100+90000/2+3750)))
	add(tsPkt(video, true, true, []byte{0, 0, 1, 0xE0, 0, 0, 0x80, 0x00, 0})) // keyframe without a PTS: skipped
	add(tsPkt(video, true, false, pesHead(0xE0, 90000*100+90000/2+7500)))     // packet 13
	add(tsPkt(video, true, true, pesHead(0xE0, 90000*100+90000)))             // packet 14
	add([]byte{0x47, 1, 2})                                                   // truncated tail

	got := tsVideo(seg)
	want := []videoPES{{2 * tsPacket, 0, true}, {5 * tsPacket, 3750.0 / 90000, false}, {9 * tsPacket, 0.5, true},
		{11 * tsPacket, 0.5 + 3750.0/90000, false}, {13 * tsPacket, 0.5 + 7500.0/90000, false}, {14 * tsPacket, 1, true}}
	if len(got) != len(want) {
		t.Fatalf("video PES = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("video PES %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The write for the boundary at index 2 may pause just past the first packet of PES index 2+flushFrames.
	if off := flushOffset(got, 2, len(seg)); off != 14*tsPacket+tsPacket {
		t.Errorf("flushOffset(2) = %d, want past packet 14 (%d)", off, 15*tsPacket)
	}
	if off := flushOffset(got, 0, len(seg)); off != 11*tsPacket+tsPacket {
		t.Errorf("flushOffset(0) = %d, want past packet 11", off)
	}
	if off := flushOffset(got, 4, len(seg)); off != len(seg) {
		t.Errorf("flushOffset near the end = %d, want the segment's end %d", off, len(seg))
	}
	// The 33-bit PTS wraps between the first frame and the keyframe.
	seg3 := append([]byte(nil), tsPkt(video, true, true, pesHead(0xE0, 1<<33-90000))...)
	seg3 = append(seg3, tsPkt(video, true, true, pesHead(0xE0, 90000))...)
	if got := tsVideo(seg3); len(got) != 2 || got[1].PTS != 2 {
		t.Errorf("pts wrap: %+v", got)
	}
	// Lost sync: nothing after it is trusted.
	seg4 := append(append([]byte(nil), seg[:3*tsPacket]...), make([]byte, tsPacket)...)
	seg4 = append(seg4, seg[9*tsPacket:]...)
	if got := tsVideo(seg4); len(got) != 1 {
		t.Errorf("after a bad sync byte: %+v", got)
	}
	if got := tsVideo(nil); got != nil {
		t.Errorf("empty: %+v", got)
	}
}

// fakeSource is a source whose playlist, segments, arrival times and now.json entries are set by the test.
type fakeSource struct {
	text    string
	segs    map[string][]byte
	mtime   map[string]time.Time // arrival when set (a file source); nil = unknown (HTTP)
	entries []OnAir
	changed bool
}

func (s *fakeSource) playlist() (string, error) { return s.text, nil }
func (s *fakeSource) segment(uri string) ([]byte, error) {
	if b, ok := s.segs[uri]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (s *fakeSource) arrival(uri string) (time.Time, bool) {
	if s.mtime == nil {
		return time.Time{}, false
	}
	t, ok := s.mtime[uri]
	return t, ok
}
func (s *fakeSource) now() ([]OnAir, bool, error) {
	c := s.changed
	s.changed = false
	return s.entries, c, nil
}
func (s *fakeSource) String() string { return "fake" }

// station models the live station: segments 10..16 of 2 s (seg 15 is 2.125 s), fed from base on, with
// EXT-X-PROGRAM-DATE-TIME running `ahead` of the wall clock (9.7 s on the station after a day). Clip B's
// first frame is seg 12's first frame; clip C's first frame is the keyframe 0.125 s (frame 3) into seg 15.
type station struct {
	text   string
	mtime  map[string]time.Time // when each segment was finished (= its start + EXTINF)
	segs   map[string][]byte
	starts map[string]time.Time // feed time of each segment's first frame
}

func newStation(base time.Time, ahead time.Duration) station {
	st := station{mtime: map[string]time.Time{}, segs: map[string][]byte{}, starts: map[string]time.Time{}}
	durs := []float64{2, 2, 2, 2, 2, 2.125, 2}
	feed := base
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:10\n")
	for i, d := range durs {
		uri := "seg" + strconv.Itoa(10+i) + ".ts"
		b.WriteString("#EXTINF:" + strconv.FormatFloat(d, 'f', 6, 64) + ",\n")
		b.WriteString("#EXT-X-PROGRAM-DATE-TIME:" + feed.Add(ahead).Format("2006-01-02T15:04:05.000-0700") + "\n" + uri + "\n")
		end := feed.Add(time.Duration(d * float64(time.Second)))
		pts := 90000 * int64(100+2*i)
		var seg []byte
		for k := 0; k < 8; k++ { // 8 frames, one TS packet each (a boundary keyframe 3 frames into the 2.125 s one)
			key := k == 0 || (d == 2.125 && k == 3)
			seg = append(seg, tsPkt(256, true, key, pesHead(0xE0, pts+3750*int64(k)))...)
		}
		st.segs[uri], st.mtime[uri], st.starts[uri] = seg, end, feed
		feed = end
	}
	st.text = b.String()
	return st
}

func TestFeedStartAndParts(t *testing.T) {
	base := time.Date(2026, 9, 18, 13, 12, 0, 0, time.Local)
	ahead := 9700 * time.Millisecond
	st := newStation(base, ahead)
	sec := func(d time.Duration) float64 { return unixSeconds(base.Add(d)) }
	// now.json: A well before, B fed 0.15 s before seg 12's first frame, C 0.2 s before the keyframe 0.125 s
	// into seg 15 (feed times: seg 12 at +4 s, seg 15 at +10 s).
	entries := []OnAir{
		{T: sec(-6 * time.Second), ID: "A"},
		{T: sec(4*time.Second - 150*time.Millisecond), ID: "B"},
		{T: sec(10*time.Second + 125*time.Millisecond - 200*time.Millisecond), ID: "C"},
	}
	segs := ParsePlaylist(st.text)
	if len(segs) != 7 || segs[2].URI != "seg12.ts" || segs[5].Duration != 2.125 {
		t.Fatalf("station playlist: %+v", segs)
	}
	var log logBuf

	t.Run("file source: mtime is the truth", func(t *testing.T) {
		src := &fakeSource{text: st.text, segs: st.segs, mtime: st.mtime, entries: entries, changed: true}
		f := &follower{src: src, log: log.f}
		f.entries = entries
		f.observe(segs, base.Add(15*time.Second))
		for _, s := range segs {
			if got, want := f.feedStart(s), st.starts[s.URI]; !got.Equal(want) {
				t.Errorf("feedStart(%s) = %s, want %s", s.URI, got.Format("15:04:05.000"), want.Format("15:04:05.000"))
			}
		}
		// Keyed on the date-time, seg 11 and seg 12..14 would all show B (date-time 9.7 s ahead of t);
		// keyed on feed time, seg 11 shows A and seg 12 B.
		want := map[string][]string{"seg10.ts": {"A"}, "seg11.ts": {"A"}, "seg12.ts": {"B"}, "seg13.ts": {"B"},
			"seg14.ts": {"B"}, "seg15.ts": {"B", "C"}, "seg16.ts": {"C"}}
		for _, s := range segs {
			plan := f.parts(s, st.segs[s.URI], f.feedStart(s))
			var ids []string
			for _, p := range plan {
				ids = append(ids, entries[p.idx].ID)
			}
			if strings.Join(ids, ",") != strings.Join(want[s.URI], ",") {
				t.Errorf("parts(%s) = %v (%+v), want %v", s.URI, ids, plan, want[s.URI])
			}
			if s.URI == "seg15.ts" && (len(plan) != 2 || plan[1].Off != 3*tsPacket || plan[1].Flush != 7*tsPacket) {
				t.Errorf("seg15: the boundary is frame 3, the text goes in past the first packet of frame 6: %+v", plan)
			}
			if plan[0].Off != 0 || plan[0].Flush != 4*tsPacket {
				t.Errorf("%s: the start's text goes in past the first packet of frame 3: %+v", s.URI, plan)
			}
		}
		// A 0.583 s segment holding the old clip's last GOP right before the new clip: the new clip's t falls
		// 0.43 s after the segment's first frame and must not claim it (it starts with the next segment).
		withD := append(append([]OnAir(nil), entries...), OnAir{T: sec(16*time.Second + 583*time.Millisecond - 150*time.Millisecond), ID: "D"})
		if i := onAirIndex(withD, base.Add(16*time.Second)); i != 2 {
			t.Errorf("start of a 0.583 s segment fed at +16 s: entry %d, want C (2)", i)
		}
		if i := onAirIndex(withD, base.Add(16*time.Second+583*time.Millisecond)); i != 3 {
			t.Errorf("start of the next segment: entry %d, want D (3)", i)
		}
	})

	t.Run("http source: listing times calibrate the date-time", func(t *testing.T) {
		src := &fakeSource{text: st.text, segs: st.segs, entries: entries, changed: true}
		f := &follower{src: src, log: log.f}
		f.entries = entries
		// Poll 1 lists seg 10..16 right after seg 16 was finished (feed +14 s), 0.3 s late.
		f.observe(segs, st.mtime["seg16.ts"].Add(300*time.Millisecond))
		if off, ok := f.offset(); !ok || off < ahead-400*time.Millisecond || off > ahead {
			t.Errorf("offset after the first listing = %v, want ~%v (a little less)", off, ahead)
		}
		for _, s := range segs {
			got, want := f.feedStart(s), st.starts[s.URI]
			if d := got.Sub(want); d < 0 || d > 400*time.Millisecond {
				t.Errorf("feedStart(%s) = %s, want %s (+0..0.4 s)", s.URI, got.Format("15:04:05.000"), want.Format("15:04:05.000"))
			}
		}
		// A later segment listed with only 50 ms latency tightens the estimate; the plan is the same.
		st2 := newStation(base, ahead)
		text := st2.text + "#EXTINF:2.000000,\n#EXT-X-PROGRAM-DATE-TIME:" +
			st2.mtime["seg16.ts"].Add(ahead).Format("2006-01-02T15:04:05.000-0700") + "\nseg17.ts\n"
		segs2 := ParsePlaylist(text)
		f.observe(segs2, st2.mtime["seg16.ts"].Add(2*time.Second+50*time.Millisecond))
		if off, _ := f.offset(); off < ahead-60*time.Millisecond || off > ahead {
			t.Errorf("offset after a prompt listing = %v, want within 60 ms of %v", off, ahead)
		}
		plan := f.parts(segs[5], st.segs["seg15.ts"], f.feedStart(segs[5]))
		if len(plan) != 2 || entries[plan[0].idx].ID != "B" || entries[plan[1].idx].ID != "C" {
			t.Errorf("seg15 over HTTP: %+v", plan)
		}
		// Without a date-time, the listing time less EXTINF stands in.
		noPDT := Segment{Seq: 17, URI: "seg17.ts", Duration: 2}
		if got, want := f.feedStart(noPDT), f.seen[17].Add(-2*time.Second); !got.Equal(want) {
			t.Errorf("feedStart without date-time = %s, want %s", got, want)
		}
	})

	t.Run("reset drops the calibration", func(t *testing.T) {
		src := &fakeSource{text: st.text, segs: st.segs, mtime: st.mtime, entries: entries, changed: true}
		ov, err := newOverlay(t.TempDir(), Options{}, log.f)
		if err != nil {
			t.Fatal(err)
		}
		f := &follower{src: src, overlay: ov, log: log.f, synced: true, next: 100}
		var w bytes.Buffer
		if err := f.poll(context.Background(), &w, func() {}); err != nil {
			t.Fatal(err)
		}
		if f.next != 17 || len(f.seen) != 7 {
			t.Errorf("after the reset poll: next=%d seen=%d", f.next, len(f.seen))
		}
		if lines := log.all(); len(lines) == 0 || !strings.Contains(lines[len(lines)-1], "playlist reset") {
			t.Errorf("no reset log: %q", lines)
		}
	})
}

// deliver writes a segment in parts around each text change: the first flushFrames frames of the new
// clip go in before the text (the old clip's held frames come out meanwhile), then the rest.
func TestDeliverSplit(t *testing.T) {
	base := time.Date(2026, 9, 18, 13, 12, 0, 0, time.Local)
	st := newStation(base, 9700*time.Millisecond)
	sec := func(d time.Duration) float64 { return unixSeconds(base.Add(d)) }
	entries := []OnAir{
		{T: sec(4*time.Second - 150*time.Millisecond), Program: "前の番組", ID: "B"},
		{T: sec(10*time.Second + 125*time.Millisecond - 200*time.Millisecond), Program: "次の番組", Caption: "冒頭", ID: "C"},
	}
	dir := t.TempDir()
	var log logBuf
	ov, err := newOverlay(dir, Options{}, log.f)
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{text: st.text, segs: st.segs, mtime: st.mtime, entries: entries}
	f := &follower{src: src, overlay: ov, log: log.f, entries: entries}
	segs := ParsePlaylist(st.text)
	f.observe(segs, base.Add(15*time.Second))

	// A writer that records each write together with the title shown at that moment.
	type chunk struct {
		n     int
		title string
	}
	var got []chunk
	w := writerFunc(func(p []byte) (int, error) {
		b, _ := os.ReadFile(filepath.Join(dir, titleFile))
		got = append(got, chunk{len(p), strings.TrimSpace(string(b))})
		return len(p), nil
	})
	// seg 14 is all clip B, nothing shown yet: 3 frames go in blank, then the title, then the rest.
	if err := f.deliver(context.Background(), w, func() {}, segs[4], st.segs["seg14.ts"]); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].n != 4*tsPacket || got[0].title != "" || got[1].n != 4*tsPacket || got[1].title != "前の番組" {
		t.Errorf("seg14 writes = %+v", got)
	}
	// seg 15: clip C starts at frame 3; its text goes in past the first packet of frame 6.
	got = nil
	started := time.Now()
	data := st.segs["seg15.ts"]
	if err := f.deliver(context.Background(), w, func() {}, segs[5], data); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].n != 7*tsPacket || got[1].n != len(data)-7*tsPacket ||
		got[0].title != "前の番組" || got[1].title != "次の番組" {
		t.Errorf("seg15 writes = %+v", got)
	}
	if time.Since(started) < settleWait {
		t.Errorf("the text change must wait settleWait after the previous write")
	}
	if sub, _ := os.ReadFile(filepath.Join(dir, subFile)); !strings.Contains(string(sub), "冒頭") {
		t.Errorf("sub.txt = %q", sub)
	}
	// The next segment, all clip C: one write, no change, no wait.
	got = nil
	started = time.Now()
	if err := f.deliver(context.Background(), w, func() {}, segs[6], st.segs["seg16.ts"]); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].title != "次の番組" || time.Since(started) > settleWait/2 {
		t.Errorf("seg16: writes %+v in %v", got, time.Since(started))
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// A segment open for reading must not stop the broadcaster deleting it (Windows: FILE_SHARE_DELETE);
// a rename over an open file is beyond a reader's control (MoveFileEx), which is why the playlist and
// now.json are read only once per change.
func TestOpenShared(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "seg000001.ts")
	if err := os.WriteFile(target, []byte("[1]"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(target)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Remove(target); err != nil {
		t.Errorf("delete of an open file: %v", err)
	}
	if b, err := readFile(target); err == nil {
		t.Errorf("deleted file still readable: %q", b)
	}
	if b, err := io.ReadAll(f); err != nil || string(b) != "[1]" {
		t.Errorf("the open handle must still read the old content: %q, %v", b, err)
	}
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		g, err := os.Open(target)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		if err := os.Remove(target); err == nil {
			t.Log("os.Open no longer blocks a delete on this Go version; openShared is redundant")
		}
	}
	// The playlist is read once per change.
	pl := filepath.Join(dir, "live.m3u8")
	if err := os.WriteFile(pl, []byte("#EXTM3U\n#EXTINF:2,\na.ts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := &fileSource{dir: dir, file: pl}
	if text, err := src.playlist(); err != nil || !strings.Contains(text, "a.ts") {
		t.Fatalf("playlist: %q, %v", text, err)
	}
	if err := os.WriteFile(pl+".tmp", []byte("#EXTM3U\n#EXTINF:2,\na.ts\n#EXTINF:2,\nb.ts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pl+".tmp", pl); err != nil {
		t.Fatal(err)
	}
	if text, _ := src.playlist(); !strings.Contains(text, "b.ts") {
		t.Errorf("playlist after the rename: %q", text)
	}
	if text, _ := src.playlist(); !strings.Contains(text, "b.ts") {
		t.Errorf("unchanged playlist: %q", text)
	}
}
