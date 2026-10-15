package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/firemio/generate-local-H3/internal/index"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	td, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	return Options{Host: "127.0.0.1", Lib: filepath.Join(td, "lib"), Web: filepath.Join(td, "web"), HLS: filepath.Join(td, "hls")}
}

func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureItems decodes testdata/items.json (two real index entries: an old one-script clip whose prompt
// carries <d> tags, and a programme clip with slot / segment_index / sources).
func fixtureItems(t *testing.T) []index.Item {
	t.Helper()
	var items []index.Item
	if err := json.Unmarshal(readFixture(t, "items.json"), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("fixture has %d items, want 2", len(items))
	}
	return items
}

// stubAirable makes /playlist.json read from a fixture for the duration of the test.
func stubAirable(t *testing.T, items []index.Item) {
	t.Helper()
	orig := airable
	airable = func() []index.Item { return items }
	t.Cleanup(func() { airable = orig })
}

// do performs one request (extra headers as k, v pairs) and returns the response and its body.
func do(t *testing.T, srv *httptest.Server, method, path string, kv ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// TestRoutes is the matrix over every route: status, Content-Type, the headers every response carries,
// the body, a Range request, path traversal and the 404.
func TestRoutes(t *testing.T) {
	stubAirable(t, fixtureItems(t))
	srv := httptest.NewServer(Handler(testOptions(t)))
	defer srv.Close()
	clip := readFixture(t, "lib", "clip-test.mp4")
	seg := readFixture(t, "hls", "seg000001.ts")

	cases := []struct {
		name, method, path string
		hdr                []string // request headers, k, v pairs
		code               int
		ctype              string
		body               []byte // nil: not checked
		want               map[string]string
	}{
		{"root", "GET", "/", nil, 200, "text/html; charset=utf-8", readFixture(t, "web", "index.html"), nil},
		{"index", "GET", "/index.html?x=1", nil, 200, "text/html; charset=utf-8", readFixture(t, "web", "index.html"), nil},
		{"hls.min.js", "GET", "/hls.min.js", nil, 200, "application/javascript", readFixture(t, "web", "hls.min.js"), nil},
		{"now.json", "GET", "/now.json", nil, 200, "application/json; charset=utf-8", readFixture(t, "hls", "now.json"), nil},
		{"clip", "GET", "/clips/clip-test.mp4", nil, 200, "video/mp4", clip, map[string]string{"Accept-Ranges": "bytes", "Content-Length": fmt.Sprint(len(clip))}},
		{"clip query", "GET", "/clips/clip-test.mp4?t=1", nil, 200, "video/mp4", clip, nil},
		{"clip head", "HEAD", "/clips/clip-test.mp4", nil, 200, "video/mp4", []byte{}, map[string]string{"Content-Length": fmt.Sprint(len(clip))}},
		{"clip range", "GET", "/clips/clip-test.mp4", []string{"Range", "bytes=100-199"}, 206, "video/mp4", clip[100:200],
			map[string]string{"Content-Range": fmt.Sprintf("bytes 100-199/%d", len(clip)), "Content-Length": "100"}},
		{"clip open range", "GET", "/clips/clip-test.mp4", []string{"Range", "bytes=2000-"}, 206, "video/mp4", clip[2000:],
			map[string]string{"Content-Range": fmt.Sprintf("bytes 2000-%d/%d", len(clip)-1, len(clip))}},
		{"clip suffix range", "GET", "/clips/clip-test.mp4", []string{"Range", "bytes=-100"}, 206, "video/mp4", clip[len(clip)-100:],
			map[string]string{"Content-Range": fmt.Sprintf("bytes %d-%d/%d", len(clip)-100, len(clip)-1, len(clip))}},
		{"clip bad range", "GET", "/clips/clip-test.mp4", []string{"Range", fmt.Sprintf("bytes=%d-", len(clip)+10)}, 416, "", nil,
			map[string]string{"Content-Range": fmt.Sprintf("bytes */%d", len(clip))}},
		{"clip missing", "GET", "/clips/missing.mp4", nil, 404, "text/plain", []byte("not found"), nil},
		{"m3u8", "GET", "/hls/live.m3u8", nil, 200, "application/vnd.apple.mpegurl", readFixture(t, "hls", "live.m3u8"), nil},
		{"segment", "GET", "/hls/seg000001.ts", nil, 200, "video/mp2t", seg, nil},
		{"segment range", "GET", "/hls/seg000001.ts", []string{"Range", "bytes=188-375"}, 206, "video/mp2t", seg[188:376],
			map[string]string{"Content-Range": fmt.Sprintf("bytes 188-375/%d", len(seg))}},
		{"segment missing", "GET", "/hls/seg999999.ts", nil, 404, "text/plain", []byte("not found"), nil},
		{"other name under hls", "GET", "/hls/now.json", nil, 200, "video/mp2t", readFixture(t, "hls", "now.json"), nil},
		{"post", "POST", "/playlist.json", nil, 405, "text/plain", []byte("method not allowed"), map[string]string{"Allow": "GET, HEAD"}},
	}
	for _, p := range []string{"/nope", "/clips", "/hls", "/index.htm", "/playlist.json/", "/web/index.html", "/hls.min.js.map"} {
		cases = append(cases, struct {
			name, method, path string
			hdr                []string
			code               int
			ctype              string
			body               []byte
			want               map[string]string
		}{"404 " + p, "GET", p, nil, 404, "text/plain", []byte("not found"), map[string]string{"Content-Length": "9"}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := do(t, srv, c.method, c.path, c.hdr...)
			if resp.StatusCode != c.code {
				t.Errorf("status %d, want %d", resp.StatusCode, c.code)
			}
			if c.ctype != "" && resp.Header.Get("Content-Type") != c.ctype {
				t.Errorf("Content-Type %q, want %q", resp.Header.Get("Content-Type"), c.ctype)
			}
			// http.ServeContent strips Cache-Control from its own error responses (the 416), by design.
			if cc := resp.Header.Get("Cache-Control"); (cc != "no-store" && c.code != 416) || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
				t.Errorf("Cache-Control %q, Access-Control-Allow-Origin %q", cc, resp.Header.Get("Access-Control-Allow-Origin"))
			}
			for k, v := range c.want {
				if got := resp.Header.Get(k); got != v {
					t.Errorf("header %s = %q, want %q", k, got, v)
				}
			}
			if c.body != nil && string(body) != string(c.body) {
				t.Errorf("body (%d bytes) %.60q, want (%d bytes) %.60q", len(body), body, len(c.body), c.body)
			}
		})
	}
}

// TestPathTraversal: go.mod exists two levels up from the served directories; no request may reach it, and
// only the base name of the path is looked up.
func TestPathTraversal(t *testing.T) {
	h := Handler(testOptions(t))
	for _, p := range []string{
		"/clips/../go.mod", "/clips/../../../go.mod", "/clips/..%2F..%2F..%2Fgo.mod", "/hls/../../../go.mod",
		"/hls/..%5C..%5C..%5Cgo.mod", "/clips/", "/clips/.", "/clips/..", "/hls/", "/hls/..", "/clips/../lib",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 404 || rec.Body.String() != "not found" {
			t.Errorf("%q: %d %q, want 404 not found", p, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/clips/../../nowhere/clip-test.mp4", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Errorf("base-name lookup: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for in, want := range map[string]string{"/clips/a.mp4": "a.mp4", "/clips/": "", "/clips/.": "", "/clips/..": "", "/hls/..\\..\\x.ts": "x.ts", "/hls/...": "..."} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPlaylist: the document has the Python's shape (compared as parsed values against the golden
// aitv/server.py produced for the same items), the 200-item window, no HTML escaping and "[]" when empty.
func TestPlaylist(t *testing.T) {
	stubAirable(t, fixtureItems(t))
	srv := httptest.NewServer(Handler(testOptions(t)))
	defer srv.Close()
	resp, body := do(t, srv, "GET", "/playlist.json")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" || resp.Header.Get("Content-Length") != fmt.Sprint(len(body)) {
		t.Errorf("%d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Length"))
	}
	var got, want any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body does not parse: %v\n%s", err, body)
	}
	if err := json.Unmarshal(readFixture(t, "playlist_golden.json"), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("playlist differs from the Python golden:\n got %s\nwant %s", body, readFixture(t, "playlist_golden.json"))
	}
	s := string(body)
	for _, raw := range []string{`<d>[Japanese]`, `</d>`, `ローカルAIが24時間つくり続ける放送局`} {
		if !strings.Contains(s, raw) {
			t.Errorf("playlist.json lacks raw %q", raw)
		}
	}

	base := fixtureItems(t)[1]
	var many []index.Item
	for i := 0; i < 250; i++ {
		it := base
		it.ID = fmt.Sprintf("clip-%03d", i)
		many = append(many, it)
	}
	stubAirable(t, many)
	_, body = do(t, srv, "GET", "/playlist.json")
	var doc struct {
		Items []index.Item `json:"items"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) != 200 || doc.Items[0].ID != "clip-050" || doc.Items[199].ID != "clip-249" {
		t.Errorf("window: %d items, %s .. %s, want 200, clip-050 .. clip-249", len(doc.Items), doc.Items[0].ID, doc.Items[len(doc.Items)-1].ID)
	}

	stubAirable(t, nil)
	_, body = do(t, srv, "GET", "/playlist.json")
	if want := `{"station":{"name":"EVO-X2 TV","tagline":"ローカルAIが24時間つくり続ける放送局","lang":"Japanese"},"items":[]}`; string(body) != want {
		t.Errorf("empty playlist:\n got %s\nwant %s", body, want)
	}
}

// TestNowJSONMissing: before the broadcaster has written anything, an empty list, still 200.
func TestNowJSONMissing(t *testing.T) {
	opt := testOptions(t)
	opt.HLS = t.TempDir()
	rec := httptest.NewRecorder()
	Handler(opt).ServeHTTP(rec, httptest.NewRequest("GET", "/now.json", nil))
	if rec.Code != 200 || rec.Body.String() != "[]" || rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Errorf("missing now.json: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
}

func TestListenTwice(t *testing.T) {
	opt := Options{Host: "127.0.0.1", Port: 0}
	l1, err := Listen(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Close()
	opt.Port = l1.Addr().(*net.TCPAddr).Port
	l2, err := Listen(opt)
	if err == nil {
		l2.Close()
		t.Fatalf("second Listen on port %d succeeded; a duplicate station would start silently", opt.Port)
	}
	if !strings.Contains(err.Error(), "in use") || !strings.Contains(err.Error(), fmt.Sprint(opt.Port)) {
		t.Errorf("error does not say the port is in use: %v", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("error does not wrap the bind failure: %v", err)
	}
	l1.Close()
	l3, err := Listen(opt)
	if err != nil {
		t.Fatalf("rebind after close: %v", err)
	}
	l3.Close()
}

// TestServe: Serve answers on the listener, returns nil once ctx is cancelled and frees the port.
func TestServe(t *testing.T) {
	stubAirable(t, fixtureItems(t))
	opt := testOptions(t)
	l, err := Listen(opt)
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, opt) }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	var resp *http.Response
	for i := 0; i < 50; i++ {
		if resp, err = http.Get(base + "/playlist.json"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("station not answering: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"station":{"name":"EVO-X2 TV"`) {
		t.Errorf("playlist via Serve: %d %.80s", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v after cancel, want nil", err)
		}
	case <-time.After(shutdownGrace + 3*time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
	if _, err := http.Get(base + "/now.json"); err == nil {
		t.Error("server still answering after Serve returned")
	}
	l2, err := Listen(Options{Host: "127.0.0.1", Port: port})
	if err != nil {
		t.Errorf("port not released: %v", err)
	} else {
		l2.Close()
	}

	// A listener failure that is not a shutdown is reported.
	l3, err := Listen(Options{Host: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	l3.Close()
	if err := Serve(context.Background(), l3, opt); err == nil {
		t.Error("Serve on a closed listener returned nil")
	}
}
