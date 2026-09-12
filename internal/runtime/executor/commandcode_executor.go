package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CommandCodeExecutor is the Command Code provider binding. The common
// executor machinery remains shared with the messages based providers while
// Command Code credentials and routing are identified independently.
type CommandCodeExecutor struct { ClaudeExecutor }

func NewCommandCodeExecutor(cfg *config.Config) *CommandCodeExecutor {
	return &CommandCodeExecutor{ClaudeExecutor: ClaudeExecutor{cfg: cfg, requestLogProvider: "commandcode", upstreamModelNormalizer: func(model string) string { return strings.TrimPrefix(model, "commandcode/") }}}
}
func (*CommandCodeExecutor) Identifier() string { return "commandcode" }
func (e *CommandCodeExecutor) Refresh(context.Context, *cliproxyauth.Auth) (*cliproxyauth.Auth, error) { return nil, nil }
func (e *CommandCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) { return e.Execute(ctx, auth, req, opts) }
func (e *CommandCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) { return e.ClaudeExecutor.HttpRequest(ctx, auth, req) }
