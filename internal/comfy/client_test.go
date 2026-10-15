package comfy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/firemio/generate-local-H3/internal/paths"
)

// ---- fake ComfyUI --------------------------------------------------------

// fakeComfy is an httptest server speaking the slice of ComfyUI's API the client uses: /system_stats,
// /free, /interrupt, /queue, /prompt, /ws, /history/<id>, /view, /upload/image.
type fakeComfy struct {
	t      *testing.T
	srv    *httptest.Server
	stop   chan struct{}
	queued chan string // prompt ids handed to the ws handler by /prompt
	pid    string      // what /prompt answers with

	script          func(ws *websocket.Conn, pid string) // websocket scenario, run once the prompt is queued
	history         string                               // fixture text (__PID__ replaced); "" = 404
	promptStatus    int                                  // /prompt override (0 = normal answer)
	promptBody      string
	alive           bool
	uploadSubfolder string

	mu         sync.Mutex
	interrupts int
	dequeued   [][]string
	frees      []map[string]any
	views      []url.Values
	prompts    []map[string]any
	promptRaw  []string
	wsClients  []string
	uploads    []upload
	viewData   map[string][]byte // "<subfolder>/<filename>" (or "<filename>") -> bytes
}

type upload struct {
	filename, partType, subfolder, overwrite, typ, contentType string
	blob                                                       []byte
}

var upgrader = websocket.Upgrader{}

func newFake(t *testing.T) *fakeComfy {
	t.Helper()
	f := &fakeComfy{
		t:               t,
		stop:            make(chan struct{}),
		queued:          make(chan string, 8),
		pid:             "pid-" + randomHex()[:12],
		alive:           true,
		uploadSubfolder: "h3gen",
		viewData:        map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		alive := f.alive
		f.mu.Unlock()
		if !alive {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"system": {"os": "nt", "comfyui_version": "0.35.0"}, "devices": [{"name": "AMD Radeon", "vram_total": 103079215104}]}`)
	})
	mux.HandleFunc("/free", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.frees = append(f.frees, body)
		f.mu.Unlock()
	})
	mux.HandleFunc("/interrupt", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.interrupts++
		f.mu.Unlock()
	})
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Delete []string `json:"delete"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.dequeued = append(f.dequeued, body.Delete)
		f.mu.Unlock()
	})
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.prompts = append(f.prompts, body)
		f.promptRaw = append(f.promptRaw, string(raw))
		status, errBody := f.promptStatus, f.promptBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, errBody)
			return
		}
		f.queued <- f.pid
		fmt.Fprintf(w, `{"prompt_id": "%s", "number": 3, "node_errors": {}}`, f.pid)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.wsClients = append(f.wsClients, r.URL.Query().Get("clientId"))
		f.mu.Unlock()
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var pid string
		select {
		case pid = <-f.queued:
		case <-f.stop:
			return
		case <-time.After(10 * time.Second):
			return
		}
		f.mu.Lock()
		script := f.script
		f.mu.Unlock()
		if script != nil {
			script(ws, pid)
		}
		for { // hold the socket open until the client hangs up
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		hist := f.history
		f.mu.Unlock()
		if hist == "" {
			http.NotFound(w, r)
			return
		}
		pid := strings.TrimPrefix(r.URL.Path, "/history/")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, strings.NewReplacer("__PID__", pid, "__CLIENT__", "test").Replace(hist))
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.views = append(f.views, q)
		key := q.Get("filename")
		if sub := q.Get("subfolder"); sub != "" {
			key = sub + "/" + key
		}
		data, ok := f.viewData[key]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Write(data)
	})
	mux.HandleFunc("/upload/image", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file, hdr, err := r.FormFile("image")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		blob, _ := io.ReadAll(file)
		file.Close()
		f.mu.Lock()
		f.uploads = append(f.uploads, upload{
			filename: hdr.Filename, partType: hdr.Header.Get("Content-Type"), blob: blob,
			subfolder: r.FormValue("subfolder"), overwrite: r.FormValue("overwrite"), typ: r.FormValue("type"),
			contentType: r.Header.Get("Content-Type"),
		})
		sub := f.uploadSubfolder
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name": "%s", "subfolder": "%s", "type": "input"}`, hdr.Filename, sub)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		close(f.stop)
		f.srv.Close()
	})
	return f
}

// client builds a Client against the fake. OutputDir is forced to outputDir: New falls back to
// paths.Output() for 127.0.0.1, and the tests must not depend on whether <root>/output exists here.
func (f *fakeComfy) client(outputDir string) *Client {
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		f.t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	c := New(host, port, outputDir)
	c.OutputDir = outputDir
	return c
}

// set changes the fake's configuration under its lock (the handlers run on other goroutines).
func (f *fakeComfy) set(fn func(f *fakeComfy)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeComfy) interruptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interrupts
}

func (f *fakeComfy) dequeuedIDs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.dequeued...)
}

func (f *fakeComfy) viewCalls() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.views...)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// logBuf collects the "[h3gen] ..." lines Run emits.
type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) f(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logBuf) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *logBuf) count(re *regexp.Regexp) int {
	n := 0
	for _, s := range l.all() {
		if re.MatchString(s) {
			n++
		}
	}
	return n
}

func (l *logBuf) has(sub string) bool {
	for _, s := range l.all() {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func ev(typ string, data map[string]any) map[string]any {
	return map[string]any{"type": typ, "data": data}
}

func send(ws *websocket.Conn, v any) error {
	return ws.WriteJSON(v)
}

// testGraph is a minimal API graph (Build is graph.go's business; the client only ships it).
func testGraph() Graph {
	return Graph{
		{"cond", Node{ClassType: "CLIPTextEncode", Inputs: Inputs{{"text", "A cat. <d>[ja]こんにちは</d>"}, {"clip", []any{"clip", 0}}}}},
		{"save", Node{ClassType: "SaveVideo", Inputs: Inputs{{"filename_prefix", "h3/h3"}, {"format", "auto"}, {"codec", "auto"}, {"video", []any{"video", 0}}}}},
	}
}

// runOK is Run with a Background context, failing the test on the error return (the raise paths).
func runOK(t *testing.T, c *Client, opt RunOptions) RunResult {
	t.Helper()
	res, err := c.Run(context.Background(), testGraph(), opt)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// okScript is a normal run: a status event without prompt_id, events of another prompt (ignored), a queue
// wait, three nodes with a binary preview frame and progress steps (one duplicated), then node null.
func okScript(ws *websocket.Conn, pid string) {
	send(ws, ev("status", map[string]any{"status": map[string]any{"exec_info": map[string]any{"queue_remaining": 1}}, "sid": "abc"}))
	send(ws, ev("executing", map[string]any{"node": "foreign", "display_node": "foreign", "prompt_id": "other-prompt"}))
	send(ws, ev("execution_error", map[string]any{"prompt_id": "other-prompt", "node_id": "x", "node_type": "Foreign", "exception_message": "not ours"}))
	send(ws, ev("progress", map[string]any{"value": 9, "max": 9, "prompt_id": "other-prompt", "node": "foreign"}))
	time.Sleep(300 * time.Millisecond) // queue wait: must not count into Seconds
	send(ws, ev("execution_start", map[string]any{"prompt_id": pid, "timestamp": 1758000000000}))
	send(ws, ev("executing", map[string]any{"node": "unet", "display_node": "unet", "prompt_id": pid}))
	time.Sleep(30 * time.Millisecond)
	ws.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 0, 0, 0xff, 0xd8})
	send(ws, ev("executing", map[string]any{"node": "sample", "display_node": "sample", "prompt_id": pid}))
	time.Sleep(30 * time.Millisecond)
	for _, v := range []int{1, 2, 2, 3, 4} {
		send(ws, ev("progress", map[string]any{"value": v, "max": 4, "prompt_id": pid, "node": "sample"}))
	}
	send(ws, ev("executing", map[string]any{"node": "save", "display_node": "save", "prompt_id": pid}))
	time.Sleep(30 * time.Millisecond)
	send(ws, ev("executing", map[string]any{"node": nil, "prompt_id": pid}))
}

// invalidPromptJSON is what ComfyUI's /prompt answers (status 400) for a graph that fails validation.
const invalidPromptJSON = `{"error": {"type": "invalid_prompt", "message": "Cannot execute because a node is missing the class_type property.", "details": "Node ID '#save'", "extra_info": {}}, "node_errors": {}}`

// ---- Run ----------------------------------------------------------------

func TestRunOKLocalOutput(t *testing.T) {
	f := newFake(t)
	f.set(func(f *fakeComfy) { f.script = okScript })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_ok.json") })
	outDir := t.TempDir()
	local := filepath.Join(outDir, "h3", "h3test_00042_.mp4")
	os.MkdirAll(filepath.Dir(local), 0o755)
	os.WriteFile(local, []byte("FRESH RENDER"), 0o644)
	c := f.client(outDir)
	var lb logBuf

	t0 := time.Now()
	res := runOK(t, c, RunOptions{Log: lb.f, DownloadDir: filepath.Join(outDir, "h3")})
	elapsed := time.Since(t0).Seconds()

	if !res.OK || res.Err != "" {
		t.Fatalf("OK=%v Err=%q, want OK", res.OK, res.Err)
	}
	if res.PromptID != f.pid {
		t.Errorf("PromptID=%q want %q", res.PromptID, f.pid)
	}
	wantNodes := map[string]bool{"unet": true, "sample": true, "save": true}
	if len(res.NodeTimes) != len(wantNodes) {
		t.Errorf("NodeTimes=%v want keys %v", res.NodeTimes, wantNodes)
	}
	for k, v := range res.NodeTimes {
		if !wantNodes[k] || v <= 0 {
			t.Errorf("NodeTimes[%q]=%v", k, v)
		}
	}
	if !reflect.DeepEqual(res.NodeOrder, []string{"unet", "sample", "save"}) { // execution order, as Python's dict kept it
		t.Errorf("NodeOrder=%v", res.NodeOrder)
	}
	if res.Seconds <= 0 || res.Seconds > elapsed-0.2 {
		t.Errorf("Seconds=%v elapsed=%v: queue wait must be excluded", res.Seconds, elapsed)
	}
	wantOut := []Output{{Filename: "h3test_00042_.mp4", Subfolder: "h3", Type: "output"}}
	if !reflect.DeepEqual(res.Outputs, wantOut) {
		t.Errorf("Outputs=%+v want %+v", res.Outputs, wantOut)
	}
	if !reflect.DeepEqual(res.LocalFiles, []string{local}) {
		t.Errorf("LocalFiles=%v want [%s]", res.LocalFiles, local)
	}
	if n := len(f.viewCalls()); n != 0 {
		t.Errorf("/view called %d times; a local output must never be re-downloaded", n)
	}
	if b, _ := os.ReadFile(local); string(b) != "FRESH RENDER" {
		t.Errorf("local render was touched: %q", b)
	}
	if f.interruptCount() != 0 || len(f.dequeuedIDs()) != 0 {
		t.Errorf("interrupt/dequeue called on a clean run")
	}

	// Log lines: "[h3gen] queued <pid>", "executing <node>", deduplicated "step v/max", nothing foreign.
	lines := lb.all()
	if len(lines) == 0 || lines[0] != "[h3gen] queued "+f.pid {
		t.Errorf("first log line %q", lines)
	}
	for _, node := range []string{"unet", "sample", "save"} {
		re := regexp.MustCompile(`^\[h3gen\] +\d+\.\ds executing ` + node + `$`)
		if lb.count(re) != 1 {
			t.Errorf("executing %s logged %d times: %q", node, lb.count(re), lines)
		}
	}
	for _, step := range []string{"1/4", "2/4", "3/4", "4/4"} {
		re := regexp.MustCompile(`^\[h3gen\] +\d+\.\ds   step ` + step + `$`)
		if lb.count(re) != 1 {
			t.Errorf("step %s logged %d times: %q", step, lb.count(re), lines)
		}
	}
	if lb.has("foreign") || lb.has("ERROR") || lb.has("9/9") {
		t.Errorf("events of another prompt leaked into the log: %q", lines)
	}

	// The queued prompt: {"prompt": graph, "client_id": id}, HTML left unescaped.
	f.mu.Lock()
	prompt, raw, wsClients := f.prompts[0], f.promptRaw[0], f.wsClients
	f.mu.Unlock()
	if prompt["client_id"] != c.ClientID {
		t.Errorf("client_id=%v want %s", prompt["client_id"], c.ClientID)
	}
	g, ok := prompt["prompt"].(map[string]any)
	if !ok || g["save"] == nil || g["cond"] == nil {
		t.Errorf("prompt body=%v", prompt["prompt"])
	}
	if text := g["cond"].(map[string]any)["inputs"].(map[string]any)["text"]; text != "A cat. <d>[ja]こんにちは</d>" {
		t.Errorf("prompt text=%q", text)
	}
	if !strings.Contains(raw, "<d>[ja]") { // the <d> tags go out verbatim, not HTML-escaped
		t.Errorf("prompt JSON escapes the <d> tag: %s", raw)
	}
	if !reflect.DeepEqual(wsClients, []string{c.ClientID}) {
		t.Errorf("ws clientId=%v want %s", wsClients, c.ClientID)
	}
}

func TestRunDownloadsWhenNotLocal(t *testing.T) {
	f := newFake(t)
	f.set(func(f *fakeComfy) { f.script = okScript })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_ok.json") })
	f.set(func(f *fakeComfy) { f.viewData["h3/h3test_00042_.mp4"] = []byte("MP4 BYTES FROM /view") })
	c := f.client("") // remote ComfyUI: no local output dir
	dl := filepath.Join(t.TempDir(), "dl", "nested")

	res := runOK(t, c, RunOptions{Log: (&logBuf{}).f, DownloadDir: dl})
	if !res.OK {
		t.Fatalf("Err=%q", res.Err)
	}
	dst := filepath.Join(dl, "h3test_00042_.mp4")
	if !reflect.DeepEqual(res.LocalFiles, []string{dst}) {
		t.Fatalf("LocalFiles=%v want [%s]", res.LocalFiles, dst)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "MP4 BYTES FROM /view" {
		t.Errorf("downloaded file: %q %v", b, err)
	}
	if _, err := os.Stat(dst + ".tmp"); err == nil {
		t.Errorf("temp file left behind")
	}
	views := f.viewCalls()
	if len(views) != 1 {
		t.Fatalf("/view calls=%v", views)
	}
	want := url.Values{"filename": {"h3test_00042_.mp4"}, "subfolder": {"h3"}, "type": {"output"}}
	if !reflect.DeepEqual(views[0], want) {
		t.Errorf("/view query=%v want %v", views[0], want)
	}
}

func TestRunNoDownloadDirNoOutputDir(t *testing.T) {
	f := newFake(t)
	f.set(func(f *fakeComfy) { f.script = okScript })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_ok.json") })
	c := f.client("")
	res := runOK(t, c, RunOptions{Log: (&logBuf{}).f})
	if !res.OK || len(res.Outputs) != 1 || len(res.LocalFiles) != 0 || len(f.viewCalls()) != 0 {
		t.Errorf("OK=%v Outputs=%v LocalFiles=%v views=%v", res.OK, res.Outputs, res.LocalFiles, f.viewCalls())
	}
}

// The collision guard: an output that is not local would be downloaded onto a path already resolved
// locally (DownloadDir == the local subfolder) — it must be skipped, never truncating the fresh render.
// Also covers the order of outputs (nodes by id, then videos, gifs, images, audio) and non-output keys
// ("animated").
func TestRunOutputOrderAndCollisionGuard(t *testing.T) {
	f := newFake(t)
	f.set(func(f *fakeComfy) { f.script = okScript })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_multi.json") })
	f.set(func(f *fakeComfy) { f.viewData["h3test_00042_.png"] = []byte("PNG") })
	f.set(func(f *fakeComfy) { f.viewData["elsewhere/h3test_00042_.mp4"] = []byte("MUST NOT BE FETCHED") })
	outDir := t.TempDir()
	h3 := filepath.Join(outDir, "h3")
	os.MkdirAll(h3, 0o755)
	mp4 := filepath.Join(h3, "h3test_00042_.mp4")
	flac := filepath.Join(h3, "h3test_00042_.flac")
	os.WriteFile(mp4, []byte("FRESH RENDER"), 0o644)
	os.WriteFile(flac, []byte("FRESH AUDIO"), 0o644)
	c := f.client(outDir)

	res := runOK(t, c, RunOptions{Log: (&logBuf{}).f, DownloadDir: h3})
	if !res.OK {
		t.Fatalf("Err=%q", res.Err)
	}
	wantOut := []Output{ // nodes preview, save, save2
		{"h3test_00042_.png", "", "temp"},
		{"h3test_00042_.flac", "h3", "output"},
		{"h3test_00042_.mp4", "h3", "output"},
		{"h3test_00042_.mp4", "elsewhere", "output"},
	}
	if !reflect.DeepEqual(res.Outputs, wantOut) {
		t.Errorf("Outputs=%+v\nwant %+v", res.Outputs, wantOut)
	}
	png := filepath.Join(h3, "h3test_00042_.png")
	if want := []string{png, flac, mp4}; !reflect.DeepEqual(res.LocalFiles, want) {
		t.Errorf("LocalFiles=%v\nwant %v", res.LocalFiles, want)
	}
	views := f.viewCalls()
	if len(views) != 1 || views[0].Get("filename") != "h3test_00042_.png" || views[0].Get("type") != "temp" || views[0].Get("subfolder") != "" {
		t.Errorf("/view calls=%v want only the png", views)
	}
	if b, _ := os.ReadFile(mp4); string(b) != "FRESH RENDER" {
		t.Errorf("the fresh render was overwritten: %q", b)
	}
	if b, _ := os.ReadFile(png); string(b) != "PNG" {
		t.Errorf("png=%q", b)
	}
}

func TestRunExecutionError(t *testing.T) {
	f := newFake(t)
	tb := make([]any, 17)
	for i := range tb {
		tb[i] = fmt.Sprintf("tb%02d\n", i)
	}
	script := func(ws *websocket.Conn, pid string) {
		send(ws, ev("executing", map[string]any{"node": "unet", "display_node": "unet", "prompt_id": pid}))
		time.Sleep(20 * time.Millisecond)
		send(ws, ev("executing", map[string]any{"node": "sample", "display_node": "sample", "prompt_id": pid}))
		time.Sleep(20 * time.Millisecond)
		send(ws, ev("execution_error", map[string]any{
			"prompt_id": pid, "node_id": "sample", "node_type": "SamplerCustomAdvanced",
			"exception_message": "HIP out of memory. Tried to allocate 2.00 GiB", "exception_type": "torch.OutOfMemoryError",
			"traceback": tb, "current_inputs": map[string]any{}, "current_outputs": []any{},
		}))
		send(ws, ev("executing", map[string]any{"node": nil, "prompt_id": pid}))
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_error.json") })
	c := f.client("")
	var lb logBuf
	res := runOK(t, c, RunOptions{Log: lb.f})

	wantErr := "SamplerCustomAdvanced: HIP out of memory. Tried to allocate 2.00 GiB"
	if res.OK || res.Err != wantErr {
		t.Errorf("OK=%v Err=%q want %q", res.OK, res.Err, wantErr)
	}
	if len(res.NodeTimes) != 2 || res.NodeTimes["unet"] <= 0 || res.NodeTimes["sample"] <= 0 {
		t.Errorf("NodeTimes=%v", res.NodeTimes)
	}
	if len(res.Outputs) != 0 || len(res.LocalFiles) != 0 {
		t.Errorf("Outputs=%v LocalFiles=%v", res.Outputs, res.LocalFiles)
	}
	if !lb.has("[h3gen] ERROR " + wantErr) {
		t.Errorf("no ERROR line: %q", lb.all())
	}
	var last15 []string
	for i := 2; i < 17; i++ {
		last15 = append(last15, fmt.Sprintf("tb%02d\n", i))
	}
	wantTB := strings.Join(last15, "\n")
	found := false
	for _, l := range lb.all() {
		if l == wantTB {
			found = true
		}
		if strings.Contains(l, "tb01\n") {
			t.Errorf("traceback not cut to the last 15 lines: %q", l)
		}
	}
	if !found {
		t.Errorf("traceback tail not logged: %q", lb.all())
	}
	if f.interruptCount() != 0 {
		t.Errorf("interrupt called after an execution_error (the prompt already finished)")
	}
}

func TestRunHistoryReportsError(t *testing.T) {
	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		send(ws, ev("executing", map[string]any{"node": nil, "prompt_id": pid}))
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_error.json") })
	res := runOK(t, f.client(""), RunOptions{Log: (&logBuf{}).f})
	if res.OK || res.Err != "history reports error" {
		t.Errorf("OK=%v Err=%q", res.OK, res.Err)
	}
}

func TestRunHistoryWithoutPrompt(t *testing.T) {
	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		send(ws, ev("executing", map[string]any{"node": nil, "prompt_id": pid}))
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = "{}" })
	res := runOK(t, f.client(""), RunOptions{Log: (&logBuf{}).f, DownloadDir: t.TempDir()})
	if !res.OK || res.Err != "" || len(res.Outputs) != 0 || len(res.LocalFiles) != 0 {
		t.Errorf("OK=%v Err=%q Outputs=%v LocalFiles=%v", res.OK, res.Err, res.Outputs, res.LocalFiles)
	}
}

// shortReads shortens the websocket read timeout for tests whose socket goes silent: the deadline and the
// stall clock are only consulted once a read returns (60 s in production).
func shortReads(t *testing.T, d time.Duration) {
	t.Helper()
	old := wsReadTimeout
	wsReadTimeout = d
	t.Cleanup(func() { wsReadTimeout = old })
}

func TestRunStallTimeout(t *testing.T) {
	shortReads(t, 50*time.Millisecond)
	f := newFake(t)
	f.set(func(f *fakeComfy) { f.script = func(ws *websocket.Conn, pid string) {} }) // never any event
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_error.json") })
	c := f.client("")
	var lb logBuf
	t0 := time.Now()
	res := runOK(t, c, RunOptions{Log: lb.f, StallTimeout: 200 * time.Millisecond})
	if el := time.Since(t0); el > 5*time.Second {
		t.Errorf("stall detection took %v", el)
	}
	if res.OK || res.Err != "no progress for 0s (stalled)" {
		t.Errorf("OK=%v Err=%q", res.OK, res.Err)
	}
	if f.interruptCount() != 1 {
		t.Errorf("interrupts=%d want 1", f.interruptCount())
	}
	if d := f.dequeuedIDs(); !reflect.DeepEqual(d, [][]string{{f.pid}}) {
		t.Errorf("dequeued=%v want [[%s]]", d, f.pid)
	}
	if !lb.has("[h3gen] no progress for 0s (stalled); interrupting " + f.pid) {
		t.Errorf("log: %q", lb.all())
	}
}

// Events of another prompt must not reset the stall clock.
func TestRunStallIgnoresForeignEvents(t *testing.T) {
	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		for i := 0; ; i++ {
			if err := send(ws, ev("progress", map[string]any{"value": i, "max": 100, "prompt_id": "other-prompt", "node": "sample"})); err != nil {
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_error.json") })
	t0 := time.Now()
	res := runOK(t, f.client(""), RunOptions{Log: (&logBuf{}).f, StallTimeout: 300 * time.Millisecond})
	if el := time.Since(t0); el > 5*time.Second {
		t.Errorf("stall detection took %v", el)
	}
	if res.OK || res.Err != "no progress for 0s (stalled)" || f.interruptCount() != 1 {
		t.Errorf("OK=%v Err=%q interrupts=%d", res.OK, res.Err, f.interruptCount())
	}
}

func TestRunTimeout(t *testing.T) {
	shortReads(t, 50*time.Millisecond)
	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		send(ws, ev("executing", map[string]any{"node": "unet", "display_node": "unet", "prompt_id": pid}))
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_error.json") })
	res := runOK(t, f.client(""), RunOptions{Log: (&logBuf{}).f, Timeout: 300 * time.Millisecond})
	if res.OK || res.Err != "timed out after 0s" {
		t.Errorf("OK=%v Err=%q", res.OK, res.Err)
	}
	if f.interruptCount() != 1 || !reflect.DeepEqual(f.dequeuedIDs(), [][]string{{f.pid}}) {
		t.Errorf("interrupts=%d dequeued=%v", f.interruptCount(), f.dequeuedIDs())
	}
}

func TestRunWebsocketClosedByServer(t *testing.T) {
	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		send(ws, ev("executing", map[string]any{"node": "unet", "display_node": "unet", "prompt_id": pid}))
		time.Sleep(20 * time.Millisecond)
		ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "restarting"))
		ws.Close()
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = "" }) // the server is gone: /history 404s
	var lb logBuf
	res := runOK(t, f.client(""), RunOptions{Log: lb.f})
	if res.OK || !strings.HasPrefix(res.Err, "websocket closed: ") {
		t.Errorf("OK=%v Err=%q", res.OK, res.Err)
	}
	if len(res.NodeTimes) != 0 { // the running node is only accounted on the next executing event
		t.Errorf("NodeTimes=%v", res.NodeTimes)
	}
	if !lb.has("[h3gen] history fetch failed: ") {
		t.Errorf("log: %q", lb.all())
	}
	if f.interruptCount() != 0 {
		t.Errorf("interrupt called on a closed socket")
	}
}

func TestRunServerUnreachable(t *testing.T) {
	old := wsReadTimeout
	wsReadTimeout = 150 * time.Millisecond
	defer func() { wsReadTimeout = old }()

	f := newFake(t)
	f.set(func(f *fakeComfy) { f.script = func(ws *websocket.Conn, pid string) {} })
	f.set(func(f *fakeComfy) { f.history = "" })
	c := f.client("")
	f.set(func(f *fakeComfy) { f.alive = false }) // set after the client dial; /system_stats now fails
	t0 := time.Now()
	res := runOK(t, c, RunOptions{Log: (&logBuf{}).f})
	if el := time.Since(t0); el > 5*time.Second {
		t.Errorf("took %v", el)
	}
	if res.OK || res.Err != "server unreachable" || f.interruptCount() != 0 {
		t.Errorf("OK=%v Err=%q interrupts=%d", res.OK, res.Err, f.interruptCount())
	}
}

// A read timeout while the server still answers keeps waiting.
func TestRunReadTimeoutKeepsWaiting(t *testing.T) {
	old := wsReadTimeout
	wsReadTimeout = 100 * time.Millisecond
	defer func() { wsReadTimeout = old }()

	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		time.Sleep(350 * time.Millisecond) // several read timeouts
		send(ws, ev("executing", map[string]any{"node": nil, "prompt_id": pid}))
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_ok.json") })
	res := runOK(t, f.client(""), RunOptions{Log: (&logBuf{}).f})
	if !res.OK || res.Err != "" {
		t.Errorf("OK=%v Err=%q", res.OK, res.Err)
	}
}

func TestRunInterrupted(t *testing.T) {
	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		send(ws, ev("executing", map[string]any{"node": "unet", "display_node": "unet", "prompt_id": pid}))
		send(ws, ev("execution_interrupted", map[string]any{"prompt_id": pid, "node_id": "unet", "node_type": "UNETLoader", "executed": []any{}}))
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_error.json") })
	res := runOK(t, f.client(""), RunOptions{Log: (&logBuf{}).f})
	if res.OK || res.Err != "interrupted" {
		t.Errorf("OK=%v Err=%q", res.OK, res.Err)
	}
}

// A rejected graph is an error return: no RunResult for the caller to treat as a failed render, no
// history fetch, nothing interrupted.
func TestRunQueueFailure(t *testing.T) {
	f := newFake(t)
	f.set(func(f *fakeComfy) { f.promptStatus = 400 })
	f.set(func(f *fakeComfy) { f.promptBody = invalidPromptJSON })
	res, err := f.client("").Run(context.Background(), testGraph(), RunOptions{Log: (&logBuf{}).f})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "Node ID '#save'") {
		t.Fatalf("err = %v, want the status and ComfyUI's message", err)
	}
	if res.OK || res.PromptID != "" || res.Err != "" {
		t.Errorf("OK=%v PromptID=%q Err=%q", res.OK, res.PromptID, res.Err)
	}
	if f.interruptCount() != 0 || len(f.dequeuedIDs()) != 0 {
		t.Errorf("interrupts=%d dequeued=%v on a queue failure", f.interruptCount(), f.dequeuedIDs())
	}
}

// A failed websocket connect is an error return too (ws.connect raised in the Python).
func TestRunConnectFailure(t *testing.T) {
	f := newFake(t)
	c := f.client("")
	f.srv.Close()
	res, err := c.Run(context.Background(), testGraph(), RunOptions{Log: (&logBuf{}).f})
	if err == nil || !strings.HasPrefix(err.Error(), "websocket connect: ") {
		t.Fatalf("err = %v, want \"websocket connect: ...\"", err)
	}
	if res.OK || res.Err != "" {
		t.Errorf("OK=%v Err=%q", res.OK, res.Err)
	}
}

// A cancelled context returns at once with ctx.Err() (Python: KeyboardInterrupt propagated out of run()):
// the prompt is left alone (no interrupt, no dequeue, no history fetch), the socket is closed.
func TestRunCancelled(t *testing.T) {
	f := newFake(t)
	f.set(func(f *fakeComfy) {
		f.script = func(ws *websocket.Conn, pid string) {
			send(ws, ev("executing", map[string]any{"node": "unet", "display_node": "unet", "prompt_id": pid}))
			time.Sleep(2 * time.Second)
		}
	})
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_ok.json") })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	t0 := time.Now()
	res, err := f.client("").Run(ctx, testGraph(), RunOptions{Log: (&logBuf{}).f})
	if el := time.Since(t0); el > time.Second {
		t.Errorf("Run returned after %v, want promptly after cancel", el)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.OK || res.PromptID != f.pid {
		t.Errorf("OK=%v PromptID=%q", res.OK, res.PromptID)
	}
	if f.interruptCount() != 0 || len(f.dequeuedIDs()) != 0 {
		t.Errorf("interrupts=%d dequeued=%v after a cancel", f.interruptCount(), f.dequeuedIDs())
	}
}

// The deadline is only consulted between reads (Python: ws.settimeout(60), the loop condition after recv):
// a job finishing inside the read window after the deadline still counts as finished, and a silent socket
// is given up only once the read times out.
func TestRunDeadlineCheckedAfterRead(t *testing.T) {
	old := wsReadTimeout
	wsReadTimeout = 600 * time.Millisecond
	defer func() { wsReadTimeout = old }()

	f := newFake(t)
	script := func(ws *websocket.Conn, pid string) {
		time.Sleep(250 * time.Millisecond) // past the 100 ms deadline, inside the read window
		send(ws, ev("executing", map[string]any{"node": nil, "prompt_id": pid}))
	}
	f.set(func(f *fakeComfy) { f.script = script })
	f.set(func(f *fakeComfy) { f.history = fixture(t, "history_ok.json") })
	res := runOK(t, f.client(""), RunOptions{Log: (&logBuf{}).f, Timeout: 100 * time.Millisecond})
	if !res.OK || res.Err != "" || f.interruptCount() != 0 {
		t.Errorf("OK=%v Err=%q interrupts=%d; want a clean finish", res.OK, res.Err, f.interruptCount())
	}

	f2 := newFake(t)
	f2.set(func(f *fakeComfy) { f.script = func(ws *websocket.Conn, pid string) {} }) // silent
	f2.set(func(f *fakeComfy) { f.history = fixture(t, "history_error.json") })
	t0 := time.Now()
	res = runOK(t, f2.client(""), RunOptions{Log: (&logBuf{}).f, Timeout: 100 * time.Millisecond})
	el := time.Since(t0)
	if res.OK || res.Err != "timed out after 0s" || f2.interruptCount() != 1 {
		t.Errorf("OK=%v Err=%q interrupts=%d", res.OK, res.Err, f2.interruptCount())
	}
	if el < wsReadTimeout {
		t.Errorf("gave up after %v, before the %v read timeout", el, wsReadTimeout)
	}
}

// ---- Queue ----------------------------------------------------------------

func TestQueue(t *testing.T) {
	f := newFake(t)
	c := f.client("")
	pid, err := c.Queue(testGraph())
	if err != nil || pid != f.pid {
		t.Fatalf("pid=%q err=%v", pid, err)
	}
	f.mu.Lock()
	body := f.prompts[0]
	f.mu.Unlock()
	if body["client_id"] != c.ClientID {
		t.Errorf("client_id=%v", body["client_id"])
	}
	g := body["prompt"].(map[string]any)["save"].(map[string]any)
	if g["class_type"] != "SaveVideo" || g["inputs"].(map[string]any)["filename_prefix"] != "h3/h3" {
		t.Errorf("graph=%v", g)
	}
}

func TestQueueErrorPropagation(t *testing.T) {
	f := newFake(t)
	f.set(func(f *fakeComfy) { f.promptStatus = 400 })
	f.set(func(f *fakeComfy) { f.promptBody = invalidPromptJSON })
	c := f.client("")
	pid, err := c.Queue(testGraph())
	if err == nil || pid != "" {
		t.Fatalf("pid=%q err=%v", pid, err)
	}
	// ComfyUI answers a rejected graph with a 400 whose body names the failing node: both reach the error.
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "Node ID '#save'") {
		t.Errorf("err=%q, want the status and the body", err)
	}
	// A long body is cut so a log line stays readable.
	long := `{"error": {"message": "` + strings.Repeat("あ", 3000) + `"}, "node_errors": {}}`
	f.set(func(f *fakeComfy) { f.promptBody = long })
	if _, err := c.Queue(testGraph()); err == nil || len([]rune(err.Error())) > 2100 || !strings.Contains(err.Error(), "あああ") {
		t.Errorf("400 with a long body: %d runes", len([]rune(err.Error())))
	}

	// A 200 carrying "error" is an error too (the body quoted, cut at 2000 characters).
	f.set(func(f *fakeComfy) { f.promptStatus = 200 })
	f.set(func(f *fakeComfy) { f.promptBody = invalidPromptJSON })
	if _, err := c.Queue(testGraph()); err == nil || !strings.Contains(err.Error(), "Node ID '#save'") {
		t.Errorf("err=%v", err)
	}
	f.set(func(f *fakeComfy) { f.promptBody = long })
	if _, err := c.Queue(testGraph()); err == nil || len([]rune(err.Error())) > 2100 {
		t.Errorf("truncation: %d runes", len([]rune(err.Error())))
	}

	// Any other status is an error.
	f.set(func(f *fakeComfy) { f.promptStatus = 500 })
	f.set(func(f *fakeComfy) { f.promptBody = "boom" })
	if _, err := c.Queue(testGraph()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err=%v", err)
	}

	// No prompt_id in a 200.
	f.set(func(f *fakeComfy) { f.promptStatus = 200 })
	f.set(func(f *fakeComfy) { f.promptBody = `{"number": 1}` })
	if _, err := c.Queue(testGraph()); err == nil || !strings.Contains(err.Error(), `{"number": 1}`) {
		t.Errorf("err=%v", err)
	}

	// Server gone.
	f.srv.Close()
	if _, err := c.Queue(testGraph()); err == nil {
		t.Errorf("expected a connection error")
	}
}

// ---- Upload ---------------------------------------------------------------

func TestUpload(t *testing.T) {
	f := newFake(t)
	c := f.client("")
	src := filepath.Join(t.TempDir(), "pic.png")
	os.WriteFile(src, []byte("\x89PNG fake"), 0o644)

	name, err := c.Upload(src, "image", "", true)
	if err != nil || name != "h3gen/pic.png" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	f.mu.Lock()
	up := f.uploads[0]
	f.mu.Unlock()
	if up.filename != "pic.png" || string(up.blob) != "\x89PNG fake" || up.partType != "application/octet-stream" {
		t.Errorf("file part: %+v", up)
	}
	if up.subfolder != "h3gen" || up.overwrite != "true" || up.typ != "input" {
		t.Errorf("fields: subfolder=%q overwrite=%q type=%q", up.subfolder, up.overwrite, up.typ)
	}
	if !strings.HasPrefix(up.contentType, "multipart/form-data; boundary=----h3gen") {
		t.Errorf("Content-Type=%q", up.contentType)
	}

	// Explicit subfolder, overwrite false, audio kind; the server reports no subfolder -> bare name.
	f.set(func(f *fakeComfy) { f.uploadSubfolder = "" })
	name, err = c.Upload(src, "audio", "refs", false)
	if err != nil || name != "pic.png" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	f.mu.Lock()
	up = f.uploads[1]
	f.mu.Unlock()
	if up.subfolder != "refs" || up.overwrite != "false" || up.typ != "input" {
		t.Errorf("fields: subfolder=%q overwrite=%q type=%q", up.subfolder, up.overwrite, up.typ)
	}

	if _, err := c.Upload(filepath.Join(t.TempDir(), "missing.png"), "image", "", true); err == nil {
		t.Errorf("missing file must fail")
	}
}

// ---- the small calls --------------------------------------------------------

func TestAliveStatsAndSilentPosts(t *testing.T) {
	f := newFake(t)
	c := f.client("")
	if !c.Alive() {
		t.Errorf("Alive() false")
	}
	st, err := c.SystemStats()
	if sys, _ := st["system"].(map[string]any); err != nil || sys["os"] != "nt" {
		t.Errorf("SystemStats=%v err=%v", st, err)
	}
	c.Free(true, false)
	c.Interrupt()
	c.Dequeue("abc")
	f.mu.Lock()
	frees, interrupts, dequeued := f.frees, f.interrupts, f.dequeued
	f.mu.Unlock()
	if !reflect.DeepEqual(frees, []map[string]any{{"unload_models": true, "free_memory": false}}) {
		t.Errorf("frees=%v", frees)
	}
	if interrupts != 1 || !reflect.DeepEqual(dequeued, [][]string{{"abc"}}) {
		t.Errorf("interrupts=%d dequeued=%v", interrupts, dequeued)
	}

	f.set(func(f *fakeComfy) { f.alive = false })
	if c.Alive() {
		t.Errorf("Alive() true on a 503")
	}
	if _, err := c.SystemStats(); err == nil {
		t.Errorf("SystemStats must fail on a 503")
	}
	f.srv.Close()
	if c.Alive() {
		t.Errorf("Alive() true on a closed server")
	}
	c.Free(true, true) // errors swallowed
	c.Interrupt()
	c.Dequeue("x")
}

func TestNew(t *testing.T) {
	c := New("127.0.0.1", 8188, "")
	if c.Base != "http://127.0.0.1:8188" || c.Host != "127.0.0.1" || c.Port != 8188 {
		t.Errorf("%+v", c)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(c.ClientID) {
		t.Errorf("ClientID=%q", c.ClientID)
	}
	if New("127.0.0.1", 1, "").ClientID == c.ClientID {
		t.Errorf("ClientID not random")
	}
	want := ""
	if st, err := os.Stat(paths.Output()); err == nil && st.IsDir() {
		want = paths.Output()
	}
	if c.OutputDir != want {
		t.Errorf("OutputDir=%q want %q (paths.Output() when it exists)", c.OutputDir, want)
	}
	if d := New("10.0.0.5", 8188, "").OutputDir; d != "" {
		t.Errorf("remote host got OutputDir %q", d)
	}
	dir := t.TempDir()
	if d := New("localhost", 8188, dir).OutputDir; d != dir {
		t.Errorf("explicit OutputDir=%q want %q", d, dir)
	}
	if d := New("localhost", 8188, filepath.Join(dir, "missing")).OutputDir; d != "" {
		t.Errorf("missing OutputDir kept: %q", d)
	}
	file := filepath.Join(dir, "f")
	os.WriteFile(file, []byte("x"), 0o644)
	if d := New("10.0.0.5", 8188, file).OutputDir; d != "" {
		t.Errorf("file as OutputDir kept: %q", d)
	}
}

// ---- helpers ------------------------------------------------------------------

func TestHelpers(t *testing.T) {
	cases := map[any]string{nil: "", "s": "s", true: "true", json.Number("5"): "5", json.Number("2.5"): "2.5", 3.0: "3", 0.5: "0.5"}
	for in, want := range cases {
		if got := text(in); got != want {
			t.Errorf("text(%v)=%q want %q", in, got, want)
		}
	}
	if got := truncRunes("あいう", 2); got != "あい" {
		t.Errorf("truncRunes=%q", got)
	}
	if got := truncRunes("ab", 5); got != "ab" {
		t.Errorf("truncRunes=%q", got)
	}
	if got := tracebackTail([]any{"a", "b", "c"}, 2); got != "b\nc" {
		t.Errorf("tracebackTail=%q", got)
	}
	if got := tracebackTail(nil, 15); got != "" {
		t.Errorf("tracebackTail(nil)=%q", got)
	}
	dir := t.TempDir()
	if !samePathIn(filepath.Join(dir, "A.mp4"), []string{filepath.Join(dir, "a.mp4")}) {
		t.Errorf("samePathIn must be case-insensitive")
	}
	if samePathIn(filepath.Join(dir, "b.mp4"), []string{filepath.Join(dir, "a.mp4")}) {
		t.Errorf("samePathIn false positive")
	}
}
