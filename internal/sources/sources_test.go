package sources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// jst is the zone the Python goldens were produced in (the machine's local zone was UTC+9).
var jst = time.FixedZone("JST", 9*3600)

// goldenNowTime is goldenNow ("fetched ...") as a time in jst.
var goldenNowTime = time.Date(2026, 9, 17, 1, 30, 0, 0, jst)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// serveFixtures replaces getURL with a map of URL -> fixture file (anything else fails like a dead
// network), clears the cache and restores both when the test ends. It returns the list of requested URLs.
func serveFixtures(t *testing.T, files map[string]string) *[]string {
	t.Helper()
	var mu sync.Mutex
	var requested []string
	old := getURL
	getURL = func(url string) ([]byte, error) {
		mu.Lock()
		requested = append(requested, url)
		mu.Unlock()
		name, ok := files[url]
		if !ok {
			return nil, errors.New("no fixture for " + url)
		}
		return os.ReadFile(filepath.Join("testdata", name))
	}
	resetCache()
	t.Cleanup(func() {
		getURL = old
		resetCache()
	})
	return &requested
}

var fixtureURLs = map[string]string{
	fmt.Sprintf(jmaOverviewURL, "130000"): "jma_overview_130000.json",
	fmt.Sprintf(jmaForecastURL, "130000"): "jma_forecast_130000.json",
	fmt.Sprintf(nhkRSSURL, "cat0"):        "nhk_cat0.xml",
	fmt.Sprintf(nhkRSSURL, "cat7"):        "nhk_cat7.xml",
}

// Python-shaped decoders for the golden JSON.
type pyForecast struct {
	Area     string `json:"area"`
	Office   string `json:"office"`
	Report   string `json:"report"`
	Overview string `json:"overview"`
	Days     []struct {
		Date    string `json:"date"`
		Weather string `json:"weather"`
	} `json:"days"`
	Temps struct {
		Point  string   `json:"point"`
		Values []string `json:"values"`
	} `json:"temps"`
	Pops []string `json:"pops"`
	Week []struct {
		Date string `json:"date"`
		Code string `json:"code"`
		Pop  string `json:"pop"`
		Min  string `json:"min"`
		Max  string `json:"max"`
	} `json:"week"`
}

func (p pyForecast) forecast() *Forecast {
	fc := &Forecast{Area: p.Area, Office: p.Office, Report: p.Report, Overview: p.Overview,
		TempPoint: p.Temps.Point, Temps: p.Temps.Values, Pops: p.Pops}
	for _, d := range p.Days {
		fc.Days = append(fc.Days, ForecastDay{Date: d.Date, Weather: d.Weather})
	}
	for _, w := range p.Week {
		fc.Week = append(fc.Week, WeekDay{Date: w.Date, Code: w.Code, Pop: w.Pop, Min: w.Min, Max: w.Max})
	}
	return fc
}

type pyHeadline struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Link        string `json:"link"`
	Pub         string `json:"pub"`
	Category    string `json:"category"`
}

func pyHeadlines(t *testing.T, js string) []Headline {
	t.Helper()
	var rows []pyHeadline
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatal(err)
	}
	out := make([]Headline, 0, len(rows))
	for _, r := range rows {
		out = append(out, Headline(r))
	}
	return out
}

func headlinesChecksum(items []Headline) string {
	h := sha256.New()
	for _, it := range items {
		h.Write([]byte(strings.Join([]string{it.Title, it.Description, it.Link, it.Pub, it.Category}, "\x1f")))
		h.Write([]byte{0x1e})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- parsers on the fixtures ------------------------------------------------------------------

func TestParseForecastFixture(t *testing.T) {
	got, err := parseForecast(fixture(t, "jma_overview_130000.json"), fixture(t, "jma_forecast_130000.json"))
	if err != nil {
		t.Fatal(err)
	}
	var py pyForecast
	if err := json.Unmarshal([]byte(goldenForecastJSON), &py); err != nil {
		t.Fatal(err)
	}
	want := py.forecast()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseForecast differs from Python\n got: %#v\nwant: %#v", got, want)
	}
}

func TestParseForecastShapes(t *testing.T) {
	ov := []byte(`{"publishingOffice":"気象庁","reportDatetime":"2026-09-16T16:38:00+09:00","text":" a\n\n　b "}`)
	for _, bad := range []string{`[]`, `[{"timeSeries":[]}]`, `[{"timeSeries":[{"timeDefines":[],"areas":[]}]}]`, `{}`, `not json`} {
		if fc, err := parseForecast(ov, []byte(bad)); err == nil {
			t.Errorf("forecast %s: want error, got %#v", bad, fc)
		}
	}
	if _, err := parseForecast([]byte(`[]`), []byte(`[{"timeSeries":[{"timeDefines":[],"areas":[{"area":{"name":"x"}}]}]}]`)); err == nil {
		t.Error("overview that is a list: want error")
	}
	// Minimal shape: one block, only weathers; the rest optional.
	fc, err := parseForecast(ov, []byte(`[{"timeSeries":[{"timeDefines":["2026-09-16T17:00:00+09:00","2026-09-17T00:00:00+09:00"],
		"areas":[{"area":{"name":"東京地方"},"weathers":["晴れ　時々　くもり"]}]}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := &Forecast{Area: "東京地方", Office: "気象庁", Report: "2026-09-16T16:38:00+09:00", Overview: "a b",
		Days: []ForecastDay{{"2026-09-16", "晴れ 時々 くもり"}}}
	if !reflect.DeepEqual(fc, want) {
		t.Errorf("minimal: got %#v want %#v", fc, want)
	}
	// Weekly block with a short weatherCodes list: "" past the end.
	fc, err = parseForecast(ov, []byte(`[{"timeSeries":[{"timeDefines":["2026-09-16T17:00:00+09:00"],"areas":[{"area":{"name":"x"},"weathers":["晴れ"]}]}]},
		{"timeSeries":[{"timeDefines":["2026-09-17T00:00:00+09:00","2026-09-18T00:00:00+09:00"],"areas":[{"area":{"name":"x"},"weatherCodes":["100"]}]}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []WeekDay{{Date: "2026-09-17", Code: "100"}, {Date: "2026-09-18"}}; !reflect.DeepEqual(fc.Week, want) {
		t.Errorf("short weatherCodes: got %#v want %#v", fc.Week, want)
	}
	// Weekly block with no areas at all -> every field "".
	fc, err = parseForecast(ov, []byte(`[{"timeSeries":[{"timeDefines":["2026-09-16T17:00:00+09:00"],"areas":[{"area":{"name":"x"},"weathers":["晴れ"]}]}]},
		{"timeSeries":[{"timeDefines":["2026-09-17T00:00:00+09:00"],"areas":[]}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []WeekDay{{Date: "2026-09-17"}}; !reflect.DeepEqual(fc.Week, want) {
		t.Errorf("empty weekly areas: got %#v want %#v", fc.Week, want)
	}
	// Temps: duplicates of (date, value) dropped, empty values skipped, point kept even when nothing is left.
	fc, err = parseForecast(ov, []byte(`[{"timeSeries":[
		{"timeDefines":["2026-09-16T17:00:00+09:00"],"areas":[{"area":{"name":"x"},"weathers":["晴れ"]}]},
		{"timeDefines":["2026-09-16T18:00:00+09:00","2026-09-17T00:00:00+09:00"],"areas":[{"area":{"name":"x"},"pops":["","30"]}]},
		{"timeDefines":["2026-09-16T09:00:00+09:00","2026-09-16T09:00:00+09:00","2026-09-17T00:00:00+09:00","2026-09-17T09:00:00+09:00"],
		 "areas":[{"area":{"name":"東京"},"temps":["25","25","","30"]}]}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"2026-09-16 最低 25℃", "2026-09-17 最高 30℃"}; !reflect.DeepEqual(fc.Temps, want) || fc.TempPoint != "東京" {
		t.Errorf("temps: got %q at %q want %q", fc.Temps, fc.TempPoint, want)
	}
	if want := []string{"00:00 30%"}; !reflect.DeepEqual(fc.Pops, want) {
		t.Errorf("pops: got %q want %q", fc.Pops, want)
	}
}

func TestParseHeadlinesFixture(t *testing.T) {
	got, err := parseHeadlines(fixture(t, "nhk_cat0.xml"), "cat0")
	if err != nil {
		t.Fatal(err)
	}
	if want := pyHeadlines(t, goldenCat0JSON); !reflect.DeepEqual(got, want) {
		t.Errorf("cat0 differs from Python\n got: %#v\nwant: %#v", got, want)
	}
	got, err = parseHeadlines(fixture(t, "nhk_cat7.xml"), "cat7")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != goldenCat7Count {
		t.Fatalf("cat7: %d items, Python had %d", len(got), goldenCat7Count)
	}
	if want := pyHeadlines(t, goldenCat7First10JSON); !reflect.DeepEqual(got[:10], want) {
		t.Errorf("cat7[:10] differs from Python\n got: %#v\nwant: %#v", got[:10], want)
	}
	if sum := headlinesChecksum(got); sum != goldenCat7SHA256 {
		t.Errorf("cat7 checksum %s, Python %s", sum, goldenCat7SHA256)
	}
}

func TestParseHeadlinesEdges(t *testing.T) {
	raw := []byte("\xef\xbb\xbf<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<rss version=\"2.0\"><channel><title>x</title>" +
		"<item><title>【ライブ】【詳細】 台風 </title><description> a \n\n b　c </description><link> https://x/1 </link><pubDate> Wed, 16 Sep 2026 22:43:51 +0900 </pubDate></item>" +
		"<item><title>【速報】</title><description>dropped: empty title</description></item>" +
		"<item><title>no desc &amp; entity</title></item>" +
		"<item><title><![CDATA[cdata <b>title</b>]]></title><description>d</description></item>" +
		"</channel></rss>")
	got, err := parseHeadlines(raw, "catX")
	if err != nil {
		t.Fatal(err)
	}
	want := []Headline{
		{Title: "台風", Description: "a b c", Link: "https://x/1", Pub: "Wed, 16 Sep 2026 22:43:51 +0900", Category: "catX"},
		{Title: "no desc & entity", Category: "catX"},
		{Title: "cdata <b>title</b>", Description: "d", Category: "catX"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v\nwant %#v", got, want)
	}
	if got, err := parseHeadlines([]byte(`<rss><channel></channel></rss>`), "cat0"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty feed: want non-nil empty list, got %#v, %v", got, err)
	}
	if _, err := parseHeadlines([]byte(`<rss><channel><item><title>x</title></channel></rss>`), "cat0"); err == nil {
		t.Error("malformed XML: want error")
	}
	if _, err := parseHeadlines([]byte(`<html><body>503</body></html>`), "cat0"); err != nil {
		t.Errorf("non-RSS document parses to nothing, got %v", err)
	}
}

func TestTitleLabels(t *testing.T) {
	for _, c := range goldenTitles {
		if got := labelRe.ReplaceAllString(strip(c[0]), ""); got != c[1] {
			t.Errorf("title %q: got %q want %q", c[0], got, c[1])
		}
	}
}

func TestCollapseWS(t *testing.T) {
	for _, c := range goldenCollapses {
		if got := strip(collapseWS(c[0])); got != c[1] {
			t.Errorf("collapse %q: got %q want %q", c[0], got, c[1])
		}
	}
}

func TestCodeText(t *testing.T) {
	for _, c := range goldenCodeText {
		if got := codeText(c[0]); got != c[1] {
			t.Errorf("codeText(%q) = %q want %q", c[0], got, c[1])
		}
	}
}

func TestRuneSlices(t *testing.T) {
	if got := cutRunes("あいうえお", 3); got != "あいう" {
		t.Errorf("cutRunes: %q", got)
	}
	if got := cutRunes("ab", 5); got != "ab" {
		t.Errorf("cutRunes past end: %q", got)
	}
	if got := sliceRunes("2026-09-17T00:00:00+09:00", 11, 16); got != "00:00" {
		t.Errorf("sliceRunes: %q", got)
	}
	if got := sliceRunes("short", 11, 16); got != "" {
		t.Errorf("sliceRunes past end: %q", got)
	}
	if got := sliceRunes("2026-09-17", 5, -1); got != "09-17" {
		t.Errorf("sliceRunes open end: %q", got)
	}
}

// ---- pubDate -----------------------------------------------------------------------------------

func TestWhen(t *testing.T) {
	for _, c := range goldenWhens {
		if got := when(c[0], jst); got != c[1] {
			t.Errorf("when(%q) = %q, Python %q", c[0], got, c[1])
		}
	}
	// A naive date is taken in the given zone, an aware one converted to it.
	utc := time.UTC
	if got := when("Tue, 15 Sep 2026 23:30:00", utc); got != "9/15 23:30" {
		t.Errorf("naive in UTC: %q", got)
	}
	if got := when("Tue, 15 Sep 2026 23:30:00 +0900", utc); got != "9/15 14:30" {
		t.Errorf("aware in UTC: %q", got)
	}
}

// ---- material ----------------------------------------------------------------------------------

func TestMaterialForNews(t *testing.T) {
	req := serveFixtures(t, fixtureURLs)
	m := materialFor("news", nil, goldenNowTime, jst)
	if m == nil {
		t.Fatal("nil material")
	}
	if m.Text != goldenNewsText {
		t.Errorf("news text differs from Python\n got:\n%s\nwant:\n%s", m.Text, goldenNewsText)
	}
	if !reflect.DeepEqual(m.Refs, goldenNewsRefs) {
		t.Errorf("news refs: got %q want %q", m.Refs, goldenNewsRefs)
	}
	// One request per category, in the Python order.
	want := []string{}
	for _, cat := range []string{"cat0", "cat1", "cat5", "cat6", "cat7", "cat3"} {
		want = append(want, fmt.Sprintf(nhkRSSURL, cat))
	}
	if !reflect.DeepEqual(*req, want) {
		t.Errorf("requests: got %q want %q", *req, want)
	}
	// Second call is served from the cache and the failed categories are cached as nothing for the TTL.
	*req = nil
	if m2 := materialFor("news", nil, goldenNowTime, jst); m2 == nil || m2.Text != m.Text {
		t.Error("cached call differs")
	}
	if len(*req) != 0 {
		t.Errorf("cached call fetched %q", *req)
	}
}

func TestMaterialForNewsDedup(t *testing.T) {
	// The same title in cat0 and cat7 is kept once (first category wins); a duplicate inside one
	// category is kept twice, as the Python did.
	dup := "<?xml version=\"1.0\" encoding=\"utf-8\"?><rss><channel>" +
		"<item><title>same</title><description>d</description><pubDate>Wed, 16 Sep 2026 22:43:51 +0900</pubDate></item>" +
		"<item><title>same</title><description>d</description><pubDate>Wed, 16 Sep 2026 22:43:51 +0900</pubDate></item>" +
		"<item><title>other</title></item></channel></rss>"
	old := getURL
	getURL = func(url string) ([]byte, error) {
		if strings.HasSuffix(url, "/cat0.xml") || strings.HasSuffix(url, "/cat7.xml") {
			return []byte(dup), nil
		}
		return nil, errors.New("down")
	}
	resetCache()
	t.Cleanup(func() { getURL = old; resetCache() })
	m := materialFor("news", nil, goldenNowTime, jst)
	if m == nil {
		t.Fatal("nil")
	}
	want := "[主要]\n1. [主要] (9/16 22:43) same — d\n2. [主要] (9/16 22:43) same — d\n3. [主要] other"
	if body := m.Text[strings.Index(m.Text, "\n")+1:]; body != want {
		t.Errorf("got:\n%s\nwant:\n%s", body, want)
	}
	if want := []string{"same", "same", "other"}; !reflect.DeepEqual(m.Refs, want) {
		t.Errorf("refs %q", m.Refs)
	}
}

func TestMaterialForNewsNothing(t *testing.T) {
	serveFixtures(t, map[string]string{})
	if m := materialFor("news", nil, goldenNowTime, jst); m != nil {
		t.Errorf("all feeds down: want nil, got %#v", m)
	}
}

func TestMaterialForWeather(t *testing.T) {
	req := serveFixtures(t, fixtureURLs)
	m := materialFor("weather", nil, goldenNowTime, jst)
	if m == nil {
		t.Fatal("nil material")
	}
	if m.Text != goldenWeatherText {
		t.Errorf("weather text differs from Python\n got:\n%s\nwant:\n%s", m.Text, goldenWeatherText)
	}
	if !reflect.DeepEqual(m.Refs, goldenWeatherRefs) {
		t.Errorf("weather refs: got %q want %q", m.Refs, goldenWeatherRefs)
	}
	want := []string{fmt.Sprintf(jmaOverviewURL, "130000"), fmt.Sprintf(jmaForecastURL, "130000")}
	if !reflect.DeepEqual(*req, want) {
		t.Errorf("requests: got %q want %q", *req, want)
	}
}

func TestMaterialForWeatherArea(t *testing.T) {
	req := serveFixtures(t, fixtureURLs)
	t.Setenv("AITV_JMA_AREA", "270000")
	if m := materialFor("weather", nil, goldenNowTime, jst); m != nil {
		t.Errorf("no fixture for 270000: want nil, got %#v", m)
	}
	want := []string{fmt.Sprintf(jmaOverviewURL, "270000")}
	if !reflect.DeepEqual(*req, want) {
		t.Errorf("requests: got %q want %q (the forecast is not fetched when the overview fails)", *req, want)
	}
}

func TestMaterialForWeatherNoDays(t *testing.T) {
	old := getURL
	getURL = func(url string) ([]byte, error) {
		if strings.Contains(url, "overview_forecast") {
			return []byte(`{"publishingOffice":"x","reportDatetime":"y","text":"z"}`), nil
		}
		return []byte(`[{"timeSeries":[{"timeDefines":[],"areas":[{"area":{"name":"x"},"weathers":[]}]}]}]`), nil
	}
	resetCache()
	t.Cleanup(func() { getURL = old; resetCache() })
	if fc := JMAForecast("130000"); fc == nil || len(fc.Days) != 0 {
		t.Fatalf("forecast %#v", fc)
	}
	if m := materialFor("weather", nil, goldenNowTime, jst); m != nil {
		t.Errorf("no days: want nil, got %#v", m)
	}
}

func TestMaterialForVariety(t *testing.T) {
	serveFixtures(t, fixtureURLs)
	pool := strings.Split(goldenVarietyPoolBlock, "\n")
	// strip the "N. " numbering so lines can be compared regardless of position
	unnumber := func(s string) string { return s[strings.Index(s, ". ")+2:] }
	poolSet := map[string]bool{}
	for _, l := range pool {
		poolSet[unnumber(l)] = true
	}
	for seed := int64(1); seed <= 20; seed++ {
		m := materialFor("variety", rand.New(rand.NewSource(seed)), goldenNowTime, jst)
		if m == nil {
			t.Fatal("nil material")
		}
		lines := strings.Split(m.Text, "\n")
		if lines[0] != goldenVarietyHeader {
			t.Fatalf("header %q", lines[0])
		}
		lines = lines[1:]
		if len(lines) != 6 || len(m.Refs) != 6 {
			t.Fatalf("want 6 picks, got %d lines / %d refs", len(lines), len(m.Refs))
		}
		seen := map[string]bool{}
		for i, l := range lines {
			if !strings.HasPrefix(l, fmt.Sprintf("%d. [スポーツ]", i+1)) {
				t.Errorf("seed %d line %d numbering: %q", seed, i, l)
			}
			body := unnumber(l)
			if !poolSet[body] {
				t.Errorf("seed %d: line not from the pool: %q", seed, l)
			}
			if seen[body] {
				t.Errorf("seed %d: duplicate pick %q", seed, l)
			}
			seen[body] = true
			if !strings.Contains(l, " "+m.Refs[i]) {
				t.Errorf("seed %d: ref %q not in line %q", seed, m.Refs[i], l)
			}
		}
	}
	// nil rng is allowed
	if m := materialFor("variety", nil, goldenNowTime, jst); m == nil || len(m.Refs) != 6 {
		t.Error("nil rng")
	}
}

func TestMaterialForVarietySmallPool(t *testing.T) {
	old := getURL
	getURL = func(url string) ([]byte, error) {
		if strings.HasSuffix(url, "/cat3.xml") {
			return []byte(`<rss><channel><item><title>a</title></item><item><title>b</title></item></channel></rss>`), nil
		}
		return nil, errors.New("down")
	}
	resetCache()
	t.Cleanup(func() { getURL = old; resetCache() })
	m := materialFor("variety", rand.New(rand.NewSource(3)), goldenNowTime, jst)
	if m == nil || len(m.Refs) != 2 {
		t.Fatalf("got %#v", m)
	}
	body := m.Text[strings.Index(m.Text, "\n")+1:]
	if body != "1. [科学・文化] a\n2. [科学・文化] b" && body != "1. [科学・文化] b\n2. [科学・文化] a" {
		t.Errorf("body %q", body)
	}
}

func TestMaterialForOther(t *testing.T) {
	req := serveFixtures(t, fixtureURLs)
	for _, k := range []string{"drama", "cm", "music", ""} {
		if m := materialFor(k, nil, goldenNowTime, jst); m != nil {
			t.Errorf("%q: want nil, got %#v", k, m)
		}
	}
	if len(*req) != 0 {
		t.Errorf("fiction formats fetched %q", *req)
	}
	if m := MaterialFor("drama", nil); m != nil {
		t.Errorf("MaterialFor drama: %#v", m)
	}
}

func TestMaterialForUsesLocalNow(t *testing.T) {
	serveFixtures(t, fixtureURLs)
	before := time.Now()
	m := MaterialFor("news", nil)
	if m == nil {
		t.Fatal("nil")
	}
	stamp := m.Text[len("Real NHK headlines by category (Japanese), fetched "):][:16]
	got, err := time.ParseInLocation("2006-01-02 15:04", stamp, time.Local)
	if err != nil {
		t.Fatalf("stamp %q: %v", stamp, err)
	}
	if d := before.Sub(got); d > 2*time.Minute || d < -2*time.Minute {
		t.Errorf("stamp %q is not now (%v)", stamp, before)
	}
}

// ---- cache / offline ---------------------------------------------------------------------------

func TestCachedSemantics(t *testing.T) {
	resetCache()
	t.Cleanup(resetCache)
	calls := 0
	fail := false
	fn := func() (*Forecast, error) {
		calls++
		if fail {
			return nil, errors.New("down")
		}
		return &Forecast{Area: fmt.Sprint(calls)}, nil
	}
	// Fresh value is fetched once, then served from the cache.
	if v := cached("k", time.Hour, fn); v == nil || v.Area != "1" {
		t.Fatalf("first: %#v", v)
	}
	if v := cached("k", time.Hour, fn); v == nil || v.Area != "1" || calls != 1 {
		t.Fatalf("second: %#v calls %d", v, calls)
	}
	// Expired: refetched.
	cacheMu.Lock()
	cache["k"] = cacheEntry{at: time.Now().Add(-2 * time.Hour), val: cache["k"].val}
	cacheMu.Unlock()
	if v := cached("k", time.Hour, fn); v == nil || v.Area != "2" || calls != 2 {
		t.Fatalf("refresh: %#v calls %d", v, calls)
	}
	// Expired and the refresh fails: the stale value is kept and every call retries.
	cacheMu.Lock()
	cache["k"] = cacheEntry{at: time.Now().Add(-2 * time.Hour), val: cache["k"].val}
	cacheMu.Unlock()
	fail = true
	if v := cached("k", time.Hour, fn); v == nil || v.Area != "2" || calls != 3 {
		t.Fatalf("stale: %#v calls %d", v, calls)
	}
	if v := cached("k", time.Hour, fn); v == nil || v.Area != "2" || calls != 4 {
		t.Fatalf("stale again: %#v calls %d", v, calls)
	}
	// A failed first fetch is cached as nothing for the TTL (no retry storm), as in the Python.
	if v := cached("k2", time.Hour, fn); v != nil || calls != 5 {
		t.Fatalf("first failure: %#v calls %d", v, calls)
	}
	if v := cached("k2", time.Hour, fn); v != nil || calls != 5 {
		t.Fatalf("cached failure: %#v calls %d", v, calls)
	}
	// ... but retried once the TTL has passed.
	cacheMu.Lock()
	cache["k2"] = cacheEntry{at: time.Now().Add(-2 * time.Hour)}
	cacheMu.Unlock()
	fail = false
	if v := cached("k2", time.Hour, fn); v == nil || v.Area != "6" || calls != 6 {
		t.Fatalf("retry after ttl: %#v calls %d", v, calls)
	}
	// An empty list is a value (an empty feed), not a failure.
	lst := cached("k3", time.Hour, func() ([]Headline, error) { return []Headline{}, nil })
	if lst == nil {
		t.Fatal("empty list lost")
	}
	if v := cached("k3", time.Hour, func() ([]Headline, error) { t.Fatal("refetched"); return nil, nil }); v == nil {
		t.Fatal("empty list not cached")
	}
}

func TestCachedConcurrent(t *testing.T) {
	resetCache()
	t.Cleanup(resetCache)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("c%d", i%5)
			v := cached(key, time.Hour, func() ([]Headline, error) {
				time.Sleep(time.Millisecond)
				return []Headline{{Title: key}}, nil
			})
			if len(v) != 1 || v[0].Title != key {
				t.Errorf("got %#v for %s", v, key)
			}
		}(i)
	}
	wg.Wait()
}

func TestOffline(t *testing.T) {
	req := serveFixtures(t, fixtureURLs)
	for _, v := range []string{"1", "true", "yes", "0x"} {
		t.Setenv("AITV_OFFLINE", v)
		resetCache()
		if fc := JMAForecast("130000"); fc != nil {
			t.Errorf("AITV_OFFLINE=%s: got a forecast", v)
		}
		if hs := NHKHeadlines("cat0"); hs != nil {
			t.Errorf("AITV_OFFLINE=%s: got headlines", v)
		}
		if m := MaterialFor("news", nil); m != nil {
			t.Errorf("AITV_OFFLINE=%s: got material", v)
		}
	}
	if len(*req) != 0 {
		t.Errorf("offline fetched %q", *req)
	}
	// "" and "0" mean online.
	for _, v := range []string{"", "0"} {
		t.Setenv("AITV_OFFLINE", v)
		resetCache()
		if fc := JMAForecast("130000"); fc == nil {
			t.Errorf("AITV_OFFLINE=%q: no forecast", v)
		}
	}
	// Offline still serves what is already cached; nothing is cached for a miss.
	t.Setenv("AITV_OFFLINE", "1")
	if fc := JMAForecast("130000"); fc == nil {
		t.Error("cached forecast not served offline")
	}
	n := len(*req)
	if hs := NHKHeadlines("cat0"); hs != nil {
		t.Error("headlines offline")
	}
	t.Setenv("AITV_OFFLINE", "")
	if hs := NHKHeadlines("cat0"); hs == nil || len(*req) != n+1 {
		t.Error("offline miss must not poison the cache")
	}
}

// ---- HTTP --------------------------------------------------------------------------------------

func TestHTTPGet(t *testing.T) {
	var gotUA, redirectedUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			gotUA = r.Header.Get("User-Agent")
			_, _ = w.Write([]byte("body"))
		case "/moved":
			http.Redirect(w, r, "/target", http.StatusMovedPermanently)
		case "/target":
			redirectedUA = r.Header.Get("User-Agent")
			_, _ = w.Write([]byte("redirected"))
		case "/gone":
			http.Error(w, "gone", http.StatusNotFound)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte("late"))
		}
	}))
	defer srv.Close()
	b, err := httpGet(srv.URL + "/ok")
	if err != nil || string(b) != "body" {
		t.Errorf("ok: %q %v", b, err)
	}
	if gotUA != "aitv/0.1 (local AI TV station)" {
		t.Errorf("User-Agent %q", gotUA)
	}
	if b, err := httpGet(srv.URL + "/moved"); err != nil || string(b) != "redirected" || redirectedUA != userAgent {
		t.Errorf("redirect: %q %v ua %q", b, err, redirectedUA)
	}
	if _, err := httpGet(srv.URL + "/gone"); err == nil {
		t.Error("404: want error")
	}
	if _, err := httpGet("http://127.0.0.1:1/"); err == nil {
		t.Error("refused: want error")
	}
	if httpClient.Timeout != 15*time.Second {
		t.Errorf("timeout %v", httpClient.Timeout)
	}
	old := httpClient.Timeout
	httpClient.Timeout = 50 * time.Millisecond
	defer func() { httpClient.Timeout = old }()
	if _, err := httpGet(srv.URL + "/slow"); err == nil {
		t.Error("timeout: want error")
	}
}

// TestLive fetches the real JMA and NHK feeds once; skipped offline or with -short.
func TestLive(t *testing.T) {
	if v := os.Getenv("AITV_OFFLINE"); v != "" && v != "0" {
		t.Skip("AITV_OFFLINE set")
	}
	if testing.Short() {
		t.Skip("-short")
	}
	resetCache()
	t.Cleanup(resetCache)
	fc := JMAForecast("130000")
	if fc == nil {
		t.Fatal("JMA 130000: nothing (network?)")
	}
	if fc.Area == "" || len(fc.Days) == 0 || fc.Days[0].Weather == "" || len(fc.Days[0].Date) != 10 {
		t.Errorf("JMA: %#v", fc)
	}
	hs := NHKHeadlines("cat0")
	if len(hs) == 0 {
		t.Fatal("NHK cat0: nothing (network?)")
	}
	if hs[0].Category != "主要" || hs[0].Title == "" || when(hs[0].Pub, time.Local) == "" {
		t.Errorf("NHK: %#v", hs[0])
	}
	if m := MaterialFor("weather", nil); m == nil || !strings.HasPrefix(m.Text, "Real JMA forecast.") || len(m.Refs) == 0 {
		t.Errorf("weather material: %#v", m)
	}
}
