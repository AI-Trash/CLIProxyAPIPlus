package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCommandCodeExecutor_ExecuteStream_CodexResponseFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"Hello"}` + "\n"))
		w.Write([]byte(`{"type":"text-delta","text":" world"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":5,"totalTokens":15,"inputTokenDetails":{"cacheReadTokens":4}}}` + "\n"))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "test-auth",
		Provider:   "commandcode",
		Metadata:   map[string]any{"api_key": "test-key", "base_url": upstream.URL},
		Attributes: map[string]string{"base_url": upstream.URL},
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek/deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","input":"hi","stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: req.Payload,
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}

	var chunks []string
	for ch := range result.Chunks {
		if ch.Err != nil {
			t.Fatalf("stream error: %v", ch.Err)
		}
		if ch.Payload != nil {
			chunks = append(chunks, string(ch.Payload))
		}
	}

	joined := strings.Join(chunks, "\n")
	t.Logf("Chunks: %s", joined)

	if !strings.Contains(joined, `"type":"response.output_item.done"`) {
		t.Errorf("missing response.output_item.done\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Errorf("missing response.completed\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"input_tokens":10`) {
		t.Errorf("missing responses usage\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"output_tokens":5`) {
		t.Errorf("missing responses output usage\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"cached_tokens":4`) {
		t.Errorf("missing cached tokens\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_ExecuteStream_OpenAIFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"Hello"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":5,"inputTokenDetails":{"cacheReadTokens":6}}}` + "\n"))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "test-auth",
		Provider:   "commandcode",
		Metadata:   map[string]any{"api_key": "test-key", "base_url": upstream.URL},
		Attributes: map[string]string{"base_url": upstream.URL},
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek/deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: req.Payload,
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}

	var chunks []string
	for ch := range result.Chunks {
		if ch.Err != nil {
			t.Fatalf("stream error: %v", ch.Err)
		}
		if ch.Payload != nil {
			chunks = append(chunks, string(ch.Payload))
		}
	}

	joined := strings.Join(chunks, "\n")

	if !strings.Contains(joined, `"delta":{"content":"Hello"}`) {
		t.Errorf("missing content delta\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens":10`) {
		t.Errorf("missing usage\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens_details":{"cached_tokens":6}`) {
		t.Errorf("missing cached token usage\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_ExecuteStream_OpenAIFormatToolCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"Let me look at the project structure."}` + "\n"))
		w.Write([]byte(`{"type":"tool-call","toolCallId":"call_list","toolName":"list_files","input":{"path":"."}}` + "\n"))
		w.Write([]byte(`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":12,"outputTokens":7}}` + "\n"))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "test-auth",
		Provider:   "commandcode",
		Metadata:   map[string]any{"api_key": "test-key", "base_url": upstream.URL},
		Attributes: map[string]string{"base_url": upstream.URL},
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"what is this project"}],"stream":true,"tools":[{"type":"function","function":{"name":"list_files","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: req.Payload,
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}

	var chunks []string
	for ch := range result.Chunks {
		if ch.Err != nil {
			t.Fatalf("stream error: %v", ch.Err)
		}
		if ch.Payload != nil {
			chunks = append(chunks, string(ch.Payload))
		}
	}

	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, `"tool_calls":[{"index":0,"id":"call_list"`) {
		t.Fatalf("missing OpenAI tool call delta\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"name":"list_files"`) {
		t.Fatalf("missing tool name\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"arguments":"{\"path\":\".\"}"`) {
		t.Fatalf("missing tool arguments\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing tool_calls finish reason\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_ExecuteStream_ResponsesFormatToolCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"tool-call","toolCallId":"call_read","toolName":"read_file","input":{"filePath":"README.md"}}` + "\n"))
		w.Write([]byte(`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":11,"outputTokens":3}}` + "\n"))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "test-auth",
		Provider:   "commandcode",
		Metadata:   map[string]any{"api_key": "test-key", "base_url": upstream.URL},
		Attributes: map[string]string{"base_url": upstream.URL},
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","input":"inspect","stream":true,"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: req.Payload,
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}

	var chunks []string
	for ch := range result.Chunks {
		if ch.Err != nil {
			t.Fatalf("stream error: %v", ch.Err)
		}
		if ch.Payload != nil {
			chunks = append(chunks, string(ch.Payload))
		}
	}

	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, `"type":"response.output_item.added"`) ||
		!strings.Contains(joined, `"type":"function_call"`) ||
		!strings.Contains(joined, `"call_id":"call_read"`) {
		t.Fatalf("missing Responses function_call events\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"type":"response.function_call_arguments.done"`) {
		t.Fatalf("missing Responses function_call arguments done\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"type":"response.completed"`) {
		t.Fatalf("missing Responses completed event\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_ExecuteStream_ClaudeFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"Hello"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":5}}` + "\n"))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "test-auth",
		Provider:   "commandcode",
		Metadata:   map[string]any{"api_key": "test-key", "base_url": upstream.URL},
		Attributes: map[string]string{"base_url": upstream.URL},
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: req.Payload,
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}

	var chunks []string
	for ch := range result.Chunks {
		if ch.Err != nil {
			t.Fatalf("stream error: %v", ch.Err)
		}
		if ch.Payload != nil {
			chunks = append(chunks, string(ch.Payload))
		}
	}

	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "event: content_block_delta") {
		t.Errorf("missing Claude content delta\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, "event: message_stop") {
		t.Errorf("missing Claude message_stop\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_Execute_NonStreamResponsesFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"Hello world","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "test-auth",
		Provider:   "commandcode",
		Metadata:   map[string]any{"api_key": "test-key", "base_url": upstream.URL},
		Attributes: map[string]string{"base_url": upstream.URL},
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","input":"hi"}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("openai-response"),
		ResponseFormat:  sdktranslator.FromString("openai-response"),
		OriginalRequest: req.Payload,
	}

	result, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if got := gjson.GetBytes(result.Payload, "object").String(); got != "response" {
		t.Fatalf("object = %q, want response; payload=%s", got, string(result.Payload))
	}
	if got := gjson.GetBytes(result.Payload, "output.0.content.0.text").String(); got != "Hello world" {
		t.Fatalf("output text = %q, want Hello world; payload=%s", got, string(result.Payload))
	}
}

func TestCommandCodeExecutor_buildRequestBody(t *testing.T) {
	exec := NewCommandCodeExecutor(&config.Config{})

	tests := []struct {
		name        string
		payload     string
		srcFormat   string
		contains    []string
		notContains []string
	}{
		{
			name:      "basic openai request",
			payload:   `{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}`,
			srcFormat: "openai",
			contains:  []string{`"type":"text"`, `"text":"hello"`, `"role":"user"`},
		},
		{
			name:      "config fields present",
			payload:   `{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			srcFormat: "openai",
			contains:  []string{`"workingDir"`, `"date"`, `"isGitRepo"`},
		},
		{
			name:        "body shape matches official CLI",
			payload:     `{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			srcFormat:   "openai",
			contains:    []string{`"permissionMode":"standard"`, `Node.js`, `"tools":[]`, `"memory":null`, `"taste":null`, `"skills":null`},
			notContains: []string{`"mode":"tool-desc"`, `"environment":"production"`, `"temperature"`},
		},
		{
			name:      "responses input handled",
			payload:   `{"model":"test","input":"hello world","stream":true}`,
			srcFormat: "openai-response",
			contains:  []string{`"type":"text"`, `"text":"hello world"`, `"role":"user"`},
		},
		{
			name:      "instructions extracted as system",
			payload:   `{"model":"test","input":"hi","instructions":"be helpful","stream":true}`,
			srcFormat: "openai-response",
			contains:  []string{`"system":"be helpful"`},
		},
		{
			name:      "openai tools passed through",
			payload:   `{"model":"test","messages":[{"role":"user","content":"inspect"}],"stream":true,"tools":[{"type":"function","function":{"name":"list_files","description":"List files","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"tool_choice":"auto","parallel_tool_calls":true}`,
			srcFormat: "openai",
			contains: []string{
				`"tools":[{"type":"function","name":"list_files","description":"List files","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]`,
				`"parallel_tool_calls":true`,
			},
		},
		{
			name:      "object tool choice passed through",
			payload:   `{"model":"test","messages":[{"role":"user","content":"inspect"}],"stream":true,"tools":[{"type":"function","function":{"name":"list_files","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"list_files"}}}`,
			srcFormat: "openai",
			contains:  []string{`"tool_choice":{"type":"function","function":{"name":"list_files"}}`},
		},
		{
			name:      "tool history converted",
			payload:   `{"model":"test","messages":[{"role":"assistant","content":"checking","tool_calls":[{"id":"call_read","type":"function","function":{"name":"read_file","arguments":"{\"filePath\":\"README.md\"}"}}]},{"role":"tool","tool_call_id":"call_read","content":"done"},{"role":"user","content":"continue"}],"stream":true}`,
			srcFormat: "openai",
			contains: []string{
				`"type":"tool-call"`,
				`"toolCallId":"call_read"`,
				`"toolName":"read_file"`,
				`"input":{"filePath":"README.md"}`,
				`"type":"tool-result"`,
				`"output":{"type":"text","value":"done"}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{
				Model:   "deepseek/deepseek-v4-pro",
				Payload: []byte(tt.payload),
			}
			opts := cliproxyexecutor.Options{
				Stream:       true,
				SourceFormat: sdktranslator.FromString(tt.srcFormat),
			}
			body := exec.buildRequestBody(req, opts, true, nil)
			bodyStr := string(body)
			for _, c := range tt.contains {
				if !strings.Contains(bodyStr, c) {
					t.Errorf("%s: missing %q\nGot: %s", tt.name, c, bodyStr)
				}
			}
			for _, c := range tt.notContains {
				if strings.Contains(bodyStr, c) {
					t.Errorf("%s: unexpected %q\nGot: %s", tt.name, c, bodyStr)
				}
			}
		})
	}
}

func TestCommandCodeExecutor_injectHeaders_CLIpfingerprint(t *testing.T) {
	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "test-auth",
		Provider:   "commandcode",
		Attributes: map[string]string{"api_key": "test-key"},
	}

	httpReq, err := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	exec.injectHeaders(httpReq, auth)

	// Verify x-* headers are stored in lowercase (not Go's Title-Case canonicalization).
	// Note: Header.Get() canonicalizes the key, so we use bracket notation.
	getLower := func(key string) string {
		vals, ok := httpReq.Header[key]
		if !ok || len(vals) == 0 {
			return ""
		}
		return vals[0]
	}
	for _, lowerKey := range []string{"x-cli-environment", "x-command-code-version", "x-session-id", "x-project-slug", "x-taste-learning", "x-co-flag", "traceparent", "accept", "accept-language"} {
		if v := getLower(lowerKey); v == "" {
			t.Errorf("missing required CLI header %q (lowercase)", lowerKey)
		}
	}

	if got := getLower("x-command-code-version"); got != "0.40.3" {
		t.Errorf("x-command-code-version = %q, want 0.40.3", got)
	}
	if got := getLower("x-cli-environment"); got != "production" {
		t.Errorf("x-cli-environment = %q, want production", got)
	}
	if got := getLower("x-taste-learning"); got != "true" {
		t.Errorf("x-taste-learning = %q, want true", got)
	}
	if got := getLower("x-co-flag"); got != "false" {
		t.Errorf("x-co-flag = %q, want false", got)
	}
	// Authorization uses Title-Case (matching official CLI), so Get() works.
	if got := httpReq.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got)
	}
	// User-Agent must be suppressed (nil slice) so Go's transport omits it.
	if got := httpReq.Header.Get("User-Agent"); got != "" {
		t.Errorf("User-Agent = %q, want empty", got)
	}

	// Verify no Title-Case duplicates exist for the x-* headers.
	for _, titleKey := range []string{"X-Cli-Environment", "X-Command-Code-Version", "X-Session-Id", "X-Project-Slug", "X-Taste-Learning", "X-Co-Flag", "Traceparent"} {
		if _, ok := httpReq.Header[titleKey]; ok {
			t.Errorf("Title-Case header key %q should not exist (use lowercase)", titleKey)
		}
	}
}
