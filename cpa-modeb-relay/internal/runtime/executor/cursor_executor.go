package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CursorExecutor executes chat completions through Cursor's Agent Connect protocol.
type CursorExecutor struct {
	cfg *config.Config
	svc *cursorauth.AuthService
}

// NewCursorExecutor creates a Cursor executor.
func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	registerCursorStickyHint()
	return &CursorExecutor{cfg: cfg, svc: cursorauth.NewAuthService()}
}

var cursorStickyHintOnce sync.Once

// registerCursorStickyHint pins requests that carry trailing tool results to
// the auth whose live Cursor session is waiting for those results. Without the
// pin the selector may rotate to another account mid-conversation, which both
// misattributes usage and throws away the provider prompt cache.
func registerCursorStickyHint() {
	cursorStickyHintOnce.Do(func() {
		cliproxyauth.RegisterStickyAuthHint(func(_ http.Header, payload []byte) (string, bool) {
			for _, id := range trailingToolCallIDs(payload) {
				if session := cursorlib.DefaultSessionManager().LookupPending(id); session != nil && strings.TrimSpace(session.AuthID) != "" {
					return session.AuthID, true
				}
			}
			return "", false
		})
	})
}

// trailingToolCallIDs extracts the tool call ids of the trailing tool-result
// rows of an OpenAI chat payload (role:"tool") or a Claude messages payload
// (user content blocks of type "tool_result").
func trailingToolCallIDs(payload []byte) []string {
	arr := gjson.GetBytes(payload, "messages")
	if !arr.IsArray() {
		return nil
	}
	items := arr.Array()
	var ids []string
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		switch item.Get("role").String() {
		case "tool":
			if id := item.Get("tool_call_id").String(); id != "" {
				ids = append(ids, id)
				continue
			}
		case "user":
			content := item.Get("content")
			found := false
			if content.IsArray() {
				for _, part := range content.Array() {
					if part.Get("type").String() == "tool_result" {
						if id := part.Get("tool_use_id").String(); id != "" {
							ids = append(ids, id)
							found = true
						}
					}
				}
			}
			if found {
				continue
			}
		}
		break
	}
	return ids
}

// Identifier returns the executor identifier.
func (e *CursorExecutor) Identifier() string { return "cursor" }

// Execute performs a non-streaming Cursor Agent chat completion.
func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), false)
	_ = originalPayloadSource

	// Keep hyphen variant suffixes (-thinking/-xhigh/…) for Cursor resolution.
	// Public wire model id is the base name; parameters ride on RequestedModel.
	upstreamModel := strings.TrimPrefix(baseModel, "cursor-")
	if upstreamModel == "" {
		upstreamModel = "default"
	}
	resolved := cursorlib.ResolveRequestedModel(upstreamModel)
	body, _ = sjson.SetBytes(body, "model", resolved.ModelID)

	messages, err := extractChatMessages(body)
	if err != nil {
		return resp, err
	}
	tools := extractTools(body)
	sessOpts := cursorlib.SessionOptions{AuthID: authID(auth), ToolChoice: extractToolChoice(body)}

	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return resp, err
	}

	result, err := cursorlib.RunChat(ctx, creds, upstreamModel, messages, tools, sessOpts)
	if err != nil {
		return resp, err
	}
	if sessOpts.ToolChoice.ForcesToolCall() && len(result.ToolCalls) == 0 {
		log.Warnf("cursor: run completed without satisfying tool_choice %s; returning text response", toolChoiceLabel(sessOpts.ToolChoice))
	}

	outPayload := buildOpenAIChatCompletion(req.Model, result)
	reporter.Publish(ctx, usage.Detail{
		InputTokens:     result.InputTokens,
		OutputTokens:    result.OutputTokens,
		CachedTokens:    result.CacheReadTokens,
		CacheReadTokens: result.CacheReadTokens,
		ReasoningTokens: result.ReasoningTokens,
		TotalTokens:     result.InputTokens + result.OutputTokens,
	})

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, outPayload, &param)
	resp = cliproxyexecutor.Response{Payload: out}
	return resp, nil
}

// ExecuteStream streams OpenAI-compatible SSE chunks from Cursor Agent events.
func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	upstreamModel := strings.TrimPrefix(baseModel, "cursor-")
	if upstreamModel == "" {
		upstreamModel = "default"
	}
	resolved := cursorlib.ResolveRequestedModel(upstreamModel)
	body, _ = sjson.SetBytes(body, "model", resolved.ModelID)

	messages, err := extractChatMessages(body)
	if err != nil {
		return nil, err
	}
	tools := extractTools(body)
	sessOpts := cursorlib.SessionOptions{AuthID: authID(auth), ToolChoice: extractToolChoice(body)}

	creds, err := e.ensureCredentials(ctx, auth)
	if err != nil {
		return nil, err
	}

	session, resumedLive, err := openCursorSession(ctx, creds, upstreamModel, messages, tools, sessOpts)
	if err != nil {
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		completionID := "chatcmpl-" + uuid.NewString()
		created := time.Now().Unix()
		var param any
		toolIndex := 0
		finishReason := "stop"
		var usageFinal cursorlib.StreamEvent

		emitLine := func(line []byte) bool {
			chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, line, &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					_ = session.Close()
					return false
				}
			}
			return true
		}

		roleChunk, _ := json.Marshal(map[string]any{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   req.Model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil},
			},
		})
		if !emitLine([]byte("data: " + string(roleChunk))) {
			return
		}

		contentEmitted := false
		iterate := func() error {
			return session.IterSegment(ctx, func(ev cursorlib.StreamEvent) error {
				var delta map[string]any
				switch ev.Type {
				case "text_delta":
					contentEmitted = true
					delta = map[string]any{"content": ev.Text}
				case "thinking_delta":
					contentEmitted = true
					delta = map[string]any{"reasoning_content": ev.Text}
				case "tool_call":
					if ev.ToolCall == nil {
						return nil
					}
					contentEmitted = true
					args, _ := json.Marshal(ev.ToolCall.Arguments)
					if args == nil {
						args = []byte("{}")
					}
					delta = map[string]any{
						"tool_calls": []map[string]any{
							{
								"index": toolIndex,
								"id":    ev.ToolCall.ID,
								"type":  "function",
								"function": map[string]any{
									"name":      ev.ToolCall.Name,
									"arguments": string(args),
								},
							},
						},
					}
					toolIndex++
				case "usage_final":
					usageFinal = ev
					reporter.Publish(ctx, usage.Detail{
						InputTokens:     ev.InputTokens,
						OutputTokens:    ev.OutputTokens,
						CachedTokens:    ev.CacheReadTokens,
						CacheReadTokens: ev.CacheReadTokens,
						ReasoningTokens: ev.ReasoningTokens,
						TotalTokens:     ev.InputTokens + ev.OutputTokens,
					})
					return nil
				case "error":
					return fmt.Errorf("%s", ev.Message)
				case "segment_end":
					if ev.Reason != "" {
						finishReason = ev.Reason
					}
					return nil
				default:
					return nil
				}
				if delta == nil {
					return nil
				}
				chunk, _ := json.Marshal(map[string]any{
					"id":      completionID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   req.Model,
					"choices": []map[string]any{
						{"index": 0, "delta": delta, "finish_reason": nil},
					},
				})
				if !emitLine([]byte("data: " + string(chunk))) {
					return context.Canceled
				}
				return nil
			})
		}
		errIter := iterate()
		if errIter != nil && resumedLive && !contentEmitted && ctx.Err() == nil {
			// The live run died before producing any output for the resumed
			// segment (idle upstream teardown while waiting on tool results).
			// Rebuild a fresh run carrying the tool results in history.
			log.Warnf("cursor: resumed stream failed before output (%v); rebuilding run from history", errIter)
			if rebuilt, errRebuild := cursorlib.StartSession(ctx, creds, upstreamModel, messages, tools, sessOpts); errRebuild == nil {
				session = rebuilt
				finishReason = "stop"
				errIter = iterate()
			}
		}
		if errIter != nil {
			reporter.PublishFailure(ctx, errIter)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errIter}:
			case <-ctx.Done():
			}
			_ = session.Close()
			return
		}

		if finishReason == "tool_calls" && toolIndex == 0 {
			finishReason = "stop"
		}
		if finishReason != "tool_calls" {
			_ = session.Close()
		}
		if sessOpts.ToolChoice.ForcesToolCall() && toolIndex == 0 {
			log.Warnf("cursor: stream completed without satisfying tool_choice %s; returning text response", toolChoiceLabel(sessOpts.ToolChoice))
		}

		endChunk, _ := json.Marshal(map[string]any{
			"id":      completionID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   req.Model,
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason},
			},
		})
		if !emitLine([]byte("data: " + string(endChunk))) {
			return
		}
		if usageFinal.Type == "usage_final" {
			usageChunk, _ := json.Marshal(map[string]any{
				"id":      completionID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   req.Model,
				"choices": []map[string]any{},
				"usage":   openAIUsagePayload(usageFinal.InputTokens, usageFinal.OutputTokens, usageFinal.CacheReadTokens, usageFinal.ReasoningTokens),
			})
			if !emitLine([]byte("data: " + string(usageChunk))) {
				return
			}
		}
		doneChunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param)
		for i := range doneChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

// openCursorSession returns a live session for the request. The second return
// reports whether the session is a resumed live run (tool results were fed to
// the original bidi stream), which makes an early failure eligible for a
// rebuild from history.
func openCursorSession(ctx context.Context, creds cursorlib.AccountCredentials, model string, messages []cursorlib.ChatMessage, tools []cursorlib.ToolDefinition, opts cursorlib.SessionOptions) (*cursorlib.Session, bool, error) {
	results := cursorlib.TrailingToolResults(messages)
	if len(results) > 0 {
		session, err := cursorlib.ResumeSession(results)
		if err == nil {
			return session, true, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		log.Warnf("cursor: resuming tool results failed (%v); rebuilding run from history", err)
	}
	session, err := cursorlib.StartSession(ctx, creds, model, messages, tools, opts)
	return session, false, err
}

func authID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return auth.ID
}

// extractToolChoice parses the OpenAI tool_choice field (string or object
// form, plus the Claude-style {"type":"tool","name":…} passthrough).
func extractToolChoice(body []byte) cursorlib.ToolChoice {
	tc := gjson.GetBytes(body, "tool_choice")
	if !tc.Exists() {
		return cursorlib.ToolChoice{}
	}
	if tc.Type == gjson.String {
		switch strings.ToLower(strings.TrimSpace(tc.String())) {
		case "none":
			return cursorlib.ToolChoice{Mode: "none"}
		case "required", "any":
			return cursorlib.ToolChoice{Mode: "required"}
		case "auto", "":
			return cursorlib.ToolChoice{Mode: "auto"}
		}
		return cursorlib.ToolChoice{}
	}
	if tc.IsObject() {
		switch strings.ToLower(strings.TrimSpace(tc.Get("type").String())) {
		case "function":
			if name := strings.TrimSpace(tc.Get("function.name").String()); name != "" {
				return cursorlib.ToolChoice{Mode: "function", FunctionName: name}
			}
		case "tool":
			if name := strings.TrimSpace(tc.Get("name").String()); name != "" {
				return cursorlib.ToolChoice{Mode: "function", FunctionName: name}
			}
		case "any", "required":
			return cursorlib.ToolChoice{Mode: "required"}
		case "none":
			return cursorlib.ToolChoice{Mode: "none"}
		case "auto":
			return cursorlib.ToolChoice{Mode: "auto"}
		}
	}
	return cursorlib.ToolChoice{}
}

func toolChoiceLabel(choice cursorlib.ToolChoice) string {
	if choice.Mode == "function" {
		return fmt.Sprintf("function %q", choice.FunctionName)
	}
	return fmt.Sprintf("%q", choice.Mode)
}

func openAIUsagePayload(input, output, cacheRead, reasoning int64) map[string]any {
	return map[string]any{
		"prompt_tokens":     input,
		"completion_tokens": output,
		"total_tokens":      input + output,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cacheRead,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": reasoning,
		},
	}
}

// CountTokens returns a best-effort character-based estimate.
func (e *CursorExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = auth
	_ = opts
	chars := len(req.Payload)
	tokens := chars / 4
	if tokens < 1 {
		tokens = 1
	}
	payload := fmt.Appendf(nil, `{"input_tokens":%d}`, tokens)
	return cliproxyexecutor.Response{Payload: payload}, nil
}

// HttpRequest is unused for Cursor Agent protocol.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	_ = ctx
	_ = auth
	_ = req
	return nil, fmt.Errorf("cursor executor: HttpRequest is not supported for Agent Connect")
}

func (e *CursorExecutor) ensureCredentials(ctx context.Context, auth *cliproxyauth.Auth) (cursorlib.AccountCredentials, error) {
	creds := cursorlib.CredentialsFromMetadata(authMetadata(auth))
	if creds.AccessToken == "" {
		return creds, fmt.Errorf("cursor: auth missing access_token")
	}
	storage := &cursorauth.TokenStorage{
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		Expired:      stringFromMeta(authMetadata(auth), "expired"),
	}
	if !storage.NeedsRefresh() {
		return creds, nil
	}
	refreshed, err := e.svc.RefreshToken(ctx, creds.RefreshToken, creds.AuthClientID, creds.BaseURL)
	if err != nil {
		log.Warnf("cursor token refresh failed, using existing token: %v", err)
		return creds, nil
	}
	creds.AccessToken = refreshed.AccessToken
	creds.RefreshToken = refreshed.RefreshToken
	if auth != nil && auth.Metadata != nil {
		auth.Metadata["access_token"] = refreshed.AccessToken
		auth.Metadata["refresh_token"] = refreshed.RefreshToken
		if !refreshed.ExpiresAt.IsZero() {
			auth.Metadata["expired"] = refreshed.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	return creds, nil
}

func authMetadata(auth *cliproxyauth.Auth) map[string]any {
	if auth == nil {
		return nil
	}
	return auth.Metadata
}

func stringFromMeta(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func extractChatMessages(body []byte) ([]cursorlib.ChatMessage, error) {
	arr := gjson.GetBytes(body, "messages")
	if !arr.IsArray() || len(arr.Array()) == 0 {
		return nil, fmt.Errorf("cursor: request messages are required")
	}
	out := make([]cursorlib.ChatMessage, 0, len(arr.Array()))
	for _, item := range arr.Array() {
		role := item.Get("role").String()
		content := item.Get("content")
		text := ""
		if content.IsArray() {
			var parts []string
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" || part.Get("text").Exists() {
					parts = append(parts, part.Get("text").String())
				}
			}
			text = strings.Join(parts, "\n")
		} else {
			text = content.String()
		}
		if role == "" {
			continue
		}
		msg := cursorlib.ChatMessage{
			Role:       role,
			Content:    text,
			Name:       item.Get("name").String(),
			ToolCallID: item.Get("tool_call_id").String(),
		}
		if toolCalls := item.Get("tool_calls"); toolCalls.IsArray() {
			for _, tc := range toolCalls.Array() {
				argsRaw := tc.Get("function.arguments").String()
				var args map[string]any
				if argsRaw != "" {
					_ = json.Unmarshal([]byte(argsRaw), &args)
				}
				if args == nil {
					args = map[string]any{}
				}
				msg.ToolCalls = append(msg.ToolCalls, cursorlib.ToolCall{
					ID:        tc.Get("id").String(),
					Name:      tc.Get("function.name").String(),
					Arguments: args,
				})
			}
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cursor: no usable chat messages")
	}
	return out, nil
}

func extractTools(body []byte) []cursorlib.ToolDefinition {
	arr := gjson.GetBytes(body, "tools")
	if !arr.IsArray() {
		return nil
	}
	out := make([]cursorlib.ToolDefinition, 0, len(arr.Array()))
	for _, item := range arr.Array() {
		typ := item.Get("type").String()
		if typ != "" && typ != "function" {
			continue
		}
		fn := item.Get("function")
		name := fn.Get("name").String()
		if name == "" {
			name = item.Get("name").String()
		}
		if name == "" {
			continue
		}
		var params map[string]any
		paramsRaw := fn.Get("parameters").Raw
		if paramsRaw == "" {
			paramsRaw = item.Get("parameters").Raw
		}
		if paramsRaw != "" {
			_ = json.Unmarshal([]byte(paramsRaw), &params)
		}
		desc := fn.Get("description").String()
		if desc == "" {
			desc = item.Get("description").String()
		}
		out = append(out, cursorlib.ToolDefinition{
			Name:        name,
			Description: desc,
			Parameters:  params,
		})
	}
	return out
}

func buildOpenAIChatCompletion(model string, result *cursorlib.ChatResult) []byte {
	id := "chatcmpl-" + uuid.NewString()
	finish := result.FinishReason
	if finish == "" {
		finish = "stop"
	}
	message := map[string]any{
		"role":    "assistant",
		"content": result.Text,
	}
	if result.Thinking != "" {
		message["reasoning_content"] = result.Thinking
	}
	if len(result.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(result.ToolCalls))
		for _, tc := range result.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			if args == nil {
				args = []byte("{}")
			}
			calls = append(calls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(args),
				},
			})
		}
		message["tool_calls"] = calls
		if finish == "stop" {
			finish = "tool_calls"
		}
	}
	payload := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
		"usage": openAIUsagePayload(result.InputTokens, result.OutputTokens, result.CacheReadTokens, result.ReasoningTokens),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":"marshal failed"}`)
	}
	return b
}
