package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGetCodexQuotaReturnsSanitizedWindows(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "account-123" {
			t.Errorf("Chatgpt-Account-Id = %q, want account-123", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "plan_type":"plus",
            "email":"must-not-leak@example.com",
            "rate_limit":{
                "allowed":true,
                "limit_reached":false,
                "primary_window":{
                    "used_percent":42.5,
                    "limit_window_seconds":604800,
                    "reset_at":1893456000
                }
            }
        }`))
	}))
	defer upstream.Close()

	previousURL := codexQuotaUsageURL
	codexQuotaUsageURL = upstream.URL
	defer func() { codexQuotaUsageURL = previousURL }()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-quota-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "test-access-token",
			"account_id":   "account-123",
		},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/quota?auth_index="+authIndex, nil)
	handler.GetCodexQuota(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var response struct {
		PlanType string                     `json:"plan_type"`
		Windows  []codexQuotaWindowResponse `json:"windows"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.PlanType != "plus" {
		t.Fatalf("plan_type = %q, want plus", response.PlanType)
	}
	if len(response.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(response.Windows))
	}
	if response.Windows[0].Label != "Weekly limit" || response.Windows[0].UsedPercent != 42.5 {
		t.Fatalf("window = %+v, want weekly 42.5%%", response.Windows[0])
	}
	if strings.Contains(recorder.Body.String(), "must-not-leak@example.com") {
		t.Fatal("upstream email leaked into response")
	}
}
