// Package sources fetches real-world material for the writer: the JMA forecast and NHK RSS headlines
// (port of aitv/sources.py).
//
// Generation stays fully local; these are the only network reads and they are best-effort: every fetch has a
// 15 s timeout, results are cached (JMA 3600 s, NHK 1200 s; a failed refresh keeps the stale value), and a
// failure just means "no material" so the station picks another programme instead of inventing news.
// AITV_OFFLINE=1 (anything but "" / "0") disables fetching; AITV_JMA_AREA picks the JMA area (130000 = 東京地方).
package sources

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Material is what the writer gets: a text block for the LLM prompt and short references kept in index.json
// so a viewer can see where a programme came from.
type Material struct {
	Text string
	Refs []string
}

// Headline is one NHK RSS item. Title has the site labels (【ライブ】【詳細】...) stripped; Description is
// whitespace-collapsed; Pub is the raw RFC 822 pubDate; Category is the Japanese category name.
type Headline struct {
	Title       string
	Description string
	Link        string
	Pub         string
	Category    string
}

// NHKCats maps the RSS feed id to its Japanese name: cat0 主要, cat1 社会, cat3 科学・文化, cat5 経済, cat6 国際, cat7 スポーツ.
var NHKCats = map[string]string{"cat0": "主要", "cat1": "社会", "cat3": "科学・文化", "cat5": "経済", "cat6": "国際", "cat7": "スポーツ"}

// Forecast is the parsed JMA forecast (aitv/sources.py _parse_forecast).
type Forecast struct {
	Area     string // forecast[0].timeSeries[0].areas[0].area.name
	Office   string // overview.publishingOffice
	Report   string // overview.reportDatetime
	Overview string // overview.text, whitespace-collapsed, first 600 runes
	Days     []ForecastDay
	// TempPoint / Temps: timeSeries[2] of the first block — "<date> 最低|最高 <v>℃" labels (odd index = 最高),
	// duplicates of (date, value) dropped, empty values skipped.
	TempPoint string
	Temps     []string
	// Pops: "<HH:MM> <p>%" from timeSeries[1], first 8, empty values skipped.
	Pops []string
	Week []WeekDay
}

// ForecastDay is one day of the short-range forecast: Date "YYYY-MM-DD", Weather whitespace-collapsed.
type ForecastDay struct {
	Date    string
	Weather string
}

// WeekDay is one day of the weekly forecast (weatherCodes / pops / tempsMin / tempsMax by index; "" when absent).
type WeekDay struct {
	Date string
	Code string
	Pop  string
	Min  string
	Max  string
}

// WeatherCodes maps the first digit of a JMA weather code to a word: 1 晴れ, 2 くもり, 3 雨, 4 雪.
var WeatherCodes = map[string]string{"1": "晴れ", "2": "くもり", "3": "雨", "4": "雪"}

const (
	jmaOverviewURL = "https://www.jma.go.jp/bosai/forecast/data/overview_forecast/%s.json"
	jmaForecastURL = "https://www.jma.go.jp/bosai/forecast/data/forecast/%s.json"
	nhkRSSURL      = "https://www.nhk.or.jp/rss/news/%s.xml"
	userAgent      = "aitv/0.1 (local AI TV station)"
	timeout        = 15 * time.Second
	jmaTTL         = 3600 * time.Second
	nhkTTL         = 1200 * time.Second
)

// offline mirrors OFFLINE = os.environ.get("AITV_OFFLINE", "") not in ("", "0").
func offline() bool {
	v := os.Getenv("AITV_OFFLINE")
	return v != "" && v != "0"
}

// jmaAreaEnv is AITV_JMA_AREA, 130000 (東京地方) by default.
func jmaAreaEnv() string {
	if v := os.Getenv("AITV_JMA_AREA"); v != "" {
		return v
	}
	return "130000"
}

var httpClient = &http.Client{Timeout: timeout}

// getURL is the one network read (aitv/sources.py _get); tests replace it to serve fixture bytes.
var getURL = httpGet

func httpGet(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// urllib raises HTTPError for anything outside 2xx once redirects have been followed.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// ---- cache (aitv/sources.py _cached) ---------------------------------------------------------

type cacheEntry struct {
	at  time.Time
	val any // nil when the only fetch so far failed (Python cached None)
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

// cached returns the cached value for key when it is younger than ttl, otherwise fetches with fn (not at all
// when offline). A failed refresh keeps the stale value, but a failed first fetch is cached as "nothing" for
// ttl, exactly as the Python did. The lock is not held while fn runs.
func cached[T any](key string, ttl time.Duration, fn func() (T, error)) T {
	var zero T
	cacheMu.Lock()
	if hit, ok := cache[key]; ok && time.Since(hit.at) < ttl {
		cacheMu.Unlock()
		v, _ := hit.val.(T)
		return v
	}
	cacheMu.Unlock()
	if offline() {
		return zero
	}
	var stored any
	if val, err := fn(); err == nil {
		stored = val
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	// keep a stale value rather than nothing when the refresh fails
	if _, ok := cache[key]; stored != nil || !ok {
		cache[key] = cacheEntry{time.Now(), stored}
	}
	v, _ := cache[key].val.(T)
	return v
}

// resetCache forgets everything (tests).
func resetCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = map[string]cacheEntry{}
}

// ---- fetchers ------------------------------------------------------------------------------

// JMAForecast fetches https://www.jma.go.jp/bosai/forecast/data/overview_forecast/<area>.json and
// .../forecast/<area>.json (User-Agent "aitv/0.1 (local AI TV station)") and parses them; cached 1 h.
// nil when unavailable.
func JMAForecast(area string) *Forecast {
	return cached("jma:"+area, jmaTTL, func() (*Forecast, error) {
		ov, err := getURL(fmt.Sprintf(jmaOverviewURL, area))
		if err != nil {
			return nil, err
		}
		fc, err := getURL(fmt.Sprintf(jmaForecastURL, area))
		if err != nil {
			return nil, err
		}
		return parseForecast(ov, fc)
	})
}

// NHKHeadlines fetches https://www.nhk.or.jp/rss/news/<cat>.xml; cached 20 min. nil when unavailable.
func NHKHeadlines(cat string) []Headline {
	return cached("nhk:"+cat, nhkTTL, func() ([]Headline, error) {
		raw, err := getURL(fmt.Sprintf(nhkRSSURL, cat))
		if err != nil {
			return nil, err
		}
		return parseHeadlines(raw, cat)
	})
}

// ---- per-format material (aitv/sources.py material_for) ---------------------------------------

// MaterialFor returns real material for formats that need it, nil when the format is fiction-only or
// nothing could be fetched. The text blocks must match aitv/sources.py material_for exactly (same wording,
// numbering "N. [category] (M/D HH:MM) title — description[:90]", the "fetched YYYY-MM-DD HH:MM" stamp):
//
//   - news: cat0 x6, cat1 x4, cat5 x4, cat6 x4, cat7 x4, cat3 x4 (dropping titles already taken), one
//     "[category]\n<block>" per non-empty category; Refs = the titles.
//   - weather: the JMA forecast; Refs = "<area> <date> <weather>" for the first two days.
//   - variety: cat7 and cat3 (10 each), rng.sample of up to 6; Refs = the titles.
func MaterialFor(fmtKey string, rng *rand.Rand) *Material {
	return materialFor(fmtKey, rng, time.Now(), time.Local)
}

// materialFor is MaterialFor with the clock and the zone injected: now stamps the news block
// ("fetched YYYY-MM-DD HH:MM", time.strftime in the Python) and loc is what pubDates are shown in
// (datetime.astimezone() with no argument, i.e. the local zone).
func materialFor(fmtKey string, rng *rand.Rand, now time.Time, loc *time.Location) *Material {
	switch fmtKey {
	case "news":
		var blocks []string
		var items []Headline
		taken := map[string]bool{}
		for _, q := range []struct {
			cat string
			per int
		}{{"cat0", 6}, {"cat1", 4}, {"cat5", 4}, {"cat6", 4}, {"cat7", 4}, {"cat3", 4}} {
			got := NHKHeadlines(q.cat)
			if len(got) > q.per {
				got = got[:q.per]
			}
			var keep []Headline
			for _, g := range got {
				if !taken[g.Title] {
					keep = append(keep, g)
				}
			}
			if len(keep) == 0 {
				continue
			}
			blocks = append(blocks, "["+NHKCats[q.cat]+"]\n"+headlineBlock(keep, loc))
			items = append(items, keep...)
			for _, g := range keep {
				taken[g.Title] = true
			}
		}
		if len(items) == 0 {
			return nil
		}
		text := "Real NHK headlines by category (Japanese), fetched " + now.In(loc).Format("2006-01-02 15:04") +
			"; each has its publication time (month/day hour:minute). Use them faithfully: read them as given, " +
			"condense but never add facts, names or numbers that are not here. Say when a story is from an earlier " +
			"day (きのう, 13日).\n" + strings.Join(blocks, "\n")
		return &Material{Text: text, Refs: titles(items)}
	case "weather":
		fc := JMAForecast(jmaAreaEnv())
		if fc == nil || len(fc.Days) == 0 {
			return nil
		}
		lines := []string{fmt.Sprintf("Area: %s (JMA %s, issued %s)", fc.Area, fc.Office, cutRunes(fc.Report, 16))}
		for _, d := range fc.Days {
			lines = append(lines, d.Date+": "+d.Weather)
		}
		if len(fc.Temps) > 0 {
			lines = append(lines, "Temperatures at "+fc.TempPoint+": "+strings.Join(fc.Temps, ", "))
		}
		if len(fc.Pops) > 0 {
			lines = append(lines, "Chance of rain: "+strings.Join(fc.Pops, ", "))
		}
		if len(fc.Week) > 0 {
			parts := make([]string, 0, len(fc.Week))
			for _, w := range fc.Week {
				s := sliceRunes(w.Date, 5, -1) + " " + codeText(w.Code)
				if w.Min != "" && w.Max != "" {
					s += " " + w.Min + "-" + w.Max + "℃"
				}
				if w.Pop != "" {
					s += " 降水" + w.Pop + "%"
				}
				parts = append(parts, s)
			}
			lines = append(lines, "Week ahead: "+strings.Join(parts, ", "))
		}
		if fc.Overview != "" {
			lines = append(lines, "Overview: "+fc.Overview)
		}
		text := "Real JMA forecast. State only what is here (weather, temperatures, rain chance); no invented details.\n" +
			strings.Join(lines, "\n")
		var refs []string
		for _, d := range fc.Days[:min(2, len(fc.Days))] {
			refs = append(refs, fc.Area+" "+d.Date+" "+d.Weather)
		}
		return &Material{Text: text, Refs: refs}
	case "variety":
		var pool []Headline
		for _, cat := range []string{"cat7", "cat3"} {
			got := NHKHeadlines(cat)
			if len(got) > 10 {
				got = got[:10]
			}
			pool = append(pool, got...)
		}
		if len(pool) == 0 {
			return nil
		}
		if rng == nil {
			rng = rand.New(rand.NewSource(time.Now().UnixNano()))
		}
		// random.sample: k distinct items in random order
		k := min(6, len(pool))
		picks := make([]Headline, 0, k)
		for _, i := range rng.Perm(len(pool))[:k] {
			picks = append(picks, pool[i])
		}
		text := "Light real topics from today's NHK news (sports, science, culture) the hosts can chat or quiz about. " +
			"Mention them by subject only; never portray or name real people.\n" + headlineBlock(picks, loc)
		return &Material{Text: text, Refs: titles(picks)}
	}
	return nil
}

func titles(items []Headline) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}
