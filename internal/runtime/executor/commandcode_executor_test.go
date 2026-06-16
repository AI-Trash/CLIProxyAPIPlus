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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`{"type":"text-delta","text":"Hello"}` + "\n"))
		w.Write([]byte(`{"type":"text-delta","text":" world"}` + "\n"))
		w.Write([]byte(`{"type":"finish","totalUsage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}` + "\n"))
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
		Payload: []byte(`{"model":"deepseek-v4-pro","input":"hi","stream":true}`),
	}
	opts := cliproxyexecutor.Options{
		Stream:         true,
		SourceFormat:   sdktranslator.FromString("openai-response"),
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

	// TranslateStream converts OpenAI SSE to source format. For openai-response,
	// the translator produces response.created through response.output_item.done.
	// response.completed depends on the handler's param state.
	if !strings.Contains(joined, `"type":"response.output_item.done"`) {
		t.Errorf("missing response.output_item.done\nGot:\n%s", joined)
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
		SourceFormat:   sdktranslator.FromString("openai"),
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
}

func TestCommandCodeExecutor_buildRequestBody(t *testing.T) {
	exec := NewCommandCodeExecutor(&config.Config{})

	tests := []struct {
		name      string
		payload   string
		srcFormat string
		contains  []string
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
			body := exec.buildRequestBody(req, opts, true)
			bodyStr := string(body)
			for _, c := range tt.contains {
				if !strings.Contains(bodyStr, c) {
					t.Errorf("%s: missing %q\nGot: %s", tt.name, c, bodyStr)
				}
			}
		})
	}
}
