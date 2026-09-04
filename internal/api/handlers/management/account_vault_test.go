package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAccountVaultRoundTrip(t *testing.T) {
	directory := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "vault-auth", Provider: "codex"}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	handler := NewHandler(&config.Config{AuthDir: directory}, filepath.Join(directory, "config.yaml"), manager)

	putBody := `{
		"auth_index":"` + authIndex + `",
		"email":"owner@example.com",
		"password":"correct horse battery staple",
		"totp_secret":"2fa-test-string"
	}`
	putRecorder, putContext := newAccountVaultTestContext(http.MethodPut, "/vault", putBody)
	handler.PutAccountVaultEntry(putContext)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", putRecorder.Code, putRecorder.Body.String())
	}

	vaultBytes, errRead := os.ReadFile(handler.accountVaultPath())
	if errRead != nil {
		t.Fatalf("read vault: %v", errRead)
	}
	if info, errStat := os.Stat(handler.accountVaultPath()); errStat != nil {
		t.Fatalf("stat vault: %v", errStat)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("vault mode = %o, want 600", got)
	}
	for _, value := range []string{"owner@example.com", "correct horse battery staple", "2fa-test-string"} {
		if !strings.Contains(string(vaultBytes), value) {
			t.Fatalf("plaintext value %q was not saved in the local vault", value)
		}
	}

	getRecorder, getContext := newAccountVaultTestContext(http.MethodGet, "/vault?auth_index="+authIndex, "")
	getContext.Request.URL.RawQuery = "auth_index=" + authIndex
	handler.GetAccountVaultEntry(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	var credentials struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		TOTPSecret string `json:"totp_secret"`
	}
	if errDecode := json.Unmarshal(getRecorder.Body.Bytes(), &credentials); errDecode != nil {
		t.Fatalf("decode credentials: %v", errDecode)
	}
	if credentials.Email != "owner@example.com" || credentials.Password != "correct horse battery staple" || credentials.TOTPSecret != "2fa-test-string" {
		t.Fatalf("unexpected credentials: %+v", credentials)
	}

	updateRecorder, updateContext := newAccountVaultTestContext(http.MethodPut, "/vault", `{"auth_index":"`+authIndex+`","email":"updated@example.com"}`)
	handler.PutAccountVaultEntry(updateContext)
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("metadata-only update status = %d body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}

	if !strings.Contains(updateRecorder.Body.String(), "correct horse battery staple") || !strings.Contains(updateRecorder.Body.String(), "2fa-test-string") {
		t.Fatalf("metadata-only update did not preserve credentials: %s", updateRecorder.Body.String())
	}
}

func newAccountVaultTestContext(method string, target string, body string) (*httptest.ResponseRecorder, *gin.Context) {
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		ginContext.Request.Header.Set("Content-Type", "application/json")
	}
	return recorder, ginContext
}
