package llm

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// --- ExtractJSON ------------------------------------------------------------------------------------------

// extractCases: ok is false where no object can be found (prose, a bare list or scalar, null, broken JSON);
// want is the object re-encoded with sorted keys.
var extractCases = []struct {
	name string
	in   string
	ok   bool
	want string
}{
	{name: "plain", in: "{\"title\": \"朝のニュース\", \"sections\": [{\"name\": \"天気\", \"seconds\": 60}], \"ok\": true}", ok: true, want: "{\"ok\":true,\"sections\":[{\"name\":\"天気\",\"seconds\":60}],\"title\":\"朝のニュース\"}"},
	{name: "fenced_json", in: "```json\n{\"title\": \"Fenced\", \"n\": 3}\n```", ok: true, want: "{\"n\":3,\"title\":\"Fenced\"}"},
	{name: "fenced_plain", in: "```\n{\"title\": \"Bare fence\"}\n```", ok: true, want: "{\"title\":\"Bare fence\"}"},
	{name: "fenced_ws", in: "  \n```json   \n\n{\"a\": 1}\n\n```\n\n", ok: true, want: "{\"a\":1}"},
	{name: "prefix_suffix", in: "Sure! Here is the JSON you asked for:\n{\"title\": \"Wrapped\", \"cast\": [{\"name\": \"Aoi\"}]}\nLet me know if you need anything else.", ok: true, want: "{\"cast\":[{\"name\":\"Aoi\"}],\"title\":\"Wrapped\"}"},
	{name: "prefix_fenced", in: "Here you go:\n```json\n{\"x\": \"y\"}\n```", ok: true, want: "{\"x\":\"y\"}"},
	{name: "nested_braces", in: "note {not json} then {\"outer\": {\"inner\": {\"deep\": 1}}, \"s\": \"a } b\"} trailing", ok: false},
	{name: "bare_list", in: "[{\"name\": \"a\"}, {\"name\": \"b\"}]", ok: false},
	{name: "bare_list_fenced", in: "```json\n[1, 2, 3]\n```", ok: false},
	{name: "string", in: "\"just a string\"", ok: false},
	{name: "number", in: "42", ok: false},
	{name: "bool", in: "true", ok: false},
	{name: "null", in: "null", ok: false},
	{name: "garbage", in: "I cannot help with that.", ok: false},
	{name: "unbalanced", in: "{\"title\": \"never closed\", \"sections\": [", ok: false},
	{name: "two_objects", in: "{\"a\": 1} and {\"b\": 2}", ok: false},
	{name: "empty", in: "", ok: false},
	{name: "empty_object", in: "{}", ok: true, want: "{}"},
	{name: "trailing_comma", in: "{\"a\": 1,}", ok: false},
	{name: "single_quotes", in: "{'a': 1}", ok: false},
	{name: "brace_only", in: "{", ok: false},
	{name: "fence_only", in: "```json```", ok: false},
	{name: "fence_lang", in: "```javascript\n{\"a\": 1}\n```", ok: true, want: "{\"a\":1}"},
	{name: "crlf", in: "```json\r\n{\"a\": \"b\"}\r\n```\r\n", ok: true, want: "{\"a\":\"b\"}"},
	{name: "dup_keys", in: "{\"a\": 1, \"a\": 2}", ok: true, want: "{\"a\":2}"},
	{name: "escapes", in: `{"s": "\u3042\n<d>tag</d> \"q\""}`, ok: true, want: `{"s":"あ\n<d>tag</d> \"q\""}`},
	{name: "raw_newline_in_string", in: "{\"s\": \"a\nb\"}", ok: false},
	{name: "seconds_string", in: "{\"seconds\": \"60\", \"flag\": \"true\", \"n\": 1.5}", ok: true, want: "{\"flag\":\"true\",\"n\":1.5,\"seconds\":\"60\"}"},
	{name: "list_then_object", in: "[1, 2] {\"a\": 1}", ok: true, want: "{\"a\":1}"},
	{name: "fullwidth_space", in: "　```json　\n{\"a\": 1}\n```　", ok: true, want: "{\"a\":1}"},
	{name: "fence_not_closed", in: "```json\n{\"a\": 1}", ok: true, want: "{\"a\":1}"},
	{name: "fence_prefix_only", in: "```json", ok: false},
}

// canonical re-encodes an object compactly with sorted keys and no HTML escaping.
func canonical(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func TestExtractJSON(t *testing.T) {
	for _, g := range extractCases {
		t.Run(g.name, func(t *testing.T) {
			v, ok := ExtractJSON(g.in)
			if ok != g.ok {
				t.Fatalf("ok = %v, want %v (v=%v)", ok, g.ok, v)
			}
			if !ok {
				if v != nil {
					t.Fatalf("v = %v, want nil when !ok", v)
				}
				return
			}
			if got := canonical(t, v); got != g.want {
				t.Fatalf("got  %s\nwant %s", got, g.want)
			}
		})
	}
}

func TestExtractJSONValueTypes(t *testing.T) {
	// The values are untyped: numbers are float64, bools bool, strings string, nested objects maps.
	obj, ok := ExtractJSON("```json\n{\"seconds\": 60, \"s\": \"60\", \"b\": true, \"o\": {\"k\": [1, \"x\"]}}\n```")
	if !ok {
		t.Fatal("not ok")
	}
	if n, _ := obj["seconds"].(float64); n != 60 {
		t.Errorf("seconds = %#v, want 60", obj["seconds"])
	}
	if v, _ := obj["s"].(string); v != "60" {
		t.Errorf("s = %#v, want \"60\"", obj["s"])
	}
	if v, _ := obj["b"].(bool); !v {
		t.Errorf("b = %#v, want true", obj["b"])
	}
	o, _ := obj["o"].(map[string]any)
	if l, _ := o["k"].([]any); len(l) != 2 {
		t.Errorf("o.k = %#v, want a 2-element list", o["k"])
	}
}

// --- New ------------------------------------------------------------------------------------------------

func TestNewDefaults(t *testing.T) {
	t.Setenv("AITV_LLM_URL", "")
	t.Setenv("AITV_LLM_MODEL", "")
	t.Setenv("AITV_LLM_NUM_GPU", "")
	c := New("", "", nil)
	if c.BaseURL != "http://127.0.0.1:11434" || c.Model != "gemma4:latest" || !c.IsOllama || c.NumGPU != 0 {
		t.Fatalf("defaults: %+v", *c)
	}
	if c.Timeout != 30*time.Minute {
		t.Fatalf("Timeout = %v, want 30m", c.Timeout)
	}
}

func TestNewEnv(t *testing.T) {
	t.Setenv("AITV_LLM_URL", "http://127.0.0.1:1234/v1/")
	t.Setenv("AITV_LLM_MODEL", "qwen3:8b")
	t.Setenv("AITV_LLM_NUM_GPU", " 12 ")
	c := New("", "", nil)
	if c.BaseURL != "http://127.0.0.1:1234/v1" {
		t.Errorf("BaseURL = %q (trailing slash must go)", c.BaseURL)
	}
	if c.Model != "qwen3:8b" || c.IsOllama || c.NumGPU != 12 {
		t.Errorf("env: %+v", *c)
	}
	// Explicit arguments win over the environment; a given numGPU is taken as is, negative ones included
	// (the Python passed the CLI int through; only None fell back to the environment).
	c = New("http://127.0.0.1:11434/", "gemma4:latest", intp(0))
	if c.BaseURL != "http://127.0.0.1:11434" || c.Model != "gemma4:latest" || !c.IsOllama || c.NumGPU != 0 {
		t.Errorf("explicit: %+v", *c)
	}
	if c := New("", "", intp(-1)); c.NumGPU != -1 {
		t.Errorf("NumGPU = %d, want -1 (Ollama's every-layer value) with AITV_LLM_NUM_GPU=12", c.NumGPU)
	}
	if c := New("", "", intp(-2)); c.NumGPU != -2 {
		t.Errorf("NumGPU = %d, want -2", c.NumGPU)
	}
	// A garbage AITV_LLM_NUM_GPU falls back to 0 rather than failing.
	t.Setenv("AITV_LLM_NUM_GPU", "many")
	if c := New("", "", nil); c.NumGPU != 0 {
		t.Errorf("NumGPU = %d, want 0 for a garbage env value", c.NumGPU)
	}
	// An Ollama root given as its OpenAI-compatible /v1 base is still Ollama.
	if c := New("http://localhost:11434/v1", "", intp(3)); !c.IsOllama || c.NumGPU != 3 {
		t.Errorf("11434/v1: %+v", *c)
	}
}

// --- Chat -----------------------------------------------------------------------------------------------

// capture records what a fake server saw and answers with a fixed body per request.
type capture struct {
	mu      sync.Mutex
	method  string
	path    string
	headers http.Header
	body    map[string]any
	raw     []byte
	calls   int
	// reply is called per request to produce (status, body).
	reply func(n int) (int, string)
}

func (cp *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		cp.calls++
		cp.method = r.Method
		cp.path = r.URL.Path
		cp.headers = r.Header.Clone()
		cp.raw, _ = io.ReadAll(r.Body)
		cp.body = nil
		_ = json.Unmarshal(cp.raw, &cp.body)
		status, body := cp.reply(cp.calls)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func ollamaReply(content string) string {
	b, _ := json.Marshal(map[string]any{"model": "m", "message": map[string]any{"role": "assistant", "content": content}, "done": true})
	return string(b)
}

func openAIReply(content string) string {
	b, _ := json.Marshal(map[string]any{"id": "x", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}})
	return string(b)
}

func messagesOf(t *testing.T, body map[string]any) []message {
	t.Helper()
	raw, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages missing or not a list: %#v", body["messages"])
	}
	var out []message
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		content, _ := mm["content"].(string)
		out = append(out, message{Role: role, Content: content})
	}
	return out
}

func TestChatOllama(t *testing.T) {
	unsetEnv(t, "AITV_LLM_KEY") // os.environ.get("AITV_LLM_KEY", "local"): absent -> "local"
	cp := &capture{reply: func(int) (int, string) { return 200, ollamaReply("{\"title\": \"こんにちは <d>x</d>\"}") }}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "gemma4:latest", Timeout: 10 * time.Second, IsOllama: true, NumGPU: 7}
	got, err := c.Chat("SYS <d>tag</d>", "USER 日本語", true, 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if got != "{\"title\": \"こんにちは <d>x</d>\"}" {
		t.Errorf("content = %q", got)
	}
	if cp.method != "POST" || cp.path != "/api/chat" {
		t.Errorf("%s %s, want POST /api/chat", cp.method, cp.path)
	}
	if h := cp.headers.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q", h)
	}
	if h := cp.headers.Get("Authorization"); h != "Bearer local" {
		t.Errorf("Authorization = %q, want \"Bearer local\"", h)
	}
	if cp.body["model"] != "gemma4:latest" {
		t.Errorf("model = %#v", cp.body["model"])
	}
	if stream, ok := cp.body["stream"].(bool); !ok || stream {
		t.Errorf("stream = %#v, want false (Ollama streams by default)", cp.body["stream"])
	}
	if cp.body["format"] != "json" {
		t.Errorf("format = %#v, want \"json\"", cp.body["format"])
	}
	opts, _ := cp.body["options"].(map[string]any)
	if opts["temperature"] != 0.9 || opts["num_ctx"] != float64(16384) || opts["num_gpu"] != float64(7) {
		t.Errorf("options = %#v", cp.body["options"])
	}
	if _, has := cp.body["temperature"]; has {
		t.Errorf("Ollama payload must not carry a top-level temperature")
	}
	if _, has := cp.body["response_format"]; has {
		t.Errorf("Ollama payload must not carry response_format")
	}
	msgs := messagesOf(t, cp.body)
	want := []message{{"system", "SYS <d>tag</d>"}, {"user", "USER 日本語"}}
	if len(msgs) != 2 || msgs[0] != want[0] || msgs[1] != want[1] {
		t.Errorf("messages = %+v, want %+v", msgs, want)
	}
	if !utf8.Valid(cp.raw) || !bytes.Contains(cp.raw, []byte("<d>tag</d>")) {
		t.Errorf("request body must be plain UTF-8 without HTML escaping: %s", cp.raw)
	}
	// Python dict order on the wire: model, messages, stream, options, format.
	order := []string{`"model"`, `"messages"`, `"stream"`, `"options"`, `"format"`}
	last := -1
	for _, k := range order {
		i := bytes.Index(cp.raw, []byte(k))
		if i < last {
			t.Errorf("key order: %s out of place in %s", k, cp.raw)
		}
		last = i
	}

	// jsonMode false: no "format"; a different temperature goes through.
	if _, err := c.Chat("s", "u", false, 0.2); err != nil {
		t.Fatal(err)
	}
	if _, has := cp.body["format"]; has {
		t.Errorf("format must be absent when jsonMode is false: %s", cp.raw)
	}
	opts, _ = cp.body["options"].(map[string]any)
	if opts["temperature"] != 0.2 {
		t.Errorf("temperature = %#v, want 0.2", opts["temperature"])
	}
}

func TestChatOllamaV1BaseAndKey(t *testing.T) {
	t.Setenv("AITV_LLM_KEY", "sk-test")
	cp := &capture{reply: func(int) (int, string) { return 200, ollamaReply("ok") }}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()
	// An Ollama base written as its /v1 endpoint: the trailing /v1 is stripped for /api/chat.
	c := &Client{BaseURL: srv.URL + "/v1", Model: "m", IsOllama: true}
	if got, err := c.Chat("s", "u", true, 0.9); err != nil || got != "ok" {
		t.Fatalf("got %q, %v", got, err)
	}
	if cp.path != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", cp.path)
	}
	if h := cp.headers.Get("Authorization"); h != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want \"Bearer sk-test\"", h)
	}
}

func TestChatOpenAI(t *testing.T) {
	unsetEnv(t, "AITV_LLM_KEY")
	cp := &capture{reply: func(int) (int, string) { return 200, openAIReply("{\"a\": 1}") }}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	// No /v1 on the base: it is appended.
	c := &Client{BaseURL: srv.URL, Model: "lmstudio-community/x", Timeout: 10 * time.Second}
	got, err := c.Chat("SYS", "USER", true, 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if got != "{\"a\": 1}" {
		t.Errorf("content = %q", got)
	}
	if cp.method != "POST" || cp.path != "/v1/chat/completions" {
		t.Errorf("%s %s, want POST /v1/chat/completions", cp.method, cp.path)
	}
	if h := cp.headers.Get("Authorization"); h != "Bearer local" {
		t.Errorf("Authorization = %q", h)
	}
	if h := cp.headers.Get("Content-Type"); h != "application/json" {
		t.Errorf("Content-Type = %q", h)
	}
	if cp.body["model"] != "lmstudio-community/x" || cp.body["temperature"] != 0.9 {
		t.Errorf("body = %s", cp.raw)
	}
	if stream, ok := cp.body["stream"].(bool); !ok || stream {
		t.Errorf("stream = %#v, want false", cp.body["stream"])
	}
	rf, _ := cp.body["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Errorf("response_format = %#v, want {\"type\":\"json_object\"}", cp.body["response_format"])
	}
	for _, k := range []string{"options", "format"} {
		if _, has := cp.body[k]; has {
			t.Errorf("OpenAI payload must not carry %q", k)
		}
	}
	msgs := messagesOf(t, cp.body)
	if len(msgs) != 2 || msgs[0] != (message{"system", "SYS"}) || msgs[1] != (message{"user", "USER"}) {
		t.Errorf("messages = %+v", msgs)
	}

	// jsonMode false: no response_format.
	if _, err := c.Chat("s", "u", false, 0.5); err != nil {
		t.Fatal(err)
	}
	if _, has := cp.body["response_format"]; has {
		t.Errorf("response_format must be absent when jsonMode is false: %s", cp.raw)
	}
	if cp.body["temperature"] != 0.5 {
		t.Errorf("temperature = %#v, want 0.5", cp.body["temperature"])
	}

	// A base that already ends in /v1 is used as is.
	c = &Client{BaseURL: srv.URL + "/v1", Model: "m"}
	if _, err := c.Chat("s", "u", true, 0.9); err != nil {
		t.Fatal(err)
	}
	if cp.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", cp.path)
	}
}

func TestChatErrors(t *testing.T) {
	cases := []struct {
		name   string
		ollama bool
		status int
		body   string
		want   string // substring of the error
	}{
		{"ollama 404 with body", true, 404, `{"error":"model 'nope' not found"}`, "HTTP Error 404: Not Found — {\"error\":\"model 'nope' not found\"}"},
		{"ollama 500", true, 500, "", "HTTP Error 500: Internal Server Error"},
		{"openai 401", false, 401, `{"error":{"message":"bad key"}}`, "HTTP Error 401: Unauthorized"},
		{"ollama malformed body", true, 200, "<html>not json</html>", "malformed reply"},
		{"ollama missing message", true, 200, `{"done": true}`, "malformed reply"},
		{"ollama content not a string", true, 200, `{"message": {"role": "assistant", "content": null}}`, "malformed reply"},
		{"openai empty choices", false, 200, `{"choices": []}`, "malformed reply"},
		{"openai missing content", false, 200, `{"choices": [{"message": {"role": "assistant"}}]}`, "malformed reply"},
		{"openai body is a list", false, 200, `[1, 2]`, "malformed reply"},
		{"openai body is null", false, 200, `null`, "malformed reply"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := &capture{reply: func(int) (int, string) { return tc.status, tc.body }}
			srv := httptest.NewServer(cp.handler())
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, Model: "m", IsOllama: tc.ollama}
			got, err := c.Chat("s", "u", true, 0.9)
			if err == nil {
				t.Fatalf("no error, got %q", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
			if got != "" {
				t.Errorf("content = %q with an error", got)
			}
		})
	}
}

func TestChatUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{BaseURL: url, Model: "m", IsOllama: true, Timeout: 2 * time.Second}
	if _, err := c.Chat("s", "u", true, 0.9); err == nil {
		t.Fatal("no error against a closed server")
	}
}

func TestChatTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	c := &Client{BaseURL: srv.URL, Model: "m", IsOllama: true, Timeout: 200 * time.Millisecond}
	start := time.Now()
	_, err := c.Chat("s", "u", true, 0.9)
	if err == nil {
		t.Fatal("no error on timeout")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v, timeout not applied", d)
	}
}

// --- ChatJSON ---------------------------------------------------------------------------------------------

func TestChatJSONRetriesThenSucceeds(t *testing.T) {
	cp := &capture{reply: func(n int) (int, string) {
		switch n {
		case 1:
			return 200, ollamaReply("I'd rather not.")
		case 2:
			return 200, ollamaReply("null") // json.loads -> None: not a result
		default:
			return 200, ollamaReply("```json\n{\"sections\": [{\"name\": \"A\", \"seconds\": \"60\"}]}\n```")
		}
	}}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", IsOllama: true}
	obj, err := c.ChatJSON("SYS", "USER")
	if err != nil {
		t.Fatal(err)
	}
	if cp.calls != 3 {
		t.Errorf("calls = %d, want 3", cp.calls)
	}
	secs, _ := obj["sections"].([]any)
	if len(secs) != 1 {
		t.Fatalf("obj = %v", obj)
	}
	if sec, _ := secs[0].(map[string]any); sec["name"] != "A" || sec["seconds"] != "60" {
		t.Errorf("section = %v", secs[0])
	}
	// Every attempt was json mode at temperature 0.9.
	if cp.body["format"] != "json" {
		t.Errorf("format = %#v", cp.body["format"])
	}
	if opts, _ := cp.body["options"].(map[string]any); opts["temperature"] != 0.9 {
		t.Errorf("temperature = %#v, want 0.9", opts["temperature"])
	}
	msgs := messagesOf(t, cp.body)
	if len(msgs) != 2 || msgs[0].Content != "SYS" || msgs[1].Content != "USER" {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestChatJSONFirstTry(t *testing.T) {
	cp := &capture{reply: func(int) (int, string) { return 200, openAIReply("{\"title\": \"T\"}") }}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Model: "m"}
	obj, err := c.ChatJSON("s", "u")
	if err != nil || obj["title"] != "T" {
		t.Fatalf("obj=%v err=%v", obj, err)
	}
	if cp.calls != 1 {
		t.Errorf("calls = %d, want 1", cp.calls)
	}
	if rf, _ := cp.body["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format = %#v", cp.body["response_format"])
	}
}

func TestChatJSONGivesUpAfterThree(t *testing.T) {
	// A long non-JSON reply of multi-byte characters: the error carries its first 400 characters
	// (code points, as Python's last[:400]), not 400 bytes.
	long := strings.Repeat("あ", 500)
	cp := &capture{reply: func(n int) (int, string) {
		if n < 3 {
			return 200, ollamaReply("nope " + strings.Repeat("x", 10))
		}
		return 200, ollamaReply(long)
	}}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", IsOllama: true}
	obj, err := c.ChatJSON("s", "u")
	if err == nil {
		t.Fatalf("no error, obj=%v", obj)
	}
	if obj != nil {
		t.Errorf("obj = %v with an error", obj)
	}
	if cp.calls != 3 {
		t.Errorf("calls = %d, want 3", cp.calls)
	}
	want := "LLM did not return JSON: " + strings.Repeat("あ", 400)
	if err.Error() != want {
		t.Errorf("error = %q (len %d)\nwant  %q", err.Error(), utf8.RuneCountInString(err.Error()), want)
	}
	if !strings.HasPrefix(err.Error(), "LLM did not return JSON: ") {
		t.Errorf("error prefix: %q", err.Error())
	}
}

// A reply that is a bare list or scalar is not an object: it is retried like prose.
func TestChatJSONNonObjectRetried(t *testing.T) {
	cp := &capture{reply: func(n int) (int, string) {
		if n == 1 {
			return 200, ollamaReply("[1, 2]")
		}
		return 200, ollamaReply("{\"a\": 1}")
	}}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()
	cl := &Client{BaseURL: srv.URL, Model: "m", IsOllama: true}
	obj, err := cl.ChatJSON("s", "u")
	if err != nil || obj["a"] != 1.0 {
		t.Fatalf("obj=%v err=%v", obj, err)
	}
	if cp.calls != 2 {
		t.Errorf("calls = %d, want 2", cp.calls)
	}
}

func TestChatJSONShortReplyNotTruncated(t *testing.T) {
	cp := &capture{reply: func(int) (int, string) { return 200, ollamaReply("short") }}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", IsOllama: true}
	_, err := c.ChatJSON("s", "u")
	if err == nil || err.Error() != "LLM did not return JSON: short" {
		t.Fatalf("error = %v", err)
	}
}

func TestChatJSONTransportErrorStopsRetrying(t *testing.T) {
	cp := &capture{reply: func(n int) (int, string) {
		if n == 1 {
			return 200, ollamaReply("garbage")
		}
		return 500, `{"error":"boom"}`
	}}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", IsOllama: true}
	_, err := c.ChatJSON("s", "u")
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "HTTP Error 500") {
		t.Errorf("error = %q, want the HTTP error, not the JSON one", err.Error())
	}
	if cp.calls != 2 {
		t.Errorf("calls = %d, want 2 (an HTTP error is not retried)", cp.calls)
	}
}

// --- Alive ------------------------------------------------------------------------------------------------

func TestAlive(t *testing.T) {
	cp := &capture{reply: func(int) (int, string) { return 200, `{"models": []}` }}
	srv := httptest.NewServer(cp.handler())
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "m", IsOllama: true}
	if !c.Alive() {
		t.Error("Ollama: not alive on 200")
	}
	if cp.method != "GET" || cp.path != "/api/tags" {
		t.Errorf("%s %s, want GET /api/tags", cp.method, cp.path)
	}
	c = &Client{BaseURL: srv.URL + "/v1", Model: "m", IsOllama: true}
	if !c.Alive() || cp.path != "/api/tags" {
		t.Errorf("Ollama with /v1 base: path = %q", cp.path)
	}

	c = &Client{BaseURL: srv.URL, Model: "m"}
	if !c.Alive() {
		t.Error("OpenAI: not alive on 200")
	}
	if cp.method != "GET" || cp.path != "/v1/models" {
		t.Errorf("%s %s, want GET /v1/models", cp.method, cp.path)
	}
	c = &Client{BaseURL: srv.URL + "/v1", Model: "m"}
	if !c.Alive() || cp.path != "/v1/models" {
		t.Errorf("OpenAI with /v1 base: path = %q", cp.path)
	}

	// Anything but 200 is dead.
	cp.reply = func(int) (int, string) { return 404, "" }
	if c.Alive() {
		t.Error("alive on 404")
	}
	cp.reply = func(int) (int, string) { return 500, "" }
	if c.Alive() {
		t.Error("alive on 500")
	}
	cp.reply = func(int) (int, string) { return 204, "" }
	if c.Alive() {
		t.Error("alive on 204")
	}
}

func TestAliveUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if (&Client{BaseURL: url, IsOllama: true}).Alive() {
		t.Error("alive against a closed server")
	}
	if (&Client{BaseURL: url}).Alive() {
		t.Error("alive against a closed server (OpenAI)")
	}
}

// --- helpers ---------------------------------------------------------------------------------------------

// unsetEnv removes key for the test and restores it afterwards (t.Setenv cannot express "absent").
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // registers the restore
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func intp(i int) *int { return &i }
