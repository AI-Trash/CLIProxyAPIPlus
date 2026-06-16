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
)

func TestCommandCodeExecutor_ExecuteStream_CodexResponseFormat(t *testing.T) {
	// Mock upstream that returns command-code SSE format
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Simulate /alpha/generate response: raw JSON lines
		w.Write([]byte(`{"type":"text-delta","text":"Hello"}` + "\n"))
		w.Write([]byte(`{"type":"text-delta","text":" world"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}` + "\n"))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "test-auth",
		Provider: "commandcode",
		Metadata: map[string]any{
			"api_key":    "test-api-key",
			"base_url":   upstream.URL,
		},
	}
	auth.Attributes = map[string]string{
		"base_url": upstream.URL,
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek/deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FromString("openai-response"), // /v1/responses endpoint
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

	// Verify all required Codex response events are present
	joined := strings.Join(chunks, "\n")

	requiredEvents := []string{
		`"type":"response.created"`,
		`"type":"response.in_progress"`,
		`"type":"response.output_item.added"`,
		`"type":"response.content_part.added"`,
		`"type":"response.output_text.delta"`,
		`"type":"response.output_text.done"`,
		`"type":"response.content_part.done"`,
		`"type":"response.output_item.done"`,
		`"type":"response.completed"`,
	}

	for _, evt := range requiredEvents {
		if !strings.Contains(joined, evt) {
			t.Errorf("missing expected Codex event: %s\nGot:\n%s", evt, joined)
		}
	}

	// Verify usage in response.completed
	if !strings.Contains(joined, `"input_tokens":10`) {
		t.Errorf("response.completed missing input_tokens\nGot:\n%s", joined)
	}
	if !strings.Contains(joined, `"output_tokens":5`) {
		t.Errorf("response.completed missing output_tokens\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_ExecuteStream_OpenAIFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"Hello"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":5}}` + "\n"))
	}))
	defer upstream.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "test-auth",
		Provider: "commandcode",
		Metadata: map[string]any{"api_key": "test-key", "base_url": upstream.URL},
		Attributes: map[string]string{"base_url": upstream.URL},
	}

	req := cliproxyexecutor.Request{
		Model:   "deepseek/deepseek-v4-pro",
		Payload: []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}],"stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FromString("openai"), // /v1/chat/completions endpoint
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

	// OpenAI format: should have choices delta content
	if !strings.Contains(joined, `"delta":{"content":"Hello"}`) {
		t.Errorf("missing OpenAI content delta\nGot:\n%s", joined)
	}

	// Should have finish_reason
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason\nGot:\n%s", joined)
	}

	// Should have usage
	if !strings.Contains(joined, `"prompt_tokens":10`) {
		t.Errorf("missing prompt_tokens usage\nGot:\n%s", joined)
	}

	// Should NOT have Codex response events
	if strings.Contains(joined, `"type":"response.created"`) {
		t.Errorf("OpenAI format should NOT contain Codex response.created\nGot:\n%s", joined)
	}
}

func TestCommandCodeExecutor_buildRequestBody_ContentFormat(t *testing.T) {
	exec := NewCommandCodeExecutor(&config.Config{})

	tests := []struct {
		name     string
		payload  string
		contains []string
		excludes []string
	}{
		{
			name: "converts string content to array",
			payload: `{"model":"test","messages":[
				{"role":"user","content":"hello"}
			]}`,
			contains: []string{
				`"type":"text"`,
				`"text":"hello"`,
				`"role":"user"`,
			},
		},
		{
			name: "filters out system messages",
			payload: `{"model":"test","messages":[
				{"role":"system","content":"you are helpful"},
				{"role":"user","content":"hi"}
			]}`,
			contains: []string{`"role":"user"`},
			excludes: []string{`"role":"system"`},
		},
		{
			name: "includes required config fields",
			payload: `{"model":"test","messages":[{"role":"user","content":"hi"}]}`,
			contains: []string{
				`"workingDir"`,
				`"date"`,
				`"isGitRepo"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{
				Model:   "deepseek/deepseek-v4-pro",
				Payload: []byte(tt.payload),
			}
			opts := cliproxyexecutor.Options{Stream: true}
			body := exec.buildRequestBody(req, opts, true)
			bodyStr := string(body)
			for _, c := range tt.contains {
				if !strings.Contains(bodyStr, c) {
					t.Errorf("%s: body should contain %q\nGot: %s", tt.name, c, bodyStr)
				}
			}
			for _, c := range tt.excludes {
				if strings.Contains(bodyStr, c) {
					t.Errorf("%s: body should NOT contain %q\nGot: %s", tt.name, c, bodyStr)
				}
			}
		})
	}
}
