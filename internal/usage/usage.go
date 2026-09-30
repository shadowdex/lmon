// Package usage normalizes token accounting across LLM providers.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// Usage is the provider-neutral token accounting for one response.
type Usage struct {
	Model            string `json:"model,omitempty"`
	InputTokens      int    `json:"input_tokens"` // uncached input
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`  // input served from cache
	CacheWriteTokens int    `json:"cache_write_tokens"` // input written to cache (Anthropic)
	// CacheWrite1hTokens is the part of CacheWriteTokens written with the
	// 1-hour TTL, which is priced higher than the default 5-minute TTL.
	CacheWrite1hTokens int `json:"cache_write_1h_tokens,omitempty"`
	ReasoningTokens    int `json:"reasoning_tokens,omitempty"`
}

// TotalInput is all input tokens, cached or not.
func (u Usage) TotalInput() int { return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens }

// Adapter extracts Usage from a provider's response body.
type Adapter interface {
	Name() string
	// Upstream is the default API base URL for this provider.
	Upstream() string
	// ParseJSON parses a non-streaming response body.
	ParseJSON(body []byte) (Usage, bool)
	// ParseSSE parses a streaming (server-sent events) response body.
	ParseSSE(body []byte) (Usage, bool)
}

// Registry maps a URL path prefix segment (e.g. "anthropic") to its adapter.
var Registry = map[string]Adapter{
	"anthropic":  Anthropic{},
	"openai":     OpenAICompat{name: "openai", upstream: "https://api.openai.com"},
	"xai":        OpenAICompat{name: "xai", upstream: "https://api.x.ai"},
	"groq":       OpenAICompat{name: "groq", upstream: "https://api.groq.com/openai"},
	"mistral":    OpenAICompat{name: "mistral", upstream: "https://api.mistral.ai"},
	"deepseek":   OpenAICompat{name: "deepseek", upstream: "https://api.deepseek.com"},
	"openrouter": OpenAICompat{name: "openrouter", upstream: "https://openrouter.ai/api"},
}

// sseData yields the JSON payload of every `data:` line in an SSE body.
func sseData(body []byte, fn func(payload []byte)) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if p == "" || p == "[DONE]" {
			continue
		}
		fn([]byte(p))
	}
}

// ---- Anthropic ----

type Anthropic struct{}

func (Anthropic) Name() string     { return "anthropic" }
func (Anthropic) Upstream() string { return "https://api.anthropic.com" }

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheCreation            struct {
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

func (a anthropicUsage) into(u *Usage) {
	u.InputTokens = a.InputTokens
	u.CacheReadTokens = a.CacheReadInputTokens
	u.CacheWriteTokens = a.CacheCreationInputTokens
	u.CacheWrite1hTokens = a.CacheCreation.Ephemeral1h
	if a.OutputTokens > 0 {
		u.OutputTokens = a.OutputTokens
	}
}

func (Anthropic) ParseJSON(body []byte) (Usage, bool) {
	var r struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil || r.Usage == nil {
		return Usage{}, false
	}
	u := Usage{Model: r.Model}
	r.Usage.into(&u)
	return u, true
}

// ParseSSE: message_start carries model + input/cache usage; message_delta
// carries the final cumulative output_tokens.
func (Anthropic) ParseSSE(body []byte) (Usage, bool) {
	var u Usage
	found := false
	sseData(body, func(p []byte) {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string          `json:"model"`
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
			Usage *anthropicUsage `json:"usage"`
		}
		if json.Unmarshal(p, &ev) != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			u.Model = ev.Message.Model
			if ev.Message.Usage != nil {
				ev.Message.Usage.into(&u)
				found = true
			}
		case "message_delta":
			if ev.Usage != nil {
				if ev.Usage.OutputTokens > 0 {
					u.OutputTokens = ev.Usage.OutputTokens
				}
				found = true
			}
		}
	})
	return u, found
}

// ---- OpenAI and OpenAI-compatible (xAI, Groq, Mistral, DeepSeek, OpenRouter...) ----

type OpenAICompat struct{ name, upstream string }

func (o OpenAICompat) Name() string     { return o.name }
func (o OpenAICompat) Upstream() string { return o.upstream }

type openaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	InputTokens         int `json:"input_tokens"`  // Responses API
	OutputTokens        int `json:"output_tokens"` // Responses API
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// into normalizes: OpenAI's prompt_tokens INCLUDES cached tokens, so subtract
// them to keep InputTokens meaning "uncached input" across providers.
func (o openaiUsage) into(u *Usage) {
	in, out := o.PromptTokens, o.CompletionTokens
	cached := o.PromptTokensDetails.CachedTokens
	reasoning := o.CompletionTokensDetails.ReasoningTokens
	if in == 0 && out == 0 {
		in, out = o.InputTokens, o.OutputTokens
		cached = o.InputTokensDetails.CachedTokens
		reasoning = o.OutputTokensDetails.ReasoningTokens
	}
	if cached > in {
		cached = in
	}
	u.InputTokens = in - cached
	u.CacheReadTokens = cached
	u.OutputTokens = out
	u.ReasoningTokens = reasoning
}

func (OpenAICompat) ParseJSON(body []byte) (Usage, bool) {
	var r struct {
		Model    string       `json:"model"`
		Usage    *openaiUsage `json:"usage"`
		Response *struct {
			Model string       `json:"model"`
			Usage *openaiUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &r) != nil {
		return Usage{}, false
	}
	u := Usage{Model: r.Model}
	switch {
	case r.Usage != nil:
		r.Usage.into(&u)
	case r.Response != nil && r.Response.Usage != nil:
		u.Model = r.Response.Model
		r.Response.Usage.into(&u)
	default:
		return Usage{}, false
	}
	return u, true
}

// ParseSSE: usage arrives in the last chunk, and only when the request set
// stream_options.include_usage (chat completions) or on response.completed.
func (o OpenAICompat) ParseSSE(body []byte) (Usage, bool) {
	var last Usage
	found := false
	sseData(body, func(p []byte) {
		if u, ok := o.ParseJSON(p); ok {
			last, found = u, true
		}
	})
	return last, found
}
