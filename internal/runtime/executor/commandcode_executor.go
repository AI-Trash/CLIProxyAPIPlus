package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
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
	ccHeaderProdEnv           = "production"
	ccHeaderVersion           = "0.40.3"
	ccDefaultProjectSlug      = "workspace"
	ccDefaultNodeVersion      = "v22.11.0"
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
	e.injectHeaders(httpReq, auth, false)
	httpClient := newUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *CommandCodeExecutor) injectHeaders(req *http.Request, auth *cliproxyauth.Auth, stream bool) {
	apiKey := e.resolveAPIKey(auth)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// undici (Node's fetch) auto-adds these lowercase default headers to every
	// request. Go's transport does not send them, so add them explicitly.
	// Under HTTP/2 (used by the uTLS transport), all headers are sent lowercase
	// automatically, but we set them lowercase here for HTTP/1.1 fallback too.
	ccSetLowerHeader(req, "accept", "*/*")
	ccSetLowerHeader(req, "accept-language", "*")

	// Accept-Encoding: streams must use "identity" because the line scanner
	// cannot parse compressed bytes. Non-stream requests send the same value
	// as undici's default to match the CLI fingerprint.
	if stream {
		ccSetLowerHeader(req, "accept-encoding", "identity")
	} else {
		ccSetLowerHeader(req, "accept-encoding", "gzip, deflate, br")
	}

	// CLI fingerprint headers — the official CLI sets these in lowercase.
	ccSetLowerHeader(req, "x-cli-environment", ccHeaderProdEnv)
	ccSetLowerHeader(req, "x-command-code-version", ccHeaderVersion)
	ccSetLowerHeader(req, "x-session-id", uuid.New().String())
	ccSetLowerHeader(req, "x-project-slug", e.resolveString(auth, "project_slug", ccDefaultProjectSlug))
	ccSetLowerHeader(req, "x-taste-learning", e.resolveString(auth, "taste_learning", "true"))
	ccSetLowerHeader(req, "x-co-flag", "false")
	if tp := ccGenerateTraceparent(); tp != "" {
		ccSetLowerHeader(req, "traceparent", tp)
	}

	// The official CLI uses Node's fetch (undici) which does not send a
	// User-Agent header. Suppress Go's default "Go-http-client/1.1" by setting
	// the key to a nil slice — Go's transport sees the key exists and skips its
	// default, while WriteSubset iterates zero values so nothing is emitted.
	req.Header["User-Agent"] = nil

	if auth != nil && auth.Metadata != nil {
		if tok, ok := auth.Metadata["oauth_token"].(string); ok && tok != "" {
			ccSetLowerHeader(req, "x-oauth-token", "Bearer "+tok)
		}
		if prov, ok := auth.Metadata["oauth_provider"].(string); ok && prov != "" {
			ccSetLowerHeader(req, "x-oauth-provider", prov)
		}
	}
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}
}

// ccSetLowerHeader sets a header using exact (lowercase) key casing, bypassing
// Go's automatic canonicalization to Title-Case. This is necessary because the
// command-code CLI sends headers in lowercase (via Node's fetch/undici), and
// the difference is detectable over HTTP/1.1.
func ccSetLowerHeader(req *http.Request, key, value string) {
	// Delete any canonicalized variant first to avoid duplicate keys.
	req.Header.Del(key)
	req.Header[key] = []string{value}
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

// resolveString resolves a string value from auth attributes/metadata with a
// fallback default. Attributes take priority over metadata.
func (e *CommandCodeExecutor) resolveString(auth *cliproxyauth.Auth, key, fallback string) string {
	if auth != nil {
		if auth.Attributes != nil {
			if v := strings.TrimSpace(auth.Attributes[key]); v != "" {
				return v
			}
		}
		if auth.Metadata != nil {
			if v, ok := auth.Metadata[key].(string); ok {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
		}
	}
	return fallback
}

// ccEnvironmentString builds the "config.environment" value the official CLI
// sends: "<platform>-<arch>, Node.js <version>". This is distinct from the
// x-cli-environment header (which is "production"). The value is overridable
// via auth metadata/attributes "environment".
func (e *CommandCodeExecutor) ccEnvironmentString(auth *cliproxyauth.Auth) string {
	if env := e.resolveString(auth, "environment", ""); env != "" {
		return env
	}
	nodeVer := e.resolveString(auth, "node_version", ccDefaultNodeVersion)
	return fmt.Sprintf("%s-%s, Node.js %s", ccNodePlatform(), ccNodeArch(), nodeVer)
}

func ccNodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

func ccNodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "ia32"
	default:
		return runtime.GOARCH
	}
}

// ccGenerateTraceparent generates a valid W3C Trace Context "traceparent"
// header value (00-<32hex traceId>-<16hex spanId>-01) that the official CLI's
// OpenTelemetry instrumentation attaches to every request.
func ccGenerateTraceparent() string {
	var traceBuf [16]byte
	var spanBuf [8]byte
	if _, err := rand.Read(traceBuf[:]); err != nil {
		return ""
	}
	if _, err := rand.Read(spanBuf[:]); err != nil {
		return ""
	}
	// traceId and spanId must not be all-zero per W3C spec.
	allZero := func(b []byte) bool {
		for _, v := range b {
			if v != 0 {
				return false
			}
		}
		return true
	}
	if allZero(traceBuf[:]) || allZero(spanBuf[:]) {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-01", hex.EncodeToString(traceBuf[:]), hex.EncodeToString(spanBuf[:]))
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

	ccBody := e.buildRequestBody(req, opts, false, auth)

	httpReq, err := e.buildHTTPRequest(ctx, baseURL, "/alpha/generate", apiKey, ccBody, auth, false)
	if err != nil {
		return resp, err
	}

	httpClient := newUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer httpResp.Body.Close()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	// Decompress the response body if the upstream sent compressed content
	// (we requested gzip/deflate/br for non-stream requests to match undici).
	decompressedBody, errDecomp := ccDecompressBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if errDecomp != nil {
		recordAPIResponseError(ctx, e.cfg, errDecomp)
		err = errDecomp
		return
	}
	httpResp.Body = io.NopCloser(bytes.NewReader(decompressedBody))
	httpResp.Header.Del("Content-Encoding")
	httpResp.Header.Del("Content-Length")

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

	ccBody := e.buildRequestBody(req, opts, true, auth)

	httpReq, err := e.buildHTTPRequest(ctx, baseURL, "/alpha/generate", apiKey, ccBody, auth, true)
	if err != nil {
		return nil, err
	}

	httpClient := newUtlsHTTPClient(ctx, e.cfg, auth, 0)
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
			cachedTokens     int64
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
						cachedTokens = cached.Int()
					}
					detail.TotalTokens = promptTokens + completionTokens
					if detail.TotalTokens == 0 {
						detail.TotalTokens = usageNode.Get("totalTokens").Int()
					}
					reporter.publish(ctx, detail)
				}
				finishReason := mapCommandCodeFinishReason(gjson.GetBytes(line, "finishReason").String(), sawToolCall)
				chunk := buildCCFinishChunk(promptTokens, completionTokens, cachedTokens, finishReason)
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
			emitCommandCodeTranslatedStreamChunk(ctx, out, to, responseFormat, req.Model, opts.OriginalRequest, translated, buildCCFinishChunk(promptTokens, completionTokens, cachedTokens, mapCommandCodeFinishReason("", sawToolCall)), &param)
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

func (e *CommandCodeExecutor) buildHTTPRequest(ctx context.Context, baseURL, endpoint, apiKey string, body []byte, auth *cliproxyauth.Auth, stream bool) (*http.Request, error) {
	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	e.injectHeaders(httpReq, auth, stream)

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

func (e *CommandCodeExecutor) buildRequestBody(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool, auth *cliproxyauth.Auth) []byte {
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

	// System prompt from translation takes priority; fall back to instructions
	if sysPrompt == "" {
		sysPrompt = gjson.GetBytes(translated, "system").String()
	}
	if sysPrompt == "" {
		sysPrompt = gjson.GetBytes(payload, "instructions").String()
	}

	// Params mirror the official CLI's callServerAPI: model, messages, tools,
	// system, max_tokens, stream. Note: temperature is intentionally omitted
	// (the main agent flow does not send it; only the legacy callApi path does).
	params := fmt.Sprintf(`"model":%s,"messages":%s,"tools":%s,"max_tokens":%d,"stream":%v`,
		ccEncode(model), messages, convertToolsForCC(translated), maxTokens, stream)

	// Body shape mirrors the official command-code CLI's callServerAPI request:
	// no "mode" field for regular chat, permissionMode present, and
	// config.environment is the OS/Node.js info string (NOT "production", which
	// is the x-cli-environment header value instead). memory/taste/skills use
	// JSON null (not empty string) when absent, matching the `??null` pattern.
	envInfo := e.ccEnvironmentString(auth)
	body := fmt.Sprintf(
		`{"params":{%s},"permissionMode":"standard","config":{"environment":%s,"workingDir":"/workspace","date":"%s","structure":[],"isGitRepo":false,"currentBranch":"","mainBranch":"","gitStatus":"","recentCommits":[]},"memory":null,"taste":null,"skills":null}`,
		params, ccEncode(envInfo), time.Now().UTC().Format("2006-01-02"))

	if sysPrompt != "" {
		body, _ = sjson.Set(body, "params.system", sysPrompt)
	}

	if cfg := gjson.GetBytes(translated, "reasoning_effort"); cfg.Exists() {
		body, _ = sjson.Set(body, "params.reasoning_effort", cfg.Value())
	}
	if toolChoice := gjson.GetBytes(translated, "tool_choice"); toolChoice.Exists() && toolChoice.IsObject() {
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

// ccDecompressBody decompresses an HTTP response body based on the
// Content-Encoding header. Supports gzip, deflate, and brotli (br).
// If encoding is empty or "identity", the body is returned as-is.
func ccDecompressBody(r io.Reader, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return io.ReadAll(r)
	case "gzip":
		gz, errGz := gzip.NewReader(r)
		if errGz != nil {
			return nil, fmt.Errorf("commandcode: gzip decompress init failed: %w", errGz)
		}
		defer gz.Close()
		return io.ReadAll(gz)
	case "deflate":
		fl := flate.NewReader(r)
		defer fl.Close()
		return io.ReadAll(fl)
	case "br":
		return io.ReadAll(brotli.NewReader(r))
	default:
		return io.ReadAll(r)
	}
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

func buildCCFinishChunk(promptTokens, completionTokens, cachedTokens int64, finishReason string) []byte {
	if finishReason == "" {
		finishReason = "stop"
	}
	if promptTokens > 0 || completionTokens > 0 || cachedTokens > 0 {
		return []byte(fmt.Sprintf(`data: {"id":"chatcmpl-cc","object":"chat.completion.chunk","created":0,"model":"commandcode","choices":[{"index":0,"delta":{},"finish_reason":%s}],"usage":{"prompt_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d},"completion_tokens":%d,"total_tokens":%d}}`,
			ccEncode(finishReason), promptTokens, cachedTokens, completionTokens, promptTokens+completionTokens))
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
