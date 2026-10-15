// Package server is the station web server: player page, rolling playlist JSON, clip files and the HLS
// window (port of aitv/server.py).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/firemio/generate-local-H3/internal/index"
	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/paths"
	"github.com/firemio/generate-local-H3/internal/programs"
	"github.com/firemio/generate-local-H3/internal/rundown"
)

// Options configure the server. Empty directories default to paths.Lib() / paths.Web() / paths.HLS().
type Options struct {
	Host string
	Port int
	Lib  string
	Web  string
	HLS  string
}

// playlistItems is how many of the newest airable clips /playlist.json carries.
const playlistItems = 200

// shutdownGrace bounds how long Serve waits for in-flight responses once ctx is cancelled.
const shutdownGrace = 2 * time.Second

// airable is the playlist's item source; a variable so tests can substitute a fixture.
var airable = func() []index.Item { return rundown.Airable(index.Load()) }

// playlist is the /playlist.json document.
type playlist struct {
	Station programs.Station `json:"station"`
	Items   []index.Item     `json:"items"`
}

// Handler serves the station's routes:
//
//	GET /, /index.html   -> <web>/index.html (text/html; charset=utf-8)
//	GET /playlist.json   -> {"station": programs.STATION, "items": <last 200 of rundown.Airable(index.Load())>}
//	                        (application/json; charset=utf-8, no HTML escaping: prompts carry <d> tags)
//	GET /hls.min.js      -> <web>/hls.min.js (application/javascript)
//	GET /now.json        -> <hls>/now.json, or "[]" when missing
//	GET /clips/<name>    -> <lib>/<basename> (video/mp4)
//	GET /hls/<name>      -> <hls>/<basename> (.m3u8: application/vnd.apple.mpegurl, else video/mp2t)
//	anything else        -> 404 text/plain "not found"
//
// Every response carries Cache-Control: no-store and Access-Control-Allow-Origin: *. Files go through
// http.ServeContent (byte ranges, HEAD). Only the base name of a /clips/ or /hls/ path is looked up, so a
// request can never leave the directory it is served from. Nothing is logged per request.
func Handler(opt Options) http.Handler {
	if opt.Lib == "" {
		opt.Lib = paths.Lib()
	}
	if opt.Web == "" {
		opt.Web = paths.Web()
	}
	if opt.HLS == "" {
		opt.HLS = paths.HLS()
	}
	return &handler{opt}
}

type handler struct{ opt Options }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		send(w, http.StatusMethodNotAllowed, "text/plain", []byte("method not allowed"))
		return
	}
	p := r.URL.Path
	switch {
	case p == "/" || p == "/index.html":
		serveFile(w, r, h.opt.Web, "index.html", "text/html; charset=utf-8")
	case p == "/playlist.json":
		h.playlist(w)
	case p == "/hls.min.js":
		serveFile(w, r, h.opt.Web, "hls.min.js", "application/javascript")
	case p == "/now.json":
		body, err := os.ReadFile(filepath.Join(h.opt.HLS, "now.json"))
		if err != nil {
			body = []byte("[]")
		}
		send(w, http.StatusOK, "application/json; charset=utf-8", body)
	case strings.HasPrefix(p, "/clips/"):
		serveFile(w, r, h.opt.Lib, baseName(p), "video/mp4")
	case strings.HasPrefix(p, "/hls/"):
		name := baseName(p)
		ctype := "video/mp2t"
		if strings.HasSuffix(name, ".m3u8") {
			ctype = "application/vnd.apple.mpegurl"
		}
		serveFile(w, r, h.opt.HLS, name, ctype)
	default:
		notFound(w)
	}
}

// playlist writes {"station": STATION, "items": <last 200 airable clips>} without HTML escaping
// (compact, UTF-8, "[]" rather than null when empty).
func (h *handler) playlist(w http.ResponseWriter) {
	items := airable()
	if len(items) > playlistItems {
		items = items[len(items)-playlistItems:]
	}
	if items == nil {
		items = []index.Item{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(playlist{Station: programs.STATION, Items: items}); err != nil {
		send(w, http.StatusInternalServerError, "text/plain", []byte("playlist: "+err.Error()))
		return
	}
	send(w, http.StatusOK, "application/json; charset=utf-8", bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// baseName is everything after the last '/' or '\' of a URL path; "." and ".." (and a trailing slash)
// give "", so a request can never name a file outside the directory it is served from.
func baseName(p string) string {
	name := p[strings.LastIndexAny(p, `/\`)+1:]
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// send writes a complete response with its Content-Type and Content-Length.
func send(w http.ResponseWriter, code int, ctype string, body []byte) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func notFound(w http.ResponseWriter) {
	send(w, http.StatusNotFound, "text/plain", []byte("not found"))
}

// serveFile answers with <dir>/<name> through http.ServeContent (Accept-Ranges, byte ranges, HEAD), or
// the 404 when name is empty or the path is not a regular file.
func serveFile(w http.ResponseWriter, r *http.Request, dir, name, ctype string) {
	if name == "" {
		notFound(w)
		return
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		notFound(w)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", ctype)
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// Listen binds host:port. On Windows a second bind of a port that is already listened on fails (Go does not
// set SO_REUSEADDR there), which is what we want: a duplicate station must not start silently (two producers,
// two HLS encoders, one index.json). The error then says the port is in use.
func Listen(opt Options) (net.Listener, error) {
	addr := net.JoinHostPort(opt.Host, strconv.Itoa(opt.Port))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && (errno == syscall.EADDRINUSE || errno == 10048) { // 10048: WSAEADDRINUSE
			return nil, fmt.Errorf("port %d is already in use (is another station running on %s?): %w", opt.Port, addr, err)
		}
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return l, nil
}

// Serve serves Handler(opt) on l until ctx is cancelled (then shuts down with a short grace period and
// returns nil). Logs "[aitv] station on http://host:port/   (playlist: /playlist.json, HLS: /hls/live.m3u8)"
// at start. Any other Serve failure is returned.
func Serve(ctx context.Context, l net.Listener, opt Options) error {
	srv := &http.Server{Handler: Handler(opt), ReadHeaderTimeout: 10 * time.Second}
	logx.Printf("[aitv] station on http://%s/   (playlist: /playlist.json, HLS: /hls/live.m3u8)", l.Addr())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			_ = srv.Close()
		}
		<-errc
		return nil
	}
}
