// Package llm is a tiny chat client for local LLMs (port of aitv/llm.py).
//
//   - Ollama (default, http://127.0.0.1:11434): the native /api/chat so JSON output can be forced and the model
//     pinned to the CPU (num_gpu 0, the default) — the iGPU is busy with H3.
//   - Anything OpenAI-compatible (LM Studio http://127.0.0.1:1234/v1, vLLM, ...): /chat/completions.
package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultTimeout is the per-request timeout New installs (a 6-section script on a CPU-pinned model is slow).
const DefaultTimeout = 30 * time.Minute

// Client is one writer LLM.
type Client struct {
	// BaseURL without trailing slash: an Ollama root or an OpenAI-compatible /v1 base.
	BaseURL string
	Model   string
	// Timeout per request (30 min by default: a 6-section script on a CPU-pinned model is slow).
	// Zero or negative means DefaultTimeout.
	Timeout time.Duration
	// IsOllama is true when BaseURL contains ":11434" (same heuristic as the Python client).
	IsOllama bool
	// NumGPU is Ollama's num_gpu option (0 = CPU only).
	NumGPU int
}

// New builds a client. Empty baseURL/model fall back to AITV_LLM_URL / AITV_LLM_MODEL, then
// http://127.0.0.1:11434 / "gemma4:latest". numGPU nil (Python's None) falls back to AITV_LLM_NUM_GPU,
// then 0; any value given, negative ones included (Ollama's -1 = every layer), is used as is.
func New(baseURL, model string, numGPU *int) *Client {
	if baseURL == "" {
		baseURL = os.Getenv("AITV_LLM_URL")
	}
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if model == "" {
		model = os.Getenv("AITV_LLM_MODEL")
	}
	if model == "" {
		model = "gemma4:latest"
	}
	gpu := 0
	if numGPU != nil {
		gpu = *numGPU
	} else if s := strings.TrimSpace(os.Getenv("AITV_LLM_NUM_GPU")); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			gpu = n
		}
	}
	return &Client{
		BaseURL:  baseURL,
		Model:    model,
		Timeout:  DefaultTimeout,
		IsOllama: strings.Contains(baseURL, ":11434"),
		NumGPU:   gpu,
	}
}

// message is one chat turn.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ollamaOptions is the "options" object of an Ollama /api/chat request (field order as the Python dict).
type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
	NumCtx      int     `json:"num_ctx"`
	NumGPU      int     `json:"num_gpu"`
}

// ollamaRequest is an Ollama /api/chat request (field order as the Python dict).
type ollamaRequest struct {
	Model    string        `json:"model"`
	Messages []message     `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  ollamaOptions `json:"options"`
	Format   string        `json:"format,omitempty"`
}

// responseFormat is OpenAI's "response_format" object.
type responseFormat struct {
	Type string `json:"type"`
}

// openAIRequest is an OpenAI-compatible /chat/completions request (field order as the Python dict).
type openAIRequest struct {
	Model          string          `json:"model"`
	Messages       []message       `json:"messages"`
	Temperature    float64         `json:"temperature"`
	Stream         bool            `json:"stream"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

// root is the Ollama root: BaseURL with a trailing /v1 stripped.
func (c *Client) root() string { return strings.TrimSuffix(c.BaseURL, "/v1") }

// openAIBase is the OpenAI-compatible base: BaseURL with /v1 appended when missing.
func (c *Client) openAIBase() string {
	if strings.HasSuffix(c.BaseURL, "/v1") {
		return c.BaseURL
	}
	return c.BaseURL + "/v1"
}

// apiKey is the bearer token: AITV_LLM_KEY when set (even empty, as os.environ.get did), else "local".
func apiKey() string {
	if k, ok := os.LookupEnv("AITV_LLM_KEY"); ok {
		return k
	}
	return "local"
}

// post sends payload as JSON to url and decodes the JSON reply. A non-2xx status or a body that is not a
// JSON object is an error.
func (c *Client) post(url string, payload any) (map[string]any, error) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, &body)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey())
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm: %s: read reply: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Same text as Python's urllib HTTPError, plus the body: Ollama says {"error":"model ... not found"}.
		msg := fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, reason(resp))
		if snippet := strings.TrimSpace(string(raw)); snippet != "" {
			msg += " — " + truncate(snippet, 400)
		}
		return nil, errors.New(msg)
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("llm: %s: malformed reply: %w", url, err)
	}
	if reply == nil {
		return nil, fmt.Errorf("llm: %s: malformed reply: not a JSON object", url)
	}
	return reply, nil
}

// reason is the status line's reason phrase ("Internal Server Error"), falling back to the standard one.
func reason(resp *http.Response) string {
	r := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
	if r == "" {
		r = http.StatusText(resp.StatusCode)
	}
	return r
}

// Chat sends system + user and returns the assistant text. Ollama: POST <root>/api/chat with
// {"model","messages","stream":false,"options":{"temperature","num_ctx":16384,"num_gpu"}} (+ "format":"json"
// when jsonMode); the root is BaseURL with a trailing /v1 stripped. OpenAI: POST <base>/chat/completions with
// {"model","messages","temperature","stream":false} (+ "response_format":{"type":"json_object"} when jsonMode);
// the base is BaseURL with /v1 appended when missing. Every request carries
// "Authorization: Bearer <AITV_LLM_KEY or 'local'>". A non-2xx status or a malformed body is an error.
func (c *Client) Chat(system, user string, jsonMode bool, temperature float64) (string, error) {
	messages := []message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	if c.IsOllama {
		payload := ollamaRequest{
			Model:    c.Model,
			Messages: messages,
			Stream:   false,
			Options:  ollamaOptions{Temperature: temperature, NumCtx: 16384, NumGPU: c.NumGPU},
		}
		if jsonMode {
			payload.Format = "json"
		}
		url := c.root() + "/api/chat"
		reply, err := c.post(url, payload)
		if err != nil {
			return "", err
		}
		// reply["message"]["content"]
		msg, ok := reply["message"].(map[string]any)
		if !ok {
			return "", fmt.Errorf("llm: %s: malformed reply: no \"message\" object", url)
		}
		content, ok := msg["content"].(string)
		if !ok {
			return "", fmt.Errorf("llm: %s: malformed reply: no \"message\".\"content\" string", url)
		}
		return content, nil
	}
	payload := openAIRequest{Model: c.Model, Messages: messages, Temperature: temperature, Stream: false}
	if jsonMode {
		payload.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	url := c.openAIBase() + "/chat/completions"
	reply, err := c.post(url, payload)
	if err != nil {
		return "", err
	}
	// reply["choices"][0]["message"]["content"]
	choices, ok := reply["choices"].([]any)
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("llm: %s: malformed reply: no \"choices\"", url)
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return "", fmt.Errorf("llm: %s: malformed reply: \"choices\"[0] is not an object", url)
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("llm: %s: malformed reply: no \"choices\"[0].\"message\" object", url)
	}
	content, ok := msg["content"].(string)
	if !ok {
		return "", fmt.Errorf("llm: %s: malformed reply: no \"choices\"[0].\"message\".\"content\" string", url)
	}
	return content, nil
}

// ChatJSON calls Chat(json mode, temperature 0.9) up to 3 times until ExtractJSON finds an object; the
// error text is "LLM did not return JSON: " + the last reply (truncated to 400 characters). A transport /
// HTTP error from Chat is returned at once, without further retries.
func (c *Client) ChatJSON(system, user string) (map[string]any, error) {
	last := ""
	for range 3 {
		reply, err := c.Chat(system, user, true, 0.9)
		if err != nil {
			return nil, err
		}
		last = reply
		if obj, ok := ExtractJSON(last); ok {
			return obj, nil
		}
	}
	return nil, errors.New("LLM did not return JSON: " + truncate(last, 400))
}

// Alive is a 5 s GET of <root>/api/tags (Ollama) or <base>/models (OpenAI-compatible), true on HTTP 200.
func (c *Client) Alive() bool {
	url := c.openAIBase() + "/models"
	if c.IsOllama {
		url = c.root() + "/api/tags"
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// ExtractJSON finds the JSON object in a model reply: strips a ```json fence, parses the whole text, else
// the first "{" to the last "}". ok is false when nothing parses as an object (a bare list or scalar, a
// null, prose). Values are what encoding/json gives for an untyped decode: float64, string, bool, []any,
// map[string]any, nil.
func ExtractJSON(text string) (obj map[string]any, ok bool) {
	text = stripFence(strings.TrimSpace(text))
	if obj, ok := parseObject(text); ok {
		return obj, true
	}
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j < i {
		return nil, false
	}
	return parseObject(text[i : j+1])
}

func parseObject(s string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// stripFence removes a leading ``` or ```json (and the whitespace after it) and a trailing ``` (and the
// whitespace before it).
func stripFence(text string) string {
	if rest, found := strings.CutPrefix(text, "```"); found {
		rest = strings.TrimPrefix(rest, "json")
		text = strings.TrimLeftFunc(rest, unicode.IsSpace)
	}
	if rest, found := strings.CutSuffix(text, "```"); found {
		text = strings.TrimRightFunc(rest, unicode.IsSpace)
	}
	return text
}

// truncate keeps the first n characters (code points) of s, like Python's s[:n].
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
