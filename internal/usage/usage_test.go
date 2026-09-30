package usage

import "testing"

func TestAnthropicJSON(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5-5","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":40}}`)
	u, ok := Anthropic{}.ParseJSON(body)
	if !ok || u.InputTokens != 10 || u.OutputTokens != 20 || u.CacheReadTokens != 300 || u.CacheWriteTokens != 40 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	if u.TotalInput() != 350 {
		t.Fatalf("total input = %d", u.TotalInput())
	}
}

func TestAnthropicSSE(t *testing.T) {
	body := []byte("event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-sonnet-5-5","usage":{"input_tokens":5,"output_tokens":1,"cache_read_input_tokens":100}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n")
	u, ok := Anthropic{}.ParseSSE(body)
	if !ok || u.Model != "claude-sonnet-5-5" || u.InputTokens != 5 || u.CacheReadTokens != 100 || u.OutputTokens != 42 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

func TestOpenAIJSONSubtractsCached(t *testing.T) {
	body := []byte(`{"model":"gpt-x","usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800},"completion_tokens_details":{"reasoning_tokens":30}}}`)
	u, ok := Registry["openai"].ParseJSON(body)
	if !ok || u.InputTokens != 200 || u.CacheReadTokens != 800 || u.OutputTokens != 50 || u.ReasoningTokens != 30 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

func TestOpenAISSEFinalChunk(t *testing.T) {
	body := []byte(`data: {"model":"grok","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"model":"grok","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n")
	u, ok := Registry["xai"].ParseSSE(body)
	if !ok || u.InputTokens != 7 || u.OutputTokens != 3 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

func TestOpenAIResponsesAPI(t *testing.T) {
	body := []byte(`{"type":"response.completed","response":{"model":"gpt-x","usage":{"input_tokens":100,"output_tokens":9,"input_tokens_details":{"cached_tokens":64}}}}`)
	u, ok := Registry["openai"].ParseJSON(body)
	if !ok || u.InputTokens != 36 || u.CacheReadTokens != 64 || u.OutputTokens != 9 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
}

func TestNoUsage(t *testing.T) {
	if _, ok := (Anthropic{}).ParseJSON([]byte(`{"error":"x"}`)); ok {
		t.Fatal("expected no usage")
	}
}

func TestAnthropicCacheWrite1hBreakdown(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5-5","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":30,"ephemeral_1h_input_tokens":70}}}`)
	u, ok := Anthropic{}.ParseJSON(body)
	if !ok || u.CacheWriteTokens != 100 || u.CacheWrite1hTokens != 70 {
		t.Fatalf("got %+v ok=%v", u, ok)
	}
	sse := []byte("data: " + `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":1,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_1h_input_tokens":10}}}}` + "\n\n")
	if u, _ := (Anthropic{}).ParseSSE(sse); u.CacheWrite1hTokens != 10 {
		t.Fatalf("sse: %+v", u)
	}
	// Responses without the breakdown leave it at zero (all 5-minute).
	if u, _ := (Anthropic{}).ParseJSON([]byte(`{"usage":{"input_tokens":1,"cache_creation_input_tokens":5}}`)); u.CacheWrite1hTokens != 0 {
		t.Fatalf("no breakdown: %+v", u)
	}
}

func TestAnthropicIDAndWebSearches(t *testing.T) {
	body := []byte(`{"id":"msg_abc","model":"m","usage":{"input_tokens":1,"output_tokens":2,"server_tool_use":{"web_search_requests":3}}}`)
	u, ok := Anthropic{}.ParseJSON(body)
	if !ok || u.ID != "msg_abc" || u.WebSearchRequests != 3 {
		t.Fatalf("json: %+v", u)
	}
	sse := []byte("data: " + `{"type":"message_start","message":{"id":"msg_s","model":"m","usage":{"input_tokens":1}}}` + "\n\n" +
		"data: " + `{"type":"message_delta","usage":{"output_tokens":9,"server_tool_use":{"web_search_requests":2}}}` + "\n\n")
	u, ok = Anthropic{}.ParseSSE(sse)
	if !ok || u.ID != "msg_s" || u.WebSearchRequests != 2 || u.OutputTokens != 9 {
		t.Fatalf("sse: %+v", u)
	}
}
