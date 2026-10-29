// Package ai talks to a language model, and builds the prompts wako uses it for.
//
// wako holds no model itself. An administrator points it at a service (the
// Anthropic API, or any OpenAI-compatible endpoint, which includes local
// servers such as Ollama), and the features that use it ("ask the wiki", page
// drafts) stay off until a key is configured and a space opts in.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Message is one turn of a conversation.
type Message struct {
	Role    string // "user" or "assistant"
	Content string
}

// Request is one call to a model.
type Request struct {
	System    string
	Messages  []Message
	MaxTokens int
}

// Result is what came back.
type Result struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	Truncated    bool // the model hit its output limit
}

// Provider is a language model service.
type Provider interface {
	Complete(ctx context.Context, req Request) (*Result, error)
	// Model is the model name, recorded next to anything it wrote.
	Model() string
}

// Config selects and configures a provider.
type Config struct {
	Kind      string // "anthropic" or "openai"
	Key       string
	Model     string
	URL       string // base URL; the default is the provider's own service
	MaxTokens int
	Timeout   time.Duration
	Client    *http.Client // for tests
}

// Default model and endpoints.
const (
	DefaultAnthropicModel = "claude-sonnet-5-5"
	DefaultOpenAIModel    = "gpt-4.1"
	anthropicURL          = "https://api.anthropic.com"
	openaiURL             = "https://api.openai.com/v1"
	maxResponse           = 4 << 20
)

// ErrNotConfigured means no model is set up.
var ErrNotConfigured = errors.New("no AI model is configured on this server")

// Error is a failure of the model service, worded to be shown to a user. It
// never contains the API key.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// New returns the configured provider, or nil if Kind is empty or "off".
func New(c Config) (Provider, error) {
	kind := strings.ToLower(strings.TrimSpace(c.Kind))
	if kind == "" || kind == "off" || kind == "none" {
		return nil, nil
	}
	if kind != "anthropic" && kind != "openai" {
		return nil, fmt.Errorf("ai-provider must be anthropic, openai or off, not %q", c.Kind)
	}
	if c.Key == "" && !localURL(c.URL) {
		return nil, fmt.Errorf("ai-provider %s needs an API key (WAKO_AI_KEY)", kind)
	}
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("ai-url must be an http(s) address, not %q", c.URL)
		}
	}
	if c.Timeout <= 0 {
		c.Timeout = 90 * time.Second
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 2048
	}
	if c.Client == nil {
		c.Client = &http.Client{Timeout: c.Timeout}
	}
	if kind == "anthropic" {
		if c.Model == "" {
			c.Model = DefaultAnthropicModel
		}
		if c.URL == "" {
			c.URL = anthropicURL
		}
		return &anthropic{c}, nil
	}
	if c.Model == "" {
		c.Model = DefaultOpenAIModel
	}
	if c.URL == "" {
		c.URL = openaiURL
	}
	return &openai{c}, nil
}

// localURL reports whether u is a server on this machine, which may need no key.
func localURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	h := p.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func post(ctx context.Context, c Config, endpoint string, headers map[string]string, body any, decode func([]byte) error) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL may be long; the cause is what matters
		}
		return &Error{0, "the AI service could not be reached: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return &Error{resp.StatusCode, "the AI service's answer could not be read"}
	}
	if resp.StatusCode != http.StatusOK {
		msg := serviceError(resp.StatusCode, raw)
		if c.Key != "" {
			msg = strings.ReplaceAll(msg, c.Key, "[key]") // a service should never echo it, but make sure
		}
		return &Error{resp.StatusCode, msg}
	}
	if err := decode(raw); err != nil {
		return &Error{resp.StatusCode, "the AI service sent an answer that could not be understood"}
	}
	return nil
}

// serviceError words a failure for a user: what it means and what to do, with
// the service's own message (which never holds our key) when it gave one.
func serviceError(status int, raw []byte) string {
	var j struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &j)
	detail := strings.TrimSpace(j.Error.Message)
	if len(detail) > 200 {
		detail = detail[:200] + "…"
	}
	var what string
	switch {
	case status == 401 || status == 403:
		what = "the AI service refused the server's API key (an administrator needs to check it)"
	case status == 404:
		what = "the AI service does not know the configured model (an administrator needs to check it)"
	case status == 429:
		what = "the AI service is busy or its usage limit is reached; try again later"
	case status >= 500:
		what = "the AI service had a problem; try again later"
	default:
		what = fmt.Sprintf("the AI service refused the request (%d)", status)
	}
	if detail != "" && status != 401 && status != 403 {
		what += ": " + detail
	}
	return what
}

// ---- Anthropic Messages API ----

type anthropic struct{ c Config }

func (a *anthropic) Model() string { return a.c.Model }

func (a *anthropic) Complete(ctx context.Context, r Request) (*Result, error) {
	max := r.MaxTokens
	if max <= 0 {
		max = a.c.MaxTokens
	}
	msgs := make([]map[string]string, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": a.c.Model, "max_tokens": max, "messages": msgs}
	if r.System != "" {
		body["system"] = r.System
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Model      string `json:"model"`
		Usage      struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	hdr := map[string]string{"anthropic-version": "2023-06-01"}
	if a.c.Key != "" {
		hdr["x-api-key"] = a.c.Key
	}
	if err := post(ctx, a.c, strings.TrimRight(a.c.URL, "/")+"/v1/messages", hdr, body, func(raw []byte) error { return json.Unmarshal(raw, &out) }); err != nil {
		return nil, err
	}
	var text strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return nil, &Error{200, "the AI service answered with no text"}
	}
	model := out.Model
	if model == "" {
		model = a.c.Model
	}
	return &Result{Text: text.String(), Model: model, InputTokens: out.Usage.In, OutputTokens: out.Usage.Out, Truncated: out.StopReason == "max_tokens"}, nil
}

// ---- OpenAI-compatible chat completions ----

type openai struct{ c Config }

func (o *openai) Model() string { return o.c.Model }

func (o *openai) Complete(ctx context.Context, r Request) (*Result, error) {
	max := r.MaxTokens
	if max <= 0 {
		max = o.c.MaxTokens
	}
	var msgs []map[string]string
	if r.System != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": r.System})
	}
	for _, m := range r.Messages {
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": o.c.Model, "max_tokens": max, "messages": msgs, "stream": false}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Model string `json:"model"`
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	hdr := map[string]string{}
	if o.c.Key != "" {
		hdr["Authorization"] = "Bearer " + o.c.Key
	}
	if err := post(ctx, o.c, strings.TrimRight(o.c.URL, "/")+"/chat/completions", hdr, body, func(raw []byte) error {
		if looksLikeStream(raw) {
			return decodeStream(raw, &out)
		}
		return json.Unmarshal(raw, &out)
	}); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return nil, &Error{200, "the AI service answered with no text"}
	}
	model := out.Model
	if model == "" {
		model = o.c.Model
	}
	return &Result{Text: out.Choices[0].Message.Content, Model: model, InputTokens: out.Usage.In, OutputTokens: out.Usage.Out,
		Truncated: out.Choices[0].FinishReason == "length"}, nil
}

// Some OpenAI-compatible servers (local ones, especially) stream their answer
// as server-sent events even when not asked to. Accept that too: the pieces of
// text are joined, and the finish reason and usage are taken from the last
// chunks that carry them.
func looksLikeStream(raw []byte) bool {
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte("data:"))
}

func decodeStream(raw []byte, out any) error {
	var text strings.Builder
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Model string `json:"model"`
		Usage *struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	var model, finish string
	var in, outTok int
	seen := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		chunk.Choices, chunk.Usage, chunk.Model = nil, nil, ""
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return err
		}
		seen = true
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			in, outTok = chunk.Usage.In, chunk.Usage.Out
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	if !seen {
		return errors.New("no data in stream")
	}
	// Re-encode in the shape of a complete reply, and decode that.
	whole := map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": text.String()}, "finish_reason": finish}},
		"model":   model,
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": outTok},
	}
	b, _ := json.Marshal(whole)
	return json.Unmarshal(b, out)
}
