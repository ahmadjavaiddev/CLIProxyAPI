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
            },
            "rate_limit_reset_credits":{"available_count":2}
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
		PlanType     string                     `json:"plan_type"`
		ResetCredits int                        `json:"reset_credits"`
		Windows      []codexQuotaWindowResponse `json:"windows"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.PlanType != "plus" {
		t.Fatalf("plan_type = %q, want plus", response.PlanType)
	}
	if response.ResetCredits != 2 {
		t.Fatalf("reset_credits = %d, want 2", response.ResetCredits)
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

func TestConsumeCodexQuotaResetRedeemsAvailableCredit(t *testing.T) {
	var consumeCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "account-123" {
			t.Errorf("Chatgpt-Account-Id = %q, want account-123", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/credits":
			_, _ = w.Write([]byte(`{"available_count":2,"credits":[{"id":"reset-credit-1","status":"available"}]}`))
		case "/consume":
			consumeCalls++
			var body struct {
				CreditID        string `json:"credit_id"`
				RedeemRequestID string `json:"redeem_request_id"`
			}
			if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
				t.Errorf("decode consume body: %v", errDecode)
			}
			if body.CreditID != "reset-credit-1" || body.RedeemRequestID == "" {
				t.Errorf("consume body = %+v", body)
			}
			_, _ = w.Write([]byte(`{"code":"reset","windows_reset":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	previousCreditsURL := codexQuotaResetCreditsURL
	previousConsumeURL := codexQuotaResetConsumeURL
	codexQuotaResetCreditsURL = upstream.URL + "/credits"
	codexQuotaResetConsumeURL = upstream.URL + "/consume"
	defer func() {
		codexQuotaResetCreditsURL = previousCreditsURL
		codexQuotaResetConsumeURL = previousConsumeURL
	}()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-reset-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "test-access-token",
			"account_id":   "account-123",
		},
		Quota: coreauth.QuotaState{Exceeded: true, Reason: "quota"},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/reset-quota", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.ConsumeCodexQuotaReset(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if consumeCalls != 1 {
		t.Fatalf("consume calls = %d, want 1", consumeCalls)
	}
	var response struct {
		WindowsReset     int `json:"windows_reset"`
		RemainingCredits int `json:"remaining_credits"`
	}
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if response.WindowsReset != 2 || response.RemainingCredits != 1 {
		t.Fatalf("response = %+v", response)
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.Quota.Exceeded {
		t.Fatalf("local quota was not cleared: %+v", updated)
	}
}
