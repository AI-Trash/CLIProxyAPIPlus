package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	commandCodeDefaultBaseURL = "https://api.commandcode.ai"
	commandCodeUserAgent      = "cli-proxy-commandcode"
	ccHeaderProdEnv           = "production"
	ccHeaderVersion           = "0.38.2"
)

// CommandCodeExecutor implements ProviderExecutor for the Command Code CLI's
// /alpha/generate endpoint. This is the private protocol used by the command-code
// CLI itself — distinct from the provider API (/provider/v1/*) which may be
// restricted for certain subscription tiers.
type CommandCodeExecutor struct {
	cfg *config.Config
}

func NewCommandCodeExecutor(cfg *config.Config) *CommandCodeExecutor {
	return &CommandCodeExecutor{cfg: cfg}
}

func (e *CommandCodeExecutor) Identifier() string { return "commandcode" }

// HttpRequest injects credentials into an HTTP request.
func (e *CommandCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("commandcode: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	e.injectHeaders(httpReq, auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *CommandCodeExecutor) injectHeaders(req *http.Request, auth *cliproxyauth.Auth) {
	apiKey := e.resolveAPIKey(auth)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("x-cli-environment", ccHeaderProdEnv)
	req.Header.Set("x-command-code-version", ccHeaderVersion)

	if auth != nil && auth.Metadata != nil {
		if tok, ok := auth.Metadata["oauth_token"].(string); ok && tok != "" {
			req.Header.Set("x-oauth-token", "Bearer "+tok)
		}
		if prov, ok := auth.Metadata["oauth_provider"].(string); ok && prov != "" {
			req.Header.Set("x-oauth-provider", prov)
		}
	}
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}
}

func (e *CommandCodeExecutor) resolveAPIKey(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if k := strings.TrimSpace(auth.Attributes["api_key"]); k != "" {
			return k
		}
	}
	if auth.Metadata != nil {
		if k, ok := auth.Metadata["api_key"].(string); ok {
			return strings.TrimSpace(k)
		}
	}
	return ""
}

func (e *CommandCodeExecutor) resolveBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if auth.Attributes != nil {
			if u := strings.TrimSpace(auth.Attributes["base_url"]); u != "" {
				return u
			}
		}
		if auth.Metadata != nil {
			if u, ok := auth.Metadata["base_url"].(string); ok {
				return strings.TrimSpace(u)
			}
		}
	}
	return commandCodeDefaultBaseURL
}

// Execute performs a non-streaming request via /alpha/generate.
func (e *CommandCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL := e.resolveBaseURL(auth)
	apiKey := e.resolveAPIKey(auth)
	if apiKey == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing command-code api key"}
		return
	}

	ccBody := e.buildRequestBody(req, opts, false)

	httpReq, err := e.buildHTTPRequest(ctx, baseURL, "/alpha/generate", apiKey, ccBody, auth)
	if err != nil {
		return resp, err
	}

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer httpResp.Body.Close()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		log.Debugf("commandcode: error status=%d body=%s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, body)

	openAIResp, detail := e.convertNonStreamToOpenAI(body)
	if detail.TotalTokens > 0 || detail.InputTokens > 0 || detail.OutputTokens > 0 {
		reporter.publish(ctx, detail)
	}
	reporter.ensurePublished(ctx)

	return cliproxyexecutor.Response{Payload: openAIResp, Headers: httpResp.Header.Clone()}, nil
}

// ExecuteStream performs a streaming request via /alpha/generate.
// The response is line-delimited JSON (NOT standard SSE with "data:" prefix).
// Each line is a JSON object like {"type":"text-delta","text":"hello"}.
func (e *CommandCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	baseURL := e.resolveBaseURL(auth)
	apiKey := e.resolveAPIKey(auth)
	if apiKey == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing command-code api key"}
		return nil, err
	}

	from := opts.SourceFormat
	ccBody := e.buildRequestBody(req, opts, true)

	httpReq, err := e.buildHTTPRequest(ctx, baseURL, "/alpha/generate", apiKey, ccBody, auth)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		log.Debugf("commandcode: stream error status=%d body=%s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		httpResp.Body.Close()
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer httpResp.Body.Close()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		to := sdktranslator.FromString("openai")

		var (
			finished   bool
			usageSent  bool
			accText    strings.Builder
		)

		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, line)
			if len(line) == 0 {
				continue
			}

			// Alpha generate streams raw JSON lines, NOT "data:" prefix SSE.
			// Skip this block: our executor now handles raw JSON directly.
			// But to be safe, strip any "data:" prefix if present.
			if bytes.HasPrefix(line, []byte("data:")) {
				line = bytes.TrimSpace(line[5:])
			}

			chunkType := gjson.GetBytes(line, "type").String()

			switch {
			case chunkType == "text-delta":
				text := gjson.GetBytes(line, "text").String()
				if text != "" {
					accText.WriteString(text)
					chunk := buildCCChunk(text)
					chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, ccBody, chunk, nil)
					for i := range chunks {
						out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
					}
				}

			case chunkType == "finish":
				finished = true
				usageNode := gjson.GetBytes(line, "totalUsage")
				if usageNode.Exists() && !usageSent {
					usageSent = true
					detail := usage.Detail{
						InputTokens:  usageNode.Get("inputTokens").Int(),
						OutputTokens: usageNode.Get("outputTokens").Int(),
					}
					if cached := usageNode.Get("inputTokenDetails.cacheReadTokens"); cached.Exists() {
						detail.CachedTokens = cached.Int()
					}
					detail.TotalTokens = detail.InputTokens + detail.OutputTokens
					if detail.TotalTokens == 0 {
						detail.TotalTokens = usageNode.Get("totalTokens").Int()
					}
					reporter.publish(ctx, detail)
				}
				finishChunk := buildCCFinishChunk()
				chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, ccBody, finishChunk, nil)
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
				}

			case chunkType == "error":
				msg := gjson.GetBytes(line, "error.message").String()
				if msg == "" {
					msg = gjson.GetBytes(line, "error").String()
				}
				if msg == "" {
					msg = "stream error"
				}
				isRetryable := gjson.GetBytes(line, "error.isRetryable").Bool()
				statusCode := gjson.GetBytes(line, "error.statusCode").Int()
				log.Debugf("commandcode: stream error msg=%s retryable=%v status=%d", msg, isRetryable, statusCode)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("%s", msg)}

			case chunkType == "abort":
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("aborted")}
				return

			default:
				// reasoning-start, reasoning-delta, reasoning-end, tool-call, tool-result
				// For now, silently skip these since we're translating to OpenAI chat format
				// which doesn't natively expose reasoning blocks (use passthrough-headers if needed)
			}
		}

		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else if !finished {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
		}
		reporter.ensurePublished(ctx)
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *CommandCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	translated, err := thinking.ApplyThinking(translated, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, err := tokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("commandcode: tokenizer init failed: %w", err)
	}
	count, err := countOpenAIChatTokens(enc, translated)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("commandcode: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translatedUsage)}, nil
}

func (e *CommandCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("commandcode: refresh (no-op)")
	return auth, nil
}

// --- internal helpers ---

func (e *CommandCodeExecutor) buildHTTPRequest(ctx context.Context, baseURL, endpoint, apiKey string, body []byte, auth *cliproxyauth.Auth) (*http.Request, error) {
	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	e.injectHeaders(httpReq, auth)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	return httpReq, nil
}

func (e *CommandCodeExecutor) buildRequestBody(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) []byte {
	payload := req.Payload
	model := req.Model

	// Convert messages: command-code expects content as array of blocks, not plain strings
	messages := normalizeCCMessages(payload)
	if messages == "" {
		messages = "[]"
	}

	maxTokens := gjson.GetBytes(payload, "max_tokens").Int()
	if maxTokens == 0 {
		maxTokens = gjson.GetBytes(payload, "max_completion_tokens").Int()
	}
	if maxTokens == 0 {
		maxTokens = 64000
	}

	temperature := gjson.GetBytes(payload, "temperature").Float()
	if temperature == 0 {
		temperature = 0.3
	}

	// Build params object
	params := fmt.Sprintf(`"model":%s,"messages":%s,"max_tokens":%d,"temperature":%f,"stream":%v`,
		ccEncode(model), messages, maxTokens, temperature, stream)

	// command-code requires a full config block
	body := fmt.Sprintf(
		`{"params":{%s},"mode":"tool-desc","config":{"environment":"production","workingDir":"/workspace","date":"%s","structure":[],"isGitRepo":false,"currentBranch":"","mainBranch":"","gitStatus":"","recentCommits":[]},"memory":"","taste":"","skills":""}`,
		params, time.Now().UTC().Format("2006-01-02"))

	// Add system prompt if present
	if system := extractSystemFromMessages(payload); system != "" {
		body, _ = sjson.Set(body, "params.system", system)
	}

	if cfg := gjson.GetBytes(payload, "reasoning_effort"); cfg.Exists() {
		body, _ = sjson.Set(body, "params.reasoning_effort", cfg.Value())
	}

	return []byte(body)
}

// normalizeCCMessages converts OpenAI-format messages to command-code's expected format.
// The API expects content as an array of blocks, e.g. [{"type":"text","text":"hi"}].
func normalizeCCMessages(payload []byte) string {
	msgs := gjson.GetBytes(payload, "messages")
	if !msgs.Exists() {
		return "[]"
	}

	var sb strings.Builder
	sb.WriteByte('[')
	first := true
	for _, msg := range msgs.Array() {
		role := msg.Get("role").String()
		if role == "" {
			continue
		}
		content := msg.Get("content")

		// Convert string content to array format
		var contentJSON string
		if content.Type == gjson.String {
			text := content.String()
			contentJSON = fmt.Sprintf(`[{"type":"text","text":%s}]`, ccEncode(text))
		} else if content.IsArray() {
			contentJSON = content.Raw
		} else {
			contentJSON = content.Raw
		}

		if !first {
			sb.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&sb, `{"role":%s,"content":%s}`, ccEncode(role), contentJSON)
	}
	sb.WriteByte(']')
	return sb.String()
}

func extractSystemFromMessages(payload []byte) string {
	for _, msg := range gjson.GetBytes(payload, "messages").Array() {
		if msg.Get("role").String() == "system" {
			content := msg.Get("content")
			if content.IsArray() {
				for _, part := range content.Array() {
					if part.Get("type").String() == "text" {
						return part.Get("text").String()
					}
				}
			}
			return content.String()
		}
	}
	return ""
}

func (e *CommandCodeExecutor) convertNonStreamToOpenAI(body []byte) ([]byte, usage.Detail) {
	zero := usage.Detail{}

	// If the response is already OpenAI-compatible, pass through
	if gjson.GetBytes(body, "choices").Exists() {
		return body, parseOpenAIUsage(body)
	}

	// Parse command-code response format (might be a single text object)
	text := gjson.GetBytes(body, "text").String()
	if text == "" {
		text = gjson.GetBytes(body, "content").String()
	}
	if text == "" {
		// Try parsing as array of text-delta-type events
		if gjson.ValidBytes(body) {
			var result bytes.Buffer
			arr := gjson.ParseBytes(body)
			if arr.IsArray() {
				for _, evt := range arr.Array() {
					if evt.Get("type").String() == "text-delta" {
						result.WriteString(evt.Get("text").String())
					}
				}
				text = result.String()
			}
		}
	}
	if text == "" {
		return body, zero
	}

	openAIResp := fmt.Sprintf(`{"id":"chatcmpl-%s","object":"chat.completion","created":0,"model":"commandcode","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}]}`,
		uuid.New().String(), ccEncode(text))

	var detail usage.Detail
	if u := gjson.GetBytes(body, "usage"); u.Exists() {
		detail = usage.Detail{
			InputTokens:  u.Get("input_tokens").Int(),
			OutputTokens: u.Get("output_tokens").Int(),
		}
		detail.TotalTokens = detail.InputTokens + detail.OutputTokens
	}
	return []byte(openAIResp), detail
}

func ccEncode(v string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return string(b)
}

func buildCCChunk(text string) []byte {
	return []byte(fmt.Sprintf(`data: {"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}%s`, ccEncode(text), "\n\n"))
}

func buildCCFinishChunk() []byte {
	return []byte(`data: {"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + `data: [DONE]` + "\n\n")
}
