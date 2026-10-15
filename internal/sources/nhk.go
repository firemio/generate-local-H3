package sources

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// rssItem is one <item> of the feed.
type rssItem struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Link        string `xml:"link"`
	PubDate     string `xml:"pubDate"`
}

type rssDoc struct {
	Items []rssItem `xml:"channel>item"`
}

// 【ライブ】【詳細】 are site labels: re.sub(r"^(【[^】]*】\s*)+", "", title.strip())
var labelRe = regexp.MustCompile(`^(【[^】]*】[\s\p{Z}]*)+`)

// parseHeadlines is the body of aitv/sources.py nhk_headlines on the raw RSS bytes. The result is never
// nil on success (an empty feed is an empty list, which the cache keeps as a value).
func parseHeadlines(raw []byte, cat string) ([]Headline, error) {
	var doc rssDoc
	if err := xml.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return nil, err
	}
	category := cat
	if name, ok := NHKCats[cat]; ok {
		category = name
	}
	items := make([]Headline, 0, len(doc.Items))
	for _, it := range doc.Items {
		title := labelRe.ReplaceAllString(strip(it.Title), "")
		if title == "" {
			continue
		}
		items = append(items, Headline{
			Title:       title,
			Description: strip(collapseWS(it.Description)),
			Link:        strip(it.Link),
			Pub:         strip(it.PubDate),
			Category:    category,
		})
	}
	return items, nil
}

// headlineBlock is _headline_block: "N. [category] (M/D HH:MM) title — description[:90]" per item, the
// time part omitted when the pubDate does not parse. loc is the zone the times are shown in.
func headlineBlock(items []Headline, loc *time.Location) string {
	lines := make([]string, 0, len(items))
	for i, it := range items {
		desc := ""
		if it.Description != "" {
			desc = " — " + cutRunes(it.Description, 90)
		}
		line := fmt.Sprintf("%d. [%s]", i+1, it.Category)
		if w := when(it.Pub, loc); w != "" {
			line += " (" + w + ")"
		}
		lines = append(lines, line+" "+it.Title+desc)
	}
	return strings.Join(lines, "\n")
}

// pubDateLayouts are the spellings an RSS pubDate comes in: RFC 1123 / RFC 822 with a numeric or a named
// zone, with or without the weekday, plus a naive one (read in the local zone) and RFC 3339.
var pubDateLayouts = []string{
	time.RFC1123Z,
	time.RFC1123,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 MST",
	time.RFC822Z,
	time.RFC822,
	"Mon, 2 Jan 2006 15:04:05",
	time.RFC3339,
}

// when is _when: an RSS pubDate -> "M/D HH:MM" in loc (so the writer can say きょう / きのう), "" when it
// does not parse.
func when(pub string, loc *time.Location) string {
	pub = strings.TrimSpace(pub)
	for _, layout := range pubDateLayouts {
		t, err := time.ParseInLocation(layout, pub, loc)
		if err != nil {
			continue
		}
		t = t.In(loc)
		return fmt.Sprintf("%d/%d %02d:%02d", int(t.Month()), t.Day(), t.Hour(), t.Minute())
	}
	return ""
}
