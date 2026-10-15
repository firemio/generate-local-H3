package index

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// realIndex returns the bytes of the real aitv/library/index.json, found by walking up from the package
// directory to the repository root (go test runs in the package directory, where paths.Root() cannot look).
func realIndex(t *testing.T) []byte {
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
			return b
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("repository root not found")
		}
		dir = parent
	}
}

// useTempStore points the store at a temporary library directory for the duration of the test.
func useTempStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "library")
	oldIndex, oldLib := indexPath, libDir
	indexPath = func() string { return filepath.Join(lib, "index.json") }
	libDir = func() string { return lib }
	t.Cleanup(func() { indexPath, libDir = oldIndex, oldLib })
	return lib
}

func intp(v int) *int { return &v }

// Python: [(repr(v), json.dumps(v)) for v in ...] — json.dumps uses float.__repr__.
func TestFormatPyFloat(t *testing.T) {
	cases := map[float64]string{
		0.0:                 "0.0",
		200.0:               "200.0",
		6.583333333333333:   "6.583333333333333",
		480.9:               "480.9",
		1e15:                "1000000000000000.0",
		1e16:                "1e+16",
		1234567890123456.0:  "1234567890123456.0",
		12345678901234567.0: "1.2345678901234568e+16",
		0.001:               "0.001",
		0.0001:              "0.0001",
		0.00001:             "1e-05",
		1.5e-05:             "1.5e-05",
		123456.789:          "123456.789",
		-3.25:               "-3.25",
		1e22:                "1e+22",
		2.5e-7:              "2.5e-07",
		0.1:                 "0.1",
		1.0 / 3.0:           "0.3333333333333333",
		100.0:               "100.0",
		99999999999999990.0: "9.999999999999998e+16",
		5e-324:              "5e-324",
	}
	for v, want := range cases {
		if got := formatPyFloat(v); got != want {
			t.Errorf("formatPyFloat(%v) = %q, want %q", v, got, want)
		}
	}
	if got := formatPyFloat(math.Copysign(0, -1)); got != "-0.0" {
		t.Errorf("formatPyFloat(-0.0) = %q", got)
	}
}

func TestMarshalItemKeys(t *testing.T) {
	old := Item{ID: "a", File: "a.mp4", Format: "news", Program: "P", Caption: "c", Seconds: 6, Seed: 1,
		RenderS: 200, Created: "2026-09-10T18:24:22", Prompt: "x <d>[Japanese] a & b</d>"}
	b, err := encode([]Item{old})
	if err != nil {
		t.Fatal(err)
	}
	want := "[\n {\n  \"id\": \"a\",\n  \"file\": \"a.mp4\",\n  \"format\": \"news\",\n  \"program\": \"P\",\n" +
		"  \"caption\": \"c\",\n  \"seconds\": 6.0,\n  \"seed\": 1,\n  \"render_s\": 200.0,\n" +
		"  \"created\": \"2026-09-10T18:24:22\",\n  \"prompt\": \"x <d>[Japanese] a & b</d>\"\n }\n]"
	if string(b) != want {
		t.Errorf("old clip:\n%s\nwant:\n%s", b, want)
	}

	brk := Item{ID: "b-00", File: "cm-b-00.mp4", Format: "cm", Program: "CM", ProgrammeID: "b", Section: "CM",
		SegmentIndex: intp(0), SegmentsTotal: 1, Caption: "c", Seconds: 6.583333333333333, Seed: 2, RenderS: 200.3,
		Created: "2026-09-14T21:54:17", Prompt: "p"}
	b, err = encode([]Item{brk})
	if err != nil {
		t.Fatal(err)
	}
	want = "[\n {\n  \"id\": \"b-00\",\n  \"file\": \"cm-b-00.mp4\",\n  \"format\": \"cm\",\n  \"program\": \"CM\",\n" +
		"  \"programme_id\": \"b\",\n  \"slot\": null,\n  \"section\": \"CM\",\n  \"segment_index\": 0,\n" +
		"  \"segments_total\": 1,\n  \"caption\": \"c\",\n  \"seconds\": 6.583333333333333,\n  \"seed\": 2,\n" +
		"  \"render_s\": 200.3,\n  \"created\": \"2026-09-14T21:54:17\",\n  \"prompt\": \"p\"\n }\n]"
	if string(b) != want {
		t.Errorf("break clip:\n%s\nwant:\n%s", b, want)
	}

	cur := brk
	cur.Format, cur.Slot, cur.Sources = "news", intp(0), []string{"h1", "h2"}
	b, err = encode([]Item{cur})
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"\"slot\": 0,", "\"sources\": [\n   \"h1\",\n   \"h2\"\n  ]\n }"} {
		if !strings.Contains(string(b), frag) {
			t.Errorf("current clip lacks %q:\n%s", frag, b)
		}
	}

	// An encoder that escapes HTML (json.Marshal) still gets valid, escaped output.
	m, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(m), `\u003cd\u003e`) || strings.Contains(string(m), `"slot"`) {
		t.Errorf("json.Marshal: %s", m)
	}
	var back Item
	if err := json.Unmarshal(m, &back); err != nil || !reflect.DeepEqual(back, old) {
		t.Errorf("json.Marshal round trip: %v %+v", err, back)
	}
	if b, _ := encode(nil); string(b) != "[]" {
		t.Errorf("encode(nil) = %s", b)
	}
}

// The real library: load, save to a temp path, reload, deep-compare; and the saved bytes equal what
// Python's json.dumps(items, ensure_ascii=False, indent=1) wrote (the original file).
func TestRoundTripRealIndex(t *testing.T) {
	orig := realIndex(t)
	lib := useTempStore(t)
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath(), orig, 0o644); err != nil {
		t.Fatal(err)
	}
	items := Load()
	if len(items) == 0 {
		t.Fatal("Load returned nothing")
	}
	t.Logf("%d items", len(items))
	if err := Save(items); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(indexPath() + ".tmp"); err == nil {
		t.Error("index.json.tmp left behind")
	}
	again := Load()
	if !reflect.DeepEqual(items, again) {
		t.Fatal("reload differs from the loaded items")
	}
	saved, err := os.ReadFile(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	// Value-equal to what the Python wrote: generic decode of both, key presence included.
	var a, b []map[string]any
	if err := json.Unmarshal(orig, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(saved, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("generic decode of saved JSON differs from the original")
	}
	// One-space indent, unescaped non-ASCII and < > &, no trailing newline.
	if !bytes.HasPrefix(saved, []byte("[\n {\n  \"id\": \"")) || !bytes.HasSuffix(saved, []byte("\n }\n]")) {
		t.Error("not one-space indented")
	}
	if bytes.Contains(saved, []byte(`\u`)) {
		t.Error("contains \\u escapes (non-ASCII or < > & escaped)")
	}
	if !bytes.Contains(saved, []byte("<d>[Japanese]")) || !bytes.Contains(saved, []byte("</d>")) {
		t.Error("<d> tags not written verbatim")
	}
	if bytes.Count(saved, []byte("&")) != bytes.Count(orig, []byte("&")) {
		t.Error("& count differs")
	}
	if !bytes.Contains(saved, []byte("ニュース")) {
		t.Error("Japanese not written raw")
	}
}

func TestLoadEdgeCases(t *testing.T) {
	useTempStore(t)
	if got := Load(); got == nil || len(got) != 0 {
		t.Errorf("missing file: %#v", got)
	}
	if err := os.MkdirAll(libDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "[", "{\"a\": 1}", "null"} {
		if err := os.WriteFile(indexPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Load(); got == nil || len(got) != 0 {
			t.Errorf("body %q: %#v", body, got)
		}
	}
	if err := os.WriteFile(indexPath(), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Load(); got == nil || len(got) != 0 {
		t.Errorf("empty list: %#v", got)
	}
}

func TestAppendAndPlayable(t *testing.T) {
	lib := useTempStore(t)
	a := Item{ID: "a", File: "a.mp4", Format: "news", Program: "P", Seconds: 6, Created: "2026-09-10T18:24:22"}
	b := Item{ID: "b", File: "b.mp4", Format: "cm", Program: "Q", ProgrammeID: "pb", SegmentIndex: intp(0),
		SegmentsTotal: 1, Seconds: 6.5, Created: "2026-09-10T18:25:22", Sources: []string{"s"}}
	if err := Append(a); err != nil {
		t.Fatal(err)
	}
	if err := Append(b); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if !reflect.DeepEqual(got, []Item{a, b}) {
		t.Errorf("Load = %+v", got)
	}
	if _, err := os.Stat(indexPath() + ".tmp"); err == nil {
		t.Error("index.json.tmp left behind")
	}
	if p := Playable(); len(p) != 0 {
		t.Errorf("Playable with no mp4: %+v", p)
	}
	if err := os.WriteFile(filepath.Join(lib, "b.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := Playable(); len(p) != 1 || p[0].ID != "b" {
		t.Errorf("Playable = %+v", p)
	}
	// An entry without a file is never playable even when a file of that name would exist.
	if err := Append(Item{ID: "c"}); err != nil {
		t.Fatal(err)
	}
	if p := Playable(); len(p) != 1 {
		t.Errorf("Playable = %+v", p)
	}
}

// While another handle holds index.json open, the rename fails ("Access is denied" on Windows, like
// Python's PermissionError); after the retry budget Save rewrites in place and removes the tmp file.
func TestSaveFallsBackToInPlace(t *testing.T) {
	useTempStore(t)
	oldA, oldP := saveAttempts, savePause
	saveAttempts, savePause = 3, time.Millisecond
	t.Cleanup(func() { saveAttempts, savePause = oldA, oldP })

	if err := Save([]Item{{ID: "a"}}); err != nil {
		t.Fatal(err)
	}
	fh, err := os.Open(indexPath())
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := os.Rename(indexPath()+".tmp", indexPath()); err == nil {
		t.Skip("this platform allows a rename onto an open file; the fallback path is not exercised")
	}
	start := time.Now()
	if err := Save([]Item{{ID: "a"}, {ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 3*time.Millisecond {
		t.Errorf("no retries happened (%v)", d)
	}
	if got := Load(); len(got) != 2 || got[1].ID != "b" {
		t.Errorf("Load after fallback = %+v", got)
	}
	if _, err := os.Stat(indexPath() + ".tmp"); err == nil {
		t.Error("index.json.tmp left behind after the in-place rewrite")
	}
}

// One entry that is not a clip record (a hand-edited "section": 1, a bare 7) must not read as an empty
// index, which would make the next Append drop every other clip: the readable entries survive and the
// bad one is skipped.
func TestLoadSkipsBadEntries(t *testing.T) {
	useTempStore(t)
	if err := os.MkdirAll(libDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{`{"id": "x", "section": 1}`, `{"id": "x", "seed": 1.5}`, `{"id": "x", "slot": "0"}`, `{"id": 1}`, `7`, `[1]`} {
		body := "[" + entry + `, {"id": "y", "file": "y.mp4", "programme_id": "p", "slot": 2, "seconds": 6}]`
		if err := os.WriteFile(indexPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		got := Load()
		if len(got) != 1 || got[0].ID != "y" || *got[0].Slot != 2 || got[0].Seconds != 6 {
			t.Errorf("%s: Load = %+v", entry, got)
		}
		if err := Append(Item{ID: "z"}); err != nil {
			t.Fatal(err)
		}
		if got := Load(); len(got) != 2 || got[1].ID != "z" {
			t.Errorf("%s: after Append: %+v", entry, got)
		}
	}
	// A foreign key is simply ignored; the entry itself is kept.
	if err := os.WriteFile(indexPath(), []byte(`[{"id": "a", "file": "a.mp4", "note": "hand-added"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Load(); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("foreign key: Load = %+v", got)
	}
}
