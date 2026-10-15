package comfy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/firemio/generate-local-H3/internal/logx"
	"github.com/firemio/generate-local-H3/internal/paths"
)

// Output is one file ComfyUI reports in /history (videos, gifs, images or audio entries).
type Output struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// RunResult is what Run returns.
type RunResult struct {
	PromptID string
	OK       bool
	// Seconds is the wall time of execution only (from the first "executing" event; queue wait excluded).
	Seconds   float64
	NodeTimes map[string]float64 // node id -> seconds
	// NodeOrder lists the node ids in execution order (first "executing" event of each).
	NodeOrder []string
	Outputs   []Output
	Err       string // "" when OK
	// LocalFiles are the output files resolved on disk (ComfyUI's output dir when local, else downloaded).
	LocalFiles []string
}

// Client is a minimal ComfyUI HTTP/WebSocket client.
type Client struct {
	Host     string
	Port     int
	Base     string // http://host:port
	ClientID string // random hex, sent as client_id with every /prompt and as ?clientId= on /ws
	// OutputDir is ComfyUI's local output directory ("" when ComfyUI is remote): outputs are resolved there
	// directly, never re-downloaded (see Run).
	OutputDir string
}

// Per-call timeouts.
const (
	aliveTimeout      = 5 * time.Second
	getTimeout        = 30 * time.Second
	postTimeout       = 60 * time.Second
	uploadTimeout     = 120 * time.Second
	viewTimeout       = 600 * time.Second
	defaultRunTimeout = 3 * time.Hour
)

// Websocket timeouts; variables only so the tests can shorten them.
var (
	wsHandshakeTimeout = 30 * time.Second
	wsReadTimeout      = 60 * time.Second
)

// New creates a client. outputDir "" means: when host is 127.0.0.1/localhost and <root>/output exists, use it
// (paths.Output()); otherwise no local output dir. An outputDir that is not a directory is dropped too.
func New(host string, port int, outputDir string) *Client {
	if outputDir == "" && (host == "127.0.0.1" || host == "localhost") {
		outputDir = paths.Output()
	}
	if outputDir != "" {
		if st, err := os.Stat(outputDir); err != nil || !st.IsDir() {
			outputDir = ""
		}
	}
	return &Client{
		Host:      host,
		Port:      port,
		Base:      fmt.Sprintf("http://%s:%d", host, port),
		ClientID:  randomHex(),
		OutputDir: outputDir,
	}
}

// randomHex is 32 lowercase hex digits (a uuid4-style client id).
func randomHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b[:])
}

// ---- basic HTTP ---------------------------------------------------------

// request performs one HTTP call with a total timeout and returns the body. A status outside 2xx is an
// error carrying the status line and the body (ComfyUI answers a rejected graph with a 400 whose body
// names the failing node).
func (c *Client) request(method, path string, body []byte, contentType string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: HTTP %s: %s", method, path, resp.Status, truncRunes(strings.TrimSpace(string(raw)), 2000))
	}
	return raw, nil
}

// getJSON is GET path, decoding the JSON body into out.
func (c *Client) getJSON(path string, timeout time.Duration, out any) error {
	raw, err := c.request(http.MethodGet, path, nil, "", timeout)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("GET %s: bad JSON: %w", path, err)
	}
	return nil
}

// postSilently posts to the endpoints whose result is ignored (/free, /interrupt, /queue); every error is
// swallowed (ComfyUI answers those with an empty 200).
func (c *Client) postSilently(path string, payload any) {
	body, err := encodeJSON(payload)
	if err != nil {
		return
	}
	_, _ = c.request(http.MethodPost, path, body, "application/json", postTimeout)
}

// Alive is GET /system_stats with a 5 s timeout.
func (c *Client) Alive() bool {
	var v any
	return c.getJSON("/system_stats", aliveTimeout, &v) == nil
}

// SystemStats is GET /system_stats.
func (c *Client) SystemStats() (map[string]any, error) {
	var v map[string]any
	if err := c.getJSON("/system_stats", getTimeout, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Free is POST /free {"unload_models": .., "free_memory": ..}; errors are swallowed.
func (c *Client) Free(unloadModels, freeMemory bool) {
	c.postSilently("/free", struct {
		UnloadModels bool `json:"unload_models"`
		FreeMemory   bool `json:"free_memory"`
	}{unloadModels, freeMemory})
}

// Interrupt is POST /interrupt {}: cancel whatever is executing. Used before giving up on a wedged prompt,
// otherwise the job keeps the GPU and every later prompt queues behind it forever. Errors are swallowed.
func (c *Client) Interrupt() {
	c.postSilently("/interrupt", struct{}{})
}

// Dequeue is POST /queue {"delete": [promptID]}; errors are swallowed.
func (c *Client) Dequeue(promptID string) {
	c.postSilently("/queue", struct {
		Delete []string `json:"delete"`
	}{[]string{promptID}})
}

// ---- uploads ------------------------------------------------------------

// Upload posts a file to /upload/image (multipart: image=<file>, subfolder, overwrite, type=input) and returns
// the name to use in LoadImage/LoadAudio ("<subfolder>/<name>" when a subfolder is reported). kind ("image" |
// "audio") is unused by ComfyUI. Default subfolder "h3gen" (when "" is given).
func (c *Client) Upload(path, kind, subfolder string, overwrite bool) (string, error) {
	_ = kind
	if subfolder == "" {
		subfolder = "h3gen"
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.SetBoundary("----h3gen" + randomHex()); err != nil {
		return "", err
	}
	fw, err := w.CreateFormFile("image", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(blob); err != nil {
		return "", err
	}
	ow := "false"
	if overwrite {
		ow = "true"
	}
	for _, kv := range [][2]string{{"subfolder", subfolder}, {"overwrite", ow}, {"type", "input"}} {
		if err := w.WriteField(kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	raw, err := c.request(http.MethodPost, "/upload/image", body.Bytes(), w.FormDataContentType(), uploadTimeout)
	if err != nil {
		return "", err
	}
	var info struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", fmt.Errorf("POST /upload/image: bad JSON: %w", err)
	}
	if info.Name == "" {
		return "", errors.New("POST /upload/image: no name in response")
	}
	if info.Subfolder != "" {
		return info.Subfolder + "/" + info.Name, nil
	}
	return info.Name, nil
}

// ---- run ----------------------------------------------------------------

// Queue posts {"prompt": graph, "client_id": ...} to /prompt and returns the prompt id. A rejected graph
// (a non-2xx status, or a 2xx body carrying "error") is an error that quotes the body, cut at 2000 characters.
func (c *Client) Queue(graph Graph) (string, error) {
	body, err := encodeJSON(struct {
		Prompt   Graph  `json:"prompt"`
		ClientID string `json:"client_id"`
	}{graph, c.ClientID})
	if err != nil {
		return "", err
	}
	raw, err := c.request(http.MethodPost, "/prompt", body, "application/json", postTimeout)
	if err != nil {
		return "", err
	}
	var res struct {
		PromptID string `json:"prompt_id"`
		Error    any    `json:"error"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("POST /prompt: bad JSON: %w", err)
	}
	if res.Error != nil || res.PromptID == "" {
		return "", fmt.Errorf("POST /prompt: %s", truncRunes(strings.TrimSpace(string(raw)), 2000))
	}
	return res.PromptID, nil
}

// RunOptions tune Run.
type RunOptions struct {
	// Timeout for the whole run (0 = 3 h).
	Timeout time.Duration
	// StallTimeout: give up after this long without any event for this prompt (0 = never). A HIP hang on
	// ROCm/Windows leaves the HTTP server answering while execution never progresses, so a liveness check
	// alone would wait out the full Timeout. On give-up the prompt is interrupted and dequeued.
	StallTimeout time.Duration
	// DownloadDir: when set and the output is not found locally, fetch it via /view into this directory.
	DownloadDir string
	// Log receives progress lines ("[h3gen] ..."); nil = logx.Printf. Use logx.Discard to silence.
	Log logx.Logger
}

// wsMsg is one frame from the reader goroutine (err set once, then the goroutine exits).
type wsMsg struct {
	data   []byte
	binary bool
	err    error
}

// readLoop feeds every websocket frame to msgs until the connection fails or done is closed. gorilla makes
// any read error sticky (a read deadline included), so the 60 s "no frame" check is done by Run with a
// timer instead of a deadline.
func readLoop(conn *websocket.Conn, msgs chan<- wsMsg, done <-chan struct{}) {
	for {
		mt, data, err := conn.ReadMessage()
		m := wsMsg{data: data, binary: mt == websocket.BinaryMessage, err: err}
		select {
		case msgs <- m:
		case <-done:
			return
		}
		if err != nil {
			return
		}
	}
}

// progressKey deduplicates "step v/max" lines per (node, value).
type progressKey struct {
	node, value string
}

// Run queues the graph, follows the websocket events (ws://host:port/ws?clientId=<ClientID>, connect timeout
// 30 s, read timeout 60 s — on a read timeout check Alive(): unreachable -> "server unreachable", else keep
// waiting; a closed socket -> "websocket closed: ..."), times each node from "executing" events (node null =
// finished), logs "executing <node>" and "step v/max" progress (deduplicated per (node, value)), records
// execution_error ("<node_type>: <exception_message>", plus the last 15 traceback lines to the log) and
// execution_interrupted ("interrupted"). Events whose prompt_id is present and not ours are ignored (and do
// not reset the stall clock). Binary frames (previews) are skipped. On Timeout the prompt is interrupted and
// dequeued ("timed out after Ns"). The deadline and the stall clock are only consulted between reads: a read
// blocks for up to the 60 s read timeout first, so a job that finishes inside that window still counts as
// finished.
//
// Afterwards GET /history/<pid>: every outputs.*.{videos,gifs,images,audio}[] entry becomes an Output; a
// status.status_str == "error" flips OK to false ("history reports error" when no error was recorded).
// Outputs are resolved to local files: <OutputDir>/<subfolder>/<filename> when it exists (never re-download
// into the same folder: /view + aiohttp sendfile on Windows is flaky and a colliding path would truncate the
// fresh render), else downloaded via /view?filename=&subfolder=&type= into DownloadDir when given.
// OK = finished cleanly (executing node null seen) and no error.
//
// The error return covers the failures that abandon the run before or after execution: the websocket
// connect ("websocket connect: ..."), Queue (its error text), a failed download ("download failed: ...", the
// remaining outputs are left unresolved) and a cancelled ctx (ctx.Err(); the socket is closed, the prompt is
// left running, nothing is interrupted or dequeued). Every other failure is reported in RunResult.Err with a
// nil error.
func (c *Client) Run(ctx context.Context, graph Graph, opt RunOptions) (RunResult, error) {
	log := opt.Log
	if log == nil {
		log = logx.Printf
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	res := RunResult{NodeTimes: map[string]float64{}, NodeOrder: []string{}, Outputs: []Output{}, LocalFiles: []string{}}

	// Connect before queueing so no event of ours is missed.
	dialer := websocket.Dialer{HandshakeTimeout: wsHandshakeTimeout, Proxy: http.ProxyFromEnvironment}
	wsURL := fmt.Sprintf("ws://%s:%d/ws?clientId=%s", c.Host, c.Port, url.QueryEscape(c.ClientID))
	conn, resp, err := dialer.Dial(wsURL, nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return res, fmt.Errorf("websocket connect: %w", err)
	}
	t0 := time.Now()
	pid, err := c.Queue(graph)
	if err != nil {
		conn.Close()
		return res, err
	}
	res.PromptID = pid
	log("[h3gen] queued %s", pid)

	msgs := make(chan wsMsg, 64)
	done := make(chan struct{})
	go readLoop(conn, msgs, done)

	var (
		cur       string // node being executed (curSet false = none yet)
		curSet    bool
		curT      = t0
		ok        bool
		errText   string
		lastProg  = progressKey{"", "-1"}
		tExec     time.Time // first executing event of this prompt (excludes queue wait)
		lastEvent = time.Now()
		deadline  = t0.Add(timeout)
	)
loop:
	for {
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		if opt.StallTimeout > 0 && now.Sub(lastEvent) > opt.StallTimeout {
			errText = fmt.Sprintf("no progress for %.0fs (stalled)", opt.StallTimeout.Seconds())
			log("[h3gen] %s; interrupting %s", errText, pid)
			c.Interrupt()
			c.Dequeue(pid)
			break
		}
		// Wait for the next frame for up to the read timeout; the deadline and the stall clock above are
		// only re-checked once a read returns.
		var m wsMsg
		select {
		case m = <-msgs:
		case <-ctx.Done():
			close(done)
			conn.Close()
			return res, ctx.Err()
		case <-time.After(wsReadTimeout):
			if !c.Alive() {
				errText = "server unreachable"
				break loop
			}
			continue
		}
		if m.err != nil { // socket closed by a restarting server
			errText = "websocket closed: " + m.err.Error()
			break
		}
		if m.binary {
			continue // preview images
		}
		var ev map[string]any
		dec := json.NewDecoder(bytes.NewReader(m.data))
		dec.UseNumber()
		if err := dec.Decode(&ev); err != nil || ev == nil {
			continue
		}
		typ, _ := ev["type"].(string)
		data, _ := ev["data"].(map[string]any)
		if v, has := data["prompt_id"]; has && v != nil {
			if s, isStr := v.(string); !isStr || s != pid {
				continue
			}
		}
		lastEvent = time.Now()
		now = lastEvent
		switch typ {
		case "executing":
			node := data["node"]
			if tExec.IsZero() && node != nil {
				tExec = now
			}
			if curSet {
				if _, seen := res.NodeTimes[cur]; !seen {
					res.NodeOrder = append(res.NodeOrder, cur)
				}
				res.NodeTimes[cur] += now.Sub(curT).Seconds()
			}
			if node == nil {
				ok = errText == ""
				break loop
			}
			cur, curSet, curT = text(node), true, now
			log("[h3gen] %7.1fs executing %s", now.Sub(t0).Seconds(), cur)
		case "progress":
			v, max := text(data["value"]), text(data["max"])
			key := progressKey{text(data["node"]), v}
			if key != lastProg {
				lastProg = key
				log("[h3gen] %7.1fs   step %s/%s", now.Sub(t0).Seconds(), v, max)
			}
		case "execution_error":
			errText = fmt.Sprintf("%s: %s", text(data["node_type"]), text(data["exception_message"]))
			log("[h3gen] ERROR %s", errText)
			log("%s", tracebackTail(data["traceback"], 15))
		case "execution_interrupted":
			errText = "interrupted"
			break loop
		}
	}
	close(done)
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	conn.Close()

	if errText == "" && !ok && time.Since(t0) >= timeout {
		errText = fmt.Sprintf("timed out after %.0fs", timeout.Seconds())
		c.Interrupt()
		c.Dequeue(pid)
	}
	start := t0
	if !tExec.IsZero() {
		start = tExec
	}
	res.Seconds = time.Since(start).Seconds() // wall time of execution only (queue wait excluded)

	outs, statusStr, err := c.history(pid)
	res.Outputs = append(res.Outputs, outs...)
	if err != nil {
		log("[h3gen] history fetch failed: %v", err)
	} else if statusStr == "error" {
		ok = false
		if errText == "" {
			errText = "history reports error"
		}
	}

	for _, item := range res.Outputs {
		if item.Filename == "" {
			continue
		}
		// ComfyUI runs on this machine: resolve the file directly in its output directory.
		// (Never re-download into the same folder: /view + aiohttp sendfile on Windows is flaky, and a
		//  colliding destination path would truncate the freshly rendered file.)
		if c.OutputDir != "" {
			p := filepath.Join(c.OutputDir, item.Subfolder, item.Filename)
			if isFile(p) {
				res.LocalFiles = append(res.LocalFiles, p)
				continue
			}
		}
		if opt.DownloadDir != "" {
			dst := filepath.Join(opt.DownloadDir, item.Filename)
			if samePathIn(dst, res.LocalFiles) {
				continue
			}
			if err := c.download(item, opt.DownloadDir, dst); err != nil {
				res.OK, res.Err = false, errText
				return res, fmt.Errorf("download failed: %w", err)
			}
			res.LocalFiles = append(res.LocalFiles, dst)
		}
	}
	res.OK = ok && errText == ""
	res.Err = errText
	return res, nil
}

// history is GET /history/<pid>: every outputs.<node>.{videos,gifs,images,audio}[] entry (nodes in id
// order) and status.status_str. An unknown prompt id has no outputs.
func (c *Client) history(pid string) ([]Output, string, error) {
	var top map[string]struct {
		Outputs map[string]map[string]json.RawMessage `json:"outputs"`
		Status  struct {
			StatusStr string `json:"status_str"`
		} `json:"status"`
	}
	if err := c.getJSON("/history/"+url.PathEscape(pid), getTimeout, &top); err != nil {
		return nil, "", err
	}
	hist := top[pid]
	var outs []Output
	for _, nid := range slices.Sorted(maps.Keys(hist.Outputs)) {
		for _, key := range []string{"videos", "gifs", "images", "audio"} {
			list, has := hist.Outputs[nid][key]
			if !has {
				continue
			}
			var items []Output
			if err := json.Unmarshal(list, &items); err != nil {
				return outs, hist.Status.StatusStr, fmt.Errorf("GET /history/%s: bad outputs.%s.%s: %w", pid, nid, key, err)
			}
			outs = append(outs, items...)
		}
	}
	return outs, hist.Status.StatusStr, nil
}

// download fetches one output via /view into dir (created when missing), writing <dst>.tmp then renaming.
func (c *Client) download(item Output, dir, dst string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	typ := item.Type
	if typ == "" {
		typ = "output"
	}
	q := url.Values{}
	q.Set("filename", item.Filename)
	q.Set("subfolder", item.Subfolder)
	q.Set("type", typ)
	ctx, cancel := context.WithTimeout(context.Background(), viewTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/view?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET /view: HTTP %s", resp.Status)
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// isFile reports whether p is a regular file.
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// samePathIn reports whether dst names a file already in list (absolute paths; case-insensitive, since
// Windows paths are — this guard exists to keep a download from truncating a fresh render).
func samePathIn(dst string, list []string) bool {
	a, err := filepath.Abs(dst)
	if err != nil {
		a = dst
	}
	for _, x := range list {
		b, err := filepath.Abs(x)
		if err != nil {
			b = x
		}
		if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
			return true
		}
	}
	return false
}

// text renders an event field for a log line: a string verbatim, a number as sent, a missing one empty.
func text(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// tracebackTail joins the last n lines of the traceback list ComfyUI puts in an execution_error event.
func tracebackTail(v any, n int) string {
	switch x := v.(type) {
	case []any:
		if len(x) > n {
			x = x[len(x)-n:]
		}
		lines := make([]string, len(x))
		for i, l := range x {
			lines[i] = text(l)
		}
		return strings.Join(lines, "\n")
	case string:
		return x
	}
	return ""
}

// truncRunes cuts s to n characters (code points, not bytes).
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
