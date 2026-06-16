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
//
// Output format depends on the request endpoint:
//   - /v1/chat/completions (SourceFormat="openai"): outputs OpenAI SSE chunks
//   - /v1/responses (SourceFormat="openai-response"): outputs Codex response format
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

		isResponses := opts.SourceFormat.String() == "openai-response"
		log.Infof("[commandcode] ExecuteStream start model=%s sourceFormat=%s isResponses=%v", req.Model, opts.SourceFormat, isResponses)

		var (
			finished         bool
			usageSent        bool
			accText          strings.Builder
			responsesInit    bool
			promptTokens     int64
			completionTokens int64
			chunkCount       int
			textChunkCount   int
		)

		for scanner.Scan() {
			chunkCount++
			line := bytes.TrimSpace(scanner.Bytes())
			appendAPIResponseChunk(ctx, e.cfg, line)
			if len(line) == 0 {
				continue
			}

			if bytes.HasPrefix(line, []byte("data:")) {
				line = bytes.TrimSpace(line[5:])
			}

			chunkType := gjson.GetBytes(line, "type").String()

			switch {
			case chunkType == "text-delta":
				textChunkCount++
				text := gjson.GetBytes(line, "text").String()
				if text == "" {
					continue
				}
				accText.WriteString(text)

				if isResponses {
					if !responsesInit {
						responsesInit = true
						log.Info("[commandcode] emitting Codex response init events (response.created, in_progress, output_item.added, content_part.added)")
						out <- cliproxyexecutor.StreamChunk{Payload: responsesCreated()}
						out <- cliproxyexecutor.StreamChunk{Payload: responsesInProgress()}
						out <- cliproxyexecutor.StreamChunk{Payload: responsesOutputItemAdded()}
						out <- cliproxyexecutor.StreamChunk{Payload: responsesContentPartAdded()}
					}
					out <- cliproxyexecutor.StreamChunk{Payload: responsesTextDelta(text)}
				} else {
					out <- cliproxyexecutor.StreamChunk{Payload: buildCCChunk(text)}
				}

			case chunkType == "finish":
				finished = true
				log.Infof("[commandcode] finish event received: isResponses=%v promptTokens=%d completionTokens=%d",
					isResponses, promptTokens, completionTokens)
				usageNode := gjson.GetBytes(line, "totalUsage")
				if usageNode.Exists() && !usageSent {
					usageSent = true
					promptTokens = usageNode.Get("inputTokens").Int()
					completionTokens = usageNode.Get("outputTokens").Int()
					detail := usage.Detail{
						InputTokens:  promptTokens,
						OutputTokens: completionTokens,
					}
					if cached := usageNode.Get("inputTokenDetails.cacheReadTokens"); cached.Exists() {
						detail.CachedTokens = cached.Int()
					}
					detail.TotalTokens = promptTokens + completionTokens
					if detail.TotalTokens == 0 {
						detail.TotalTokens = usageNode.Get("totalTokens").Int()
					}
					reporter.publish(ctx, detail)
				}

				if isResponses {
					fullText := accText.String()
					log.Infof("[commandcode] emitting Codex completion events: textDone, contentPartDone, outputItemDone, completed (textLen=%d)", len(fullText))
					out <- cliproxyexecutor.StreamChunk{Payload: responsesTextDone(fullText)}
					out <- cliproxyexecutor.StreamChunk{Payload: responsesContentPartDone(fullText)}
					out <- cliproxyexecutor.StreamChunk{Payload: responsesOutputItemDone(fullText)}
					out <- cliproxyexecutor.StreamChunk{Payload: responsesCompleted(promptTokens, completionTokens)}
					log.Infof("[commandcode] response.completed emitted with promptTokens=%d completionTokens=%d", promptTokens, completionTokens)
				} else {
					out <- cliproxyexecutor.StreamChunk{Payload: buildCCFinishChunk(promptTokens, completionTokens)}
				}

			case chunkType == "error":
				msg := gjson.GetBytes(line, "error.message").String()
				if msg == "" {
					msg = gjson.GetBytes(line, "error").String()
				}
				if msg == "" {
					msg = "stream error"
				}
				log.Debugf("commandcode: stream error msg=%s", msg)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("%s", msg)}

			case chunkType == "abort":
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("aborted")}
				return

			default:
				// reasoning-start/delta/end, tool-call, tool-result — skip
			}
		}

		if errScan := scanner.Err(); errScan != nil {
			log.Errorf("[commandcode] scanner error: %v", errScan)
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else if !finished {
			log.Warnf("[commandcode] stream ended without finish event: chunkCount=%d textChunkCount=%d isResponses=%v",
				chunkCount, textChunkCount, isResponses)
			if isResponses {
				out <- cliproxyexecutor.StreamChunk{Payload: responsesCompleted(0, 0)}
			} else {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
			}
		} else {
			log.Infof("[commandcode] stream completed: chunkCount=%d textChunkCount=%d textLen=%d isResponses=%v promptTokens=%d completionTokens=%d",
				chunkCount, textChunkCount, accText.Len(), isResponses, promptTokens, completionTokens)
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
// - Filters out system messages (sent via params.system instead).
// - Converts string content to array-of-blocks format: [{"type":"text","text":"..."}].
func normalizeCCMessages(payload []byte) string {
	msgs := gjson.GetBytes(payload, "messages")
	if !msgs.Exists() {
		return "[]"
	}

	converted := make([]json.RawMessage, 0, len(msgs.Array()))
	for _, msg := range msgs.Array() {
		role := msg.Get("role").String()
		if role == "system" {
			continue
		}

		content := msg.Get("content")
		// Convert string content to array of text blocks
		if content.Type == gjson.String {
			text := content.String()
			contentRaw := fmt.Sprintf(`[{"type":"text","text":%s}]`, ccEncode(text))
			converted = append(converted, json.RawMessage(
				fmt.Sprintf(`{"role":%s,"content":%s}`, ccEncode(role), contentRaw),
			))
		} else {
			// Already array or other — rebuild role+content
			converted = append(converted, json.RawMessage(
				fmt.Sprintf(`{"role":%s,"content":%s}`, ccEncode(role), content.Raw),
			))
		}
	}

	if len(converted) == 0 {
		return "[]"
	}

	var sb strings.Builder
	sb.WriteByte('[')
	for i, m := range converted {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.Write(m)
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

// ── Helpers: Codex response format (for /v1/responses endpoint) ─────────
// The proxy's ConvertCodexResponseToOpenAI consumes these events and
// converts them to OpenAI SSE for the client.

func responsesCreated() []byte {
	return []byte(`data: {"type":"response.created","response":{"id":"chatcmpl-cc","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}` + "\n\n")
}

func responsesInProgress() []byte {
	return []byte(`data: {"type":"response.in_progress","response":{"id":"chatcmpl-cc","object":"response","created_at":0,"status":"in_progress"}}` + "\n\n")
}

func responsesOutputItemAdded() []byte {
	return []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_chatcmpl-cc_0","type":"message","status":"in_progress","content":[],"role":"assistant"}}` + "\n\n")
}

func responsesContentPartAdded() []byte {
	return []byte(`data: {"type":"response.content_part.added","item_id":"msg_chatcmpl-cc_0","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}` + "\n\n")
}

func responsesTextDelta(text string) []byte {
	return []byte(fmt.Sprintf(`data: {"type":"response.output_text.delta","item_id":"msg_chatcmpl-cc_0","output_index":0,"content_index":0,"delta":%s,"logprobs":[]}%s`,
		ccEncode(text), "\n\n"))
}

func responsesTextDone(text string) []byte {
	return []byte(fmt.Sprintf(`data: {"type":"response.output_text.done","item_id":"msg_chatcmpl-cc_0","output_index":0,"content_index":0,"text":%s,"logprobs":[]}%s`,
		ccEncode(text), "\n\n"))
}

func responsesContentPartDone(text string) []byte {
	return []byte(fmt.Sprintf(`data: {"type":"response.content_part.done","item_id":"msg_chatcmpl-cc_0","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":%s}}%s`,
		ccEncode(text), "\n\n"))
}

func responsesOutputItemDone(text string) []byte {
	return []byte(fmt.Sprintf(`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_chatcmpl-cc_0","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":%s}],"role":"assistant"}}%s`,
		ccEncode(text), "\n\n"))
}

func responsesCompleted(promptTokens, completionTokens int64) []byte {
	return []byte(fmt.Sprintf(`data: {"type":"response.completed","response":{"id":"chatcmpl-cc","object":"response","created_at":0,"status":"completed","model":"commandcode","output":[],"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}%s`,
		promptTokens, completionTokens, promptTokens+completionTokens, "\n\n"))
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
	return []byte(fmt.Sprintf(`{"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, ccEncode(text)))
}

func buildCCFinishChunk(promptTokens, completionTokens int64) []byte {
	if promptTokens > 0 || completionTokens > 0 {
		return []byte(fmt.Sprintf(`{"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			promptTokens, completionTokens, promptTokens+completionTokens))
	}
	return []byte(`{"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
}
