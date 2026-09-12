package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/client/commandcode"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CommandCodeExecutor is the Command Code provider binding. The common
// executor machinery remains shared with the messages based providers while
// Command Code credentials and routing are identified independently.
type CommandCodeExecutor struct { ClaudeExecutor; client *http.Client }

func NewCommandCodeExecutor(cfg *config.Config) *CommandCodeExecutor {
	return &CommandCodeExecutor{ClaudeExecutor: ClaudeExecutor{cfg: cfg, requestLogProvider: "commandcode", upstreamModelNormalizer: func(model string) string { return strings.TrimPrefix(model, "commandcode/") }}, client: http.DefaultClient}
}
func (*CommandCodeExecutor) Identifier() string { return "commandcode" }
func (e *CommandCodeExecutor) Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error) { return nil, nil }
func (e *CommandCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) { return e.Execute(ctx, auth, req, opts) }
func (e *CommandCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) { return e.ClaudeExecutor.HttpRequest(ctx, auth, req) }

func (e *CommandCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	key := ""; base := commandcode.DefaultBaseURL; version := commandcode.DefaultCLIVersion
	if auth != nil { key = auth.Attributes[cliproxyauth.AttributeAPIKey]; if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" { base = v }; if v := strings.TrimSpace(auth.Attributes["cli_version"]); v != "" { version = v } }
	if key == "" { return nil, fmt.Errorf("commandcode: api key is missing") }
	model := commandcode.ResolveModel(strings.TrimPrefix(req.Model, "commandcode/"))
	var input map[string]any
	if err := json.Unmarshal(req.Payload, &input); err != nil { return nil, fmt.Errorf("commandcode: invalid request: %w", err) }
	body := map[string]any{"config": map[string]any{"workingDir": ""}, "memory":"", "taste":"", "skills":"", "permissionMode":"standard", "threadId": "cc-thread", "params": map[string]any{"model": model, "stream": true}}
	params := body["params"].(map[string]any)
	for _, k := range []string{"messages", "system", "tools", "tool_choice", "max_tokens", "temperature", "top_p", "stop"} { if v, ok := input[k]; ok { params[k] = v } }
	if v, ok := input["reasoning_effort"].(string); ok { if clipped := commandcode.ResolveEffort(model, v); clipped != "" { params["reasoning_effort"] = clipped } }
	if v, ok := input["model"].(string); ok && model == "" { params["model"] = commandcode.ResolveModel(v) }
	data, _ := json.Marshal(body)
	resp, err := commandcode.Do(ctx, e.client, base, key, version, body["threadId"].(string), "", bytes.NewReader(data)); if err != nil { return nil, err }
	out := make(chan cliproxyexecutor.StreamChunk, 8)
	go func() { defer resp.Body.Close(); defer close(out); scanner := bufio.NewScanner(resp.Body); scanner.Buffer(make([]byte, 4096), 4<<20); id := "cc-response"; for scanner.Scan() { event, ok := commandcode.ParseLine(scanner.Text()); if !ok { continue }; var d map[string]any; _ = json.Unmarshal(event.Data, &d); var payload []byte; switch event.Type { case "text-delta": payload = ccChatChunk(id, req.Model, map[string]any{"content": stringValue(d["text"])}); case "reasoning-delta": payload = ccChatChunk(id, req.Model, map[string]any{"reasoning_content": stringValue(d["text"])}); case "finish": payload = ccChatChunk(id, req.Model, map[string]any{"finish_reason": "stop"}) }; if len(payload)>0 { select { case out <- cliproxyexecutor.StreamChunk{Payload: payload}: case <-ctx.Done(): return } } }; if err := scanner.Err(); err != nil { select { case out <- cliproxyexecutor.StreamChunk{Err: err}: case <-ctx.Done(): } } }()
	return &cliproxyexecutor.StreamResult{Headers: resp.Header.Clone(), Chunks: out}, nil
}

func stringValue(v any) string { s, _ := v.(string); return s }
func ccChatChunk(id, model string, delta map[string]any) []byte { b, _ := json.Marshal(map[string]any{"id":id,"object":"chat.completion.chunk","created":0,"model":model,"choices":[]any{map[string]any{"index":0,"delta":delta,"finish_reason":delta["finish_reason"]}}}); return append(b, '\n') }

func (e *CommandCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) { stream, err := e.ExecuteStream(ctx, auth, req, opts); if err != nil { return cliproxyexecutor.Response{}, err }; var all []byte; for chunk := range stream.Chunks { if chunk.Err != nil { return cliproxyexecutor.Response{}, chunk.Err }; all = append(all, chunk.Payload...) }; return cliproxyexecutor.Response{Payload: bytes.TrimSpace(all)}, nil }
