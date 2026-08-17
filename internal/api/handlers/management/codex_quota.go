package management

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

var codexQuotaUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type codexQuotaWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAfterSeconds  int64    `json:"reset_after_seconds"`
	ResetAt            int64    `json:"reset_at"`
}

type codexQuotaLimit struct {
	Allowed         *bool             `json:"allowed"`
	LimitReached    bool              `json:"limit_reached"`
	PrimaryWindow   *codexQuotaWindow `json:"primary_window"`
	SecondaryWindow *codexQuotaWindow `json:"secondary_window"`
}

type codexQuotaUpstreamResponse struct {
	PlanType            string           `json:"plan_type"`
	RateLimit           *codexQuotaLimit `json:"rate_limit"`
	CodeReviewRateLimit *codexQuotaLimit `json:"code_review_rate_limit"`
}

type codexQuotaWindowResponse struct {
	ID                 string  `json:"id"`
	Label              string  `json:"label"`
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// GetCodexQuota returns a sanitized view of provider-reported Codex usage for one credential.
func (h *Handler) GetCodexQuota(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}
	auth := h.authByIndex(authIndex)
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		c.JSON(http.StatusNotFound, gin.H{"error": "Codex credential not found"})
		return
	}

	token, errToken := h.resolveTokenForAuth(c.Request.Context(), auth, "")
	if errToken != nil || strings.TrimSpace(token) == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Codex credential token unavailable"})
		return
	}

	req, errRequest := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, codexQuotaUsageURL, nil)
	if errRequest != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build quota request"})
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")
	if accountID := stringValue(auth.Metadata, "account_id"); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	client := &http.Client{
		Transport: h.apiCallTransport(auth, ""),
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "Codex quota request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Error("Codex quota response body close failed")
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		c.JSON(http.StatusBadGateway, gin.H{
			"error":           "Codex quota request was rejected",
			"upstream_status": resp.StatusCode,
		})
		return
	}

	var upstream codexQuotaUpstreamResponse
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&upstream); errDecode != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid Codex quota response"})
		return
	}

	windows := make([]codexQuotaWindowResponse, 0, 4)
	windows = appendCodexQuotaWindows(windows, "codex", "", upstream.RateLimit)
	windows = appendCodexQuotaWindows(windows, "code-review", "Code review ", upstream.CodeReviewRateLimit)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"plan_type":     strings.TrimSpace(upstream.PlanType),
		"allowed":       codexQuotaAllowed(upstream.RateLimit),
		"limit_reached": upstream.RateLimit != nil && upstream.RateLimit.LimitReached,
		"windows":       windows,
	})
}

func appendCodexQuotaWindows(out []codexQuotaWindowResponse, idPrefix string, labelPrefix string, limit *codexQuotaLimit) []codexQuotaWindowResponse {
	if limit == nil {
		return out
	}
	if limit.PrimaryWindow != nil {
		out = append(out, normalizeCodexQuotaWindow(idPrefix+"-primary", labelPrefix, limit.PrimaryWindow, "Usage"))
	}
	if limit.SecondaryWindow != nil {
		out = append(out, normalizeCodexQuotaWindow(idPrefix+"-secondary", labelPrefix, limit.SecondaryWindow, "Usage"))
	}
	return out
}

func normalizeCodexQuotaWindow(id string, labelPrefix string, window *codexQuotaWindow, fallbackLabel string) codexQuotaWindowResponse {
	usedPercent := 0.0
	if window.UsedPercent != nil {
		usedPercent = max(0, min(100, *window.UsedPercent))
	}
	resetAt := window.ResetAt
	if resetAt <= 0 && window.ResetAfterSeconds > 0 {
		resetAt = time.Now().Unix() + window.ResetAfterSeconds
	}
	return codexQuotaWindowResponse{
		ID:                 id,
		Label:              labelPrefix + codexQuotaWindowLabel(window.LimitWindowSeconds, fallbackLabel),
		UsedPercent:        usedPercent,
		LimitWindowSeconds: window.LimitWindowSeconds,
		ResetAt:            resetAt,
	}
}

func codexQuotaWindowLabel(seconds int64, fallback string) string {
	switch {
	case seconds >= 27*24*60*60:
		return "Monthly limit"
	case seconds >= 6*24*60*60:
		return "Weekly limit"
	case seconds >= 4*60*60 && seconds <= 6*60*60:
		return "5-hour limit"
	case seconds > 0:
		return fmt.Sprintf("%s limit", formatQuotaWindowDuration(seconds))
	default:
		return fallback
	}
}

func formatQuotaWindowDuration(seconds int64) string {
	duration := time.Duration(seconds) * time.Second
	if hours := int64(duration.Hours()); hours >= 1 {
		return fmt.Sprintf("%d-hour", hours)
	}
	if minutes := int64(duration.Minutes()); minutes >= 1 {
		return fmt.Sprintf("%d-minute", minutes)
	}
	return "Usage"
}

func codexQuotaAllowed(limit *codexQuotaLimit) bool {
	return limit == nil || limit.Allowed == nil || *limit.Allowed
}
