package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
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

func TestCommandCodeExecutor_ExecuteStream_OpenAIFormatReasoning(t *testing.T) {
	// command-code@1.44.0 stream events: reasoning-start/delta/end + text-delta + finish
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"reasoning-start"}` + "\n"))
		w.Write([]byte(`{"type":"reasoning-delta","text":"thinking..."}` + "\n"))
		w.Write([]byte(`{"type":"reasoning-end"}` + "\n"))
		w.Write([]byte(`{"type":"text-delta","text":"done"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":8,"outputTokens":3,"inputTokenDetails":{"cacheReadTokens":5,"cacheWriteTokens":2}},"finishReason":"end_turn"}` + "\n"))
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
	if !strings.Contains(joined, `"reasoning_content":"thinking..."`) {
		t.Errorf("missing reasoning_content delta\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"delta":{"content":"done"}`) {
		t.Errorf("missing content delta\nGot:\n%s", joined)
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
		// /alpha/generate is streaming-only; Execute() always opens a streaming
		// request and aggregates events internally.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"Hello world"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":5,"totalTokens":15,"inputTokenDetails":{"cacheReadTokens":3,"cacheWriteTokens":2}},"rawFinishReason":"end_turn"}` + "\n"))
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

func TestCommandCodeExecutor_ExecuteStream_RawFinishReasonAndCacheWrite(t *testing.T) {
	// command-code@1.44.0: finish event may carry rawFinishReason instead of
	// finishReason, and inputTokenDetails.cacheWriteTokens.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"hi"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":9,"outputTokens":2,"inputTokenDetails":{"cacheReadTokens":4,"cacheWriteTokens":1}},"rawFinishReason":"end_turn"}` + "\n"))
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
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason from rawFinishReason fallback\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens_details":{"cached_tokens":4}`) {
		t.Errorf("missing cached token usage\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_buildRequestBody(t *testing.T) {
	exec := NewCommandCodeExecutor(&config.Config{})

	tests := []struct {
		name        string
		model       string
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
			contains:    []string{`"permissionMode":"standard"`, `Node.js`, `"tools":[]`, `"memory":null`, `"taste":null`, `"skills":null`, `"threadId":`},
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
			// command-code@1.44.0 toWireTools: {name, description, input_schema} only (no type:function)
			contains: []string{
				`"name":"list_files"`,
				`"description":"List files"`,
				`"input_schema":{"type":"object","properties":{"path":{"type":"string"}}}`,
				`"parallel_tool_calls":true`,
			},
			notContains: []string{`"type":"function","name":"list_files"`},
		},
		{
			name:        "billing model prefix stripped to canonical",
			model:       "anthropic:claude-sonnet-5",
			payload:     `{"model":"anthropic:claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			srcFormat:   "openai",
			contains:    []string{`"model":"claude-sonnet-5"`},
			notContains: []string{`"model":"anthropic:claude-sonnet-5"`},
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
		{
			name:      "multimodal image_url converted to command-code wire image format",
			payload:   `{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}],"stream":true}`,
			srcFormat: "openai",
			contains: []string{
				`"type":"text"`,
				`"text":"describe this"`,
				`"type":"image"`,
				`"image":"data:image/png;base64,iVBORw0KGgo="`,
				`"mimeType":"image/png"`,
			},
		},
		{
			name:      "anthropic multimodal image converted to command-code wire image format",
			payload:   `{"model":"test","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"UklGRg=="}}]}],"stream":true}`,
			srcFormat: "openai",
			contains: []string{
				`"type":"text"`,
				`"text":"describe this"`,
				`"type":"image"`,
				`"image":"data:image/webp;base64,UklGRg=="`,
				`"mimeType":"image/webp"`,
			},
		},
		{
			name:        "reasoning_effort none omitted",
			payload:     `{"model":"test","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none","stream":true}`,
			srcFormat:   "openai",
			notContains: []string{`"reasoning_effort"`},
		},
		{
			name:        "responses reasoning effort none omitted",
			payload:     `{"model":"test","input":"hi","reasoning":{"effort":"none"},"instructions":"title","stream":true}`,
			srcFormat:   "openai-response",
			notContains: []string{`"reasoning_effort"`},
		},
		{
			name:      "reasoning_effort minimal mapped to low",
			payload:   `{"model":"test","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"minimal","stream":true}`,
			srcFormat: "openai",
			contains:  []string{`"reasoning_effort":"low"`},
		},
		{
			name:      "reasoning_effort auto mapped to high",
			payload:   `{"model":"test","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"auto","stream":true}`,
			srcFormat: "openai",
			contains:  []string{`"reasoning_effort":"high"`},
		},
		{
			name:      "reasoning_effort high passed through",
			payload:   `{"model":"test","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","stream":true}`,
			srcFormat: "openai",
			contains:  []string{`"reasoning_effort":"high"`},
		},
		{
			name:        "model suffix low sets reasoning_effort and strips suffix from model",
			model:       "meta/muse-spark-1.2-contributor(low)",
			payload:     `{"model":"meta/muse-spark-1.2-contributor(low)","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			srcFormat:   "openai",
			contains:    []string{`"model":"meta/muse-spark-1.2-contributor"`, `"reasoning_effort":"low"`},
			notContains: []string{`"model":"meta/muse-spark-1.2-contributor(low)"`},
		},
		{
			name:        "model suffix budget sets reasoning_effort and strips suffix from model",
			model:       "meta/muse-spark-1.2-contributor(16384)",
			payload:     `{"model":"meta/muse-spark-1.2-contributor(16384)","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			srcFormat:   "openai",
			contains:    []string{`"model":"meta/muse-spark-1.2-contributor"`, `"reasoning_effort":"high"`},
			notContains: []string{`"model":"meta/muse-spark-1.2-contributor(16384)"`},
		},
		{
			name:        "model suffix none omits reasoning_effort and strips suffix from model",
			model:       "meta/muse-spark-1.2-contributor(none)",
			payload:     `{"model":"meta/muse-spark-1.2-contributor(none)","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			srcFormat:   "openai",
			contains:    []string{`"model":"meta/muse-spark-1.2-contributor"`},
			notContains: []string{`"reasoning_effort"`, `(none)`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := tt.model
			if model == "" {
				model = "deepseek/deepseek-v4-pro"
			}
			req := cliproxyexecutor.Request{
				Model:   model,
				Payload: []byte(tt.payload),
			}
			opts := cliproxyexecutor.Options{
				Stream:       true,
				SourceFormat: sdktranslator.FromString(tt.srcFormat),
			}
			body := exec.buildRequestBody(req, opts, nil)
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

func TestCanonicalizeCommandCodeModel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"claude-sonnet-5", "claude-sonnet-5"},
		{"anthropic:claude-sonnet-5", "claude-sonnet-5"},
		{"openai:gpt-5.6-sol", "gpt-5.6-sol"},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001"},
		{"claude-opus-4-6", "claude-opus-4-7"},
		{"claude-sonnet-4-6-20260215", "claude-sonnet-4-6"},
		{"claude-sonnet-4-6@20260215", "claude-sonnet-4-6"},
		{"claude-fable-5-1", "claude-fable-5-1"},
		{"google/gemini-3.8-flash", "google/gemini-3.8-flash"},
		{"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-pro"},
		{"deepseek/deepseek-v4-flash-fast", "deepseek/deepseek-v4-flash-fast"},
		{"z-ai/glm-5.3-flash", "z-ai/glm-5.3-flash"},
		{"Qwen/Qwen3.8-Max-0902", "Qwen/Qwen3.8-Max-0902"},
		{"anthropic:claude-sonnet-5(high)", "claude-sonnet-5"},
	}
	for _, tt := range tests {
		if got := canonicalizeCommandCodeModel(tt.in); got != tt.want {
			t.Errorf("canonicalizeCommandCodeModel(%q) = %q, want %q", tt.in, got, tt.want)
		}
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
	exec.injectHeaders(httpReq, auth, false)

	// Verify x-* headers are stored in lowercase (not Go's Title-Case canonicalization).
	// Note: Header.Get() canonicalizes the key, so we use bracket notation.
	getLower := func(key string) string {
		vals, ok := httpReq.Header[key]
		if !ok || len(vals) == 0 {
			return ""
		}
		return vals[0]
	}
	for _, lowerKey := range []string{"x-cli-environment", "x-command-code-version", "x-session-id", "x-project-slug", "x-taste-learning", "x-co-flag", "traceparent", "accept", "accept-language", "accept-encoding"} {
		if v := getLower(lowerKey); v == "" {
			t.Errorf("missing required CLI header %q (lowercase)", lowerKey)
		}
	}

	if got := getLower("x-command-code-version"); got != helps.CCCLIVersion {
		t.Errorf("x-command-code-version = %q, want %s", got, helps.CCCLIVersion)
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
	// Optional headers from command-code@1.44.0: only sent when configured.
	if got := getLower("x-oss-primary-provider"); got != "" {
		t.Errorf("x-oss-primary-provider = %q, want empty by default", got)
	}
	if got := getLower("x-cmd-zdr"); got != "" {
		t.Errorf("x-cmd-zdr = %q, want empty by default", got)
	}
	if got := getLower("x-cmd-provider-deepseek-internal"); got != "" {
		t.Errorf("x-cmd-provider-deepseek-internal = %q, want empty by default", got)
	}
	// x-project-slug should match the seeded session project name (not a hard-coded
	// "workspace" when session context is available).
	if got := getLower("x-project-slug"); got == "" {
		t.Errorf("x-project-slug is empty")
	}
	// Non-stream requests should send full accept-encoding (matching undici).
	if got := getLower("accept-encoding"); got != "gzip, deflate, br" {
		t.Errorf("accept-encoding = %q, want \"gzip, deflate, br\"", got)
	}
	// Authorization uses Title-Case (matching official CLI), so Get() works.
	if got := httpReq.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", got)
	}
	// User-Agent must match the official CLI (lowercase "cli") to avoid the
	// server-side "Proxy use detected" rejection.
	if got := getLower("User-Agent"); got != "cli" {
		t.Errorf("User-Agent = %q, want cli", got)
	}

	// Verify no Title-Case duplicates exist for the x-* headers.
	// (User-Agent's canonical form is itself "User-Agent" with caps, so it is
	// always present and excluded from this check.)
	for _, titleKey := range []string{"X-Cli-Environment", "X-Command-Code-Version", "X-Session-Id", "X-Project-Slug", "X-Taste-Learning", "X-Co-Flag", "Traceparent"} {
		if _, ok := httpReq.Header[titleKey]; ok {
			t.Errorf("Title-Case header key %q should not exist (use lowercase)", titleKey)
		}
	}

	// Stream requests should use accept-encoding: identity.
	streamReq, errStream := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", nil)
	if errStream != nil {
		t.Fatalf("failed to create stream request: %v", errStream)
	}
	exec.injectHeaders(streamReq, auth, true)
	if got := getLowerFromReq(streamReq, "accept-encoding"); got != "identity" {
		t.Errorf("stream accept-encoding = %q, want identity", got)
	}

	// Session id must be stable across requests for the same api key
	// (official CLI reuses one session for the process lifetime).
	session1 := getLower("x-session-id")
	httpReq2, _ := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", nil)
	exec.injectHeaders(httpReq2, auth, false)
	session2 := getLowerFromReq(httpReq2, "x-session-id")
	if session1 == "" || session1 != session2 {
		t.Errorf("x-session-id not stable across requests: %q vs %q", session1, session2)
	}

	// Optional headers when auth attributes are set (mirrors env flags in CLI).
	authWithOpts := &cliproxyauth.Auth{
		ID:       "test-auth-opts",
		Provider: "commandcode",
		Attributes: map[string]string{
			"api_key":                        "test-key",
			"oss_primary_provider":           "openrouter",
			"cmd_zdr":                        "1",
			"cmd_provider_deepseek_internal": "1",
		},
	}
	httpReq3, _ := http.NewRequest(http.MethodPost, "https://api.commandcode.ai/alpha/generate", nil)
	exec.injectHeaders(httpReq3, authWithOpts, false)
	if got := getLowerFromReq(httpReq3, "x-oss-primary-provider"); got != "openrouter" {
		t.Errorf("x-oss-primary-provider = %q, want openrouter", got)
	}
	if got := getLowerFromReq(httpReq3, "x-cmd-zdr"); got != "1" {
		t.Errorf("x-cmd-zdr = %q, want 1", got)
	}
	if got := getLowerFromReq(httpReq3, "x-cmd-provider-deepseek-internal"); got != "1" {
		t.Errorf("x-cmd-provider-deepseek-internal = %q, want 1", got)
	}
}

func getLowerFromReq(req *http.Request, key string) string {
	vals, ok := req.Header[key]
	if !ok || len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func TestCommandCodeExecutor_ExecuteStream_ToolResultAndStructuredError(t *testing.T) {
	// Tests server-side tool-result event is safely consumed without erroring,
	// and verifies parseCommandCodeStreamErrorMessage parses embedded JSON errors.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"tool-result","toolCallId":"call_fetch","toolName":"web_fetch","output":{"type":"text","value":"content"}}` + "\n"))
		w.Write([]byte(`{"type":"text-delta","text":"Result received."}` + "\n"))
		w.Write([]byte(`{"type":"finish","finishReason":"end_turn","totalUsage":{"inputTokens":15,"outputTokens":5}}` + "\n"))
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
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"fetch web"}],"stream":true}`),
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
			t.Fatalf("unexpected chunk error: %v", ch.Err)
		}
		if ch.Payload != nil {
			chunks = append(chunks, string(ch.Payload))
		}
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "Result received.") {
		t.Errorf("missing text delta after tool-result: %s", joined)
	}

	// Verify parseCommandCodeStreamErrorMessage with embedded JSON
	plainErr := parseCommandCodeStreamErrorMessage([]byte(`{"type":"error","error":"plain message"}`))
	if plainErr != "plain message" {
		t.Errorf("got %q, want 'plain message'", plainErr)
	}
	embeddedErr := parseCommandCodeStreamErrorMessage([]byte(`{"type":"error","error":"500 {\"error\":{\"message\":\"quota exceeded\"}}"}`))
	if embeddedErr != "quota exceeded" {
		t.Errorf("got %q, want 'quota exceeded'", embeddedErr)
	}
}
