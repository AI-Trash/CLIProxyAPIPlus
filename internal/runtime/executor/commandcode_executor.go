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

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, translated, openAIResp, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
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

		to := sdktranslator.FromString("openai")
		responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
		translated := sdktranslator.TranslateRequest(opts.SourceFormat, to, baseModel, req.Payload, true)
		var param any

		var (
			finished         bool
			doneSent         bool
			usageSent        bool
			sawToolCall      bool
			accText          strings.Builder
			promptTokens     int64
			completionTokens int64
			chunkCount       int
			textChunkCount   int
			toolCallCount    int
		)

		log.Infof("[commandcode] ExecuteStream start model=%s sourceFormat=%s", req.Model, opts.SourceFormat)

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

				// Build OpenAI SSE chunk, then translate to source format
				chunk := buildCCChunk(text)
				emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, chunk, &param)

			case chunkType == "tool-call":
				id := gjson.GetBytes(line, "toolCallId").String()
				if id == "" {
					id = gjson.GetBytes(line, "id").String()
				}
				if id == "" {
					id = "call_" + strings.ReplaceAll(uuid.New().String(), "-", "")
				}
				name := gjson.GetBytes(line, "toolName").String()
				if name == "" {
					name = gjson.GetBytes(line, "name").String()
				}
				if name == "" {
					log.Debugf("commandcode: skipping tool-call with empty name")
					continue
				}

				sawToolCall = true
				toolIndex := toolCallCount
				toolCallCount++
				arguments := commandCodeToolArguments(line)
				chunk := buildCCToolCallChunk(toolIndex, id, name, arguments)
				emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, chunk, &param)

			case chunkType == "finish":
				finished = true
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
				finishReason := mapCommandCodeFinishReason(gjson.GetBytes(line, "finishReason").String(), sawToolCall)
				chunk := buildCCFinishChunk(promptTokens, completionTokens, finishReason)
				emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, chunk, &param)
				emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]"), &param)
				doneSent = true

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
				// reasoning/tool events — skip
			}
		}

		if errScan := scanner.Err(); errScan != nil {
			log.Errorf("[commandcode] scanner error: %v", errScan)
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		} else if !finished {
			log.Warnf("[commandcode] stream ended without finish event: chunkCount=%d", chunkCount)
			emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, buildCCFinishChunk(promptTokens, completionTokens, mapCommandCodeFinishReason("", sawToolCall)), &param)
			emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]"), &param)
			doneSent = true
		} else {
			log.Infof("[commandcode] stream completed: chunkCount=%d textChunkCount=%d toolCallCount=%d textLen=%d promptTokens=%d completionTokens=%d",
				chunkCount, textChunkCount, toolCallCount, accText.Len(), promptTokens, completionTokens)
		}
		if finished && !doneSent {
			emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, []byte("data: [DONE]"), &param)
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

	// Use the proxy's built-in translator to convert from source format to
	// OpenAI format — this properly resolves content nesting.
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	translated := sdktranslator.TranslateRequest(from, to, model, payload, stream)

	// Command-code expects: content as array-of-blocks, system in params.system
	messages, sysPrompt := convertMessagesForCC(translated)
	if messages == "" || messages == "[]" {
		if input := gjson.GetBytes(payload, "input"); input.Exists() {
			if input.Type == gjson.String {
				messages = fmt.Sprintf(`[{"role":"user","content":[{"type":"text","text":%s}]}]`, ccEncode(input.String()))
			} else if input.IsArray() {
				messages = fmt.Sprintf(`[{"role":"user","content":%s}]`, input.Raw)
			}
		}
	}
	if messages == "" {
		messages = "[]"
	}

	maxTokens := gjson.GetBytes(translated, "max_tokens").Int()
	if maxTokens == 0 {
		maxTokens = gjson.GetBytes(translated, "max_completion_tokens").Int()
	}
	if maxTokens == 0 {
		maxTokens = 64000
	}

	temperature := gjson.GetBytes(translated, "temperature").Float()
	if temperature == 0 {
		temperature = 0.3
	}

	// System prompt from translation takes priority; fall back to instructions
	if sysPrompt == "" {
		sysPrompt = gjson.GetBytes(translated, "system").String()
	}
	if sysPrompt == "" {
		sysPrompt = gjson.GetBytes(payload, "instructions").String()
	}

	params := fmt.Sprintf(`"model":%s,"messages":%s,"max_tokens":%d,"temperature":%f,"stream":%v`,
		ccEncode(model), messages, maxTokens, temperature, stream)

	body := fmt.Sprintf(
		`{"params":{%s},"mode":"tool-desc","config":{"environment":"production","workingDir":"/workspace","date":"%s","structure":[],"isGitRepo":false,"currentBranch":"","mainBranch":"","gitStatus":"","recentCommits":[]},"memory":"","taste":"","skills":""}`,
		params, time.Now().UTC().Format("2006-01-02"))

	if sysPrompt != "" {
		body, _ = sjson.Set(body, "params.system", sysPrompt)
	}

	if cfg := gjson.GetBytes(translated, "reasoning_effort"); cfg.Exists() {
		body, _ = sjson.Set(body, "params.reasoning_effort", cfg.Value())
	}
	if tools := convertToolsForCC(translated); tools != "[]" {
		body, _ = sjson.SetRaw(body, "params.tools", tools)
	}
	if toolChoice := gjson.GetBytes(translated, "tool_choice"); toolChoice.Exists() {
		body, _ = sjson.SetRaw(body, "params.tool_choice", toolChoice.Raw)
	}
	if parallelToolCalls := gjson.GetBytes(translated, "parallel_tool_calls"); parallelToolCalls.Exists() {
		body, _ = sjson.Set(body, "params.parallel_tool_calls", parallelToolCalls.Bool())
	}

	log.Infof("[commandcode] built request body: targetModel=%s srcFormat=%s bodyPreview=%s",
		req.Model, opts.SourceFormat, summary([]byte(body), 400))

	return []byte(body)
}

func summary(s []byte, n int) string {
	if len(s) <= n {
		return string(s)
	}
	return string(s[:n]) + "..."
}

// convertMessagesForCC converts OpenAI-format messages to command-code format:
// - Filters system messages (returns them separately as the second return value)
// - Converts string content to array-of-blocks format
func convertMessagesForCC(translated []byte) (messages, system string) {
	msgs := gjson.GetBytes(translated, "messages")
	if !msgs.Exists() {
		return "[]", ""
	}

	pairedToolCallIDs := commandCodePairedToolCallIDs(msgs)
	var converted []json.RawMessage
	for _, msg := range msgs.Array() {
		role := msg.Get("role").String()
		if role == "" {
			continue
		}
		if role == "system" {
			if content := msg.Get("content"); content.Exists() {
				system = content.String()
			}
			continue
		}
		if role == "tool" {
			toolCallID := msg.Get("tool_call_id").String()
			if toolCallID == "" || !pairedToolCallIDs[toolCallID] {
				continue
			}
			toolName := commandCodeToolNameForID(msgs, toolCallID)
			toolContent := msg.Get("content").String()
			contentJSON := fmt.Sprintf(`[{"type":"tool-result","toolCallId":%s,"toolName":%s,"output":{"type":"text","value":%s}}]`,
				ccEncode(toolCallID), ccEncode(toolName), ccEncode(toolContent))
			converted = append(converted, json.RawMessage(
				fmt.Sprintf(`{"role":"tool","content":%s}`, contentJSON),
			))
			continue
		}
		content := msg.Get("content")
		contentJSON := commandCodeMessageContent(content)
		if role == "assistant" {
			contentJSON = commandCodeAssistantContent(msg, contentJSON, pairedToolCallIDs)
		}
		if contentJSON == "" || contentJSON == "[]" {
			if role == "assistant" {
				continue
			}
			contentJSON = "[]"
		}
		converted = append(converted, json.RawMessage(
			fmt.Sprintf(`{"role":%s,"content":%s}`, ccEncode(role), contentJSON),
		))
	}

	if len(converted) == 0 {
		return "[]", system
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
	return sb.String(), system
}

func commandCodePairedToolCallIDs(msgs gjson.Result) map[string]bool {
	callIDs := make(map[string]bool)
	resultIDs := make(map[string]bool)
	for _, msg := range msgs.Array() {
		switch msg.Get("role").String() {
		case "assistant":
			for _, toolCall := range msg.Get("tool_calls").Array() {
				if id := toolCall.Get("id").String(); id != "" {
					callIDs[id] = true
				}
			}
			for _, part := range msg.Get("content").Array() {
				if part.Get("type").String() == "tool-call" {
					if id := part.Get("toolCallId").String(); id != "" {
						callIDs[id] = true
					}
				}
			}
		case "tool":
			if id := msg.Get("tool_call_id").String(); id != "" {
				resultIDs[id] = true
			}
		}
	}

	paired := make(map[string]bool)
	for id := range callIDs {
		if resultIDs[id] {
			paired[id] = true
		}
	}
	return paired
}

func commandCodeToolNameForID(msgs gjson.Result, toolCallID string) string {
	for _, msg := range msgs.Array() {
		if msg.Get("role").String() != "assistant" {
			continue
		}
		for _, toolCall := range msg.Get("tool_calls").Array() {
			if toolCall.Get("id").String() == toolCallID {
				return toolCall.Get("function.name").String()
			}
		}
		for _, part := range msg.Get("content").Array() {
			if part.Get("type").String() == "tool-call" && part.Get("toolCallId").String() == toolCallID {
				return part.Get("toolName").String()
			}
		}
	}
	return ""
}

func commandCodeMessageContent(content gjson.Result) string {
	if !content.Exists() {
		return "[]"
	}
	if content.Type == gjson.String {
		return fmt.Sprintf(`[{"type":"text","text":%s}]`, ccEncode(content.String()))
	}
	if content.IsArray() {
		return content.Raw
	}
	if content.Raw != "" && content.Raw != "null" {
		return fmt.Sprintf(`[{"type":"text","text":%s}]`, ccEncode(content.String()))
	}
	return "[]"
}

func commandCodeAssistantContent(msg gjson.Result, contentJSON string, pairedToolCallIDs map[string]bool) string {
	var parts []json.RawMessage
	content := msg.Get("content")
	if content.Type == gjson.String && content.String() != "" {
		parts = append(parts, json.RawMessage(fmt.Sprintf(`{"type":"text","text":%s}`, ccEncode(content.String()))))
	} else if content.IsArray() {
		for _, part := range content.Array() {
			if part.Get("type").String() == "tool-call" {
				id := part.Get("toolCallId").String()
				if id == "" || !pairedToolCallIDs[id] {
					continue
				}
				parts = append(parts, json.RawMessage(part.Raw))
				continue
			}
			parts = append(parts, json.RawMessage(part.Raw))
		}
	}

	for _, toolCall := range msg.Get("tool_calls").Array() {
		id := toolCall.Get("id").String()
		if id == "" || !pairedToolCallIDs[id] {
			continue
		}
		name := toolCall.Get("function.name").String()
		args := jsonObjectRaw(toolCall.Get("function.arguments"))
		parts = append(parts, json.RawMessage(
			fmt.Sprintf(`{"type":"tool-call","toolCallId":%s,"toolName":%s,"input":%s}`,
				ccEncode(id), ccEncode(name), args),
		))
	}

	if len(parts) == 0 {
		return contentJSON
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, part := range parts {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.Write(part)
	}
	sb.WriteByte(']')
	return sb.String()
}

func convertToolsForCC(translated []byte) string {
	tools := gjson.GetBytes(translated, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return "[]"
	}

	var converted []json.RawMessage
	for _, tool := range tools.Array() {
		name := tool.Get("function.name").String()
		description := tool.Get("function.description").String()
		inputSchema := tool.Get("function.parameters")
		if name == "" {
			name = tool.Get("name").String()
			description = tool.Get("description").String()
			inputSchema = tool.Get("input_schema")
		}
		if name == "" {
			continue
		}

		item := []byte(`{"type":"function","name":"","description":"","input_schema":{}}`)
		item, _ = sjson.SetBytes(item, "name", name)
		if description != "" {
			item, _ = sjson.SetBytes(item, "description", description)
		} else {
			item, _ = sjson.DeleteBytes(item, "description")
		}
		if inputSchema.Exists() && inputSchema.Raw != "" && inputSchema.Raw != "null" {
			item, _ = sjson.SetRawBytes(item, "input_schema", []byte(inputSchema.Raw))
		}
		converted = append(converted, json.RawMessage(item))
	}

	if len(converted) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, tool := range converted {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.Write(tool)
	}
	sb.WriteByte(']')
	return sb.String()
}

func jsonObjectRaw(value gjson.Result) string {
	if !value.Exists() {
		return "{}"
	}
	if value.IsObject() {
		return value.Raw
	}
	if value.Type == gjson.String {
		raw := strings.TrimSpace(value.String())
		if raw != "" && gjson.Valid(raw) && gjson.Parse(raw).IsObject() {
			return raw
		}
	}
	return "{}"
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
	return []byte(fmt.Sprintf(`data: {"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, ccEncode(text)))
}

func buildCCToolCallChunk(index int, id, name, arguments string) []byte {
	return []byte(fmt.Sprintf(`data: {"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`,
		index, ccEncode(id), ccEncode(name), ccEncode(arguments)))
}

func emitCommandCodeTranslatedStreamChunk(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, to, responseFormat sdktranslator.Format, model string, originalRequest, request, chunk []byte, param *any) {
	chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, model, originalRequest, request, chunk, param)
	for _, c := range chunks {
		select {
		case out <- cliproxyexecutor.StreamChunk{Payload: c}:
		case <-ctx.Done():
			return
		}
	}
}

func buildCCFinishChunk(promptTokens, completionTokens int64, finishReason string) []byte {
	if finishReason == "" {
		finishReason = "stop"
	}
	if promptTokens > 0 || completionTokens > 0 {
		return []byte(fmt.Sprintf(`data: {"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{},"finish_reason":%s}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			ccEncode(finishReason), promptTokens, completionTokens, promptTokens+completionTokens))
	}
	return []byte(fmt.Sprintf(`data: {"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{},"finish_reason":%s}]}`, ccEncode(finishReason)))
}

func commandCodeToolArguments(line []byte) string {
	for _, path := range []string{"input", "args", "arguments"} {
		value := gjson.GetBytes(line, path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.String {
			raw := strings.TrimSpace(value.String())
			if raw != "" {
				return raw
			}
			return "{}"
		}
		if value.Raw != "" && value.Raw != "null" {
			return value.Raw
		}
	}
	return "{}"
}

func mapCommandCodeFinishReason(reason string, sawToolCall bool) string {
	switch strings.TrimSpace(reason) {
	case "tool-calls", "tool_calls", "toolUse":
		return "tool_calls"
	case "length", "max_tokens", "max-tokens", "max_output_tokens":
		return "length"
	case "content_filter":
		return "content_filter"
	}
	if sawToolCall {
		return "tool_calls"
	}
	return "stop"
}
