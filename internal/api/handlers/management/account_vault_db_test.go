package management

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var errStubVaultFailure = errors.New("stub vault failure")

type stubAccountVaultBackend struct {
	mu      sync.Mutex
	data    []byte
	saves   int
	loadErr error
	saveErr error
}

func (s *stubAccountVaultBackend) LoadAccountVault(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return append([]byte(nil), s.data...), nil
}

func (s *stubAccountVaultBackend) SaveAccountVault(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.data = append([]byte(nil), data...)
	s.saves++
	return nil
}

func (s *stubAccountVaultBackend) saved() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...)
}

func newAccountVaultDBTestHandler(t *testing.T, stub *stubAccountVaultBackend) (*Handler, string) {
	t.Helper()
	directory := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "vault-db-auth", Provider: "codex"}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	handler := NewHandler(&config.Config{AuthDir: directory}, filepath.Join(directory, "config.yaml"), manager)
	handler.vaultStore = stub
	return handler, authIndex
}

func TestAccountVaultUsesDatabaseBackend(t *testing.T) {
	stub := &stubAccountVaultBackend{}
	handler, authIndex := newAccountVaultDBTestHandler(t, stub)

	putRecorder, putContext := newAccountVaultTestContext(http.MethodPut, "/vault", `{"auth_index":"`+authIndex+`","email":"db@example.com","password":"db-secret"}`)
	handler.PutAccountVaultEntry(putContext)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", putRecorder.Code, putRecorder.Body.String())
	}
	if saved := string(stub.saved()); !strings.Contains(saved, "db@example.com") || !strings.Contains(saved, "db-secret") {
		t.Fatalf("database does not hold the vault: %s", saved)
	}

	// Change the database behind the handler's back: reads must follow the DB,
	// proving it is the source of truth rather than the local mirror.
	stub.mu.Lock()
	stub.data = []byte(`{"version":1,"entries":{"` + authIndex + `":{"credentials":{"email":"changed@example.com"},"updated_at":"2026-01-01T00:00:00Z"}}}`)
	stub.mu.Unlock()

	getRecorder, getContext := newAccountVaultTestContext(http.MethodGet, "/vault", "")
	getContext.Request.URL.RawQuery = "auth_index=" + authIndex
	handler.GetAccountVaultEntry(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	if !strings.Contains(getRecorder.Body.String(), "changed@example.com") {
		t.Fatalf("get did not follow the database: %s", getRecorder.Body.String())
	}
}

func TestAccountVaultSeedsDatabaseFromLocalFile(t *testing.T) {
	stub := &stubAccountVaultBackend{}
	handler, authIndex := newAccountVaultDBTestHandler(t, stub)

	seed := accountVaultFile{
		Version: accountVaultVersion,
		Entries: map[string]accountVaultStoredEntry{
			authIndex: {Credentials: accountVaultEntry{Email: "seed@example.com", Password: "seed-secret"}},
		},
	}
	if err := handler.writeAccountVaultLocalFile(seed); err != nil {
		t.Fatalf("write local seed: %v", err)
	}

	getRecorder, getContext := newAccountVaultTestContext(http.MethodGet, "/vault", "")
	getContext.Request.URL.RawQuery = "auth_index=" + authIndex
	handler.GetAccountVaultEntry(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	if !strings.Contains(getRecorder.Body.String(), "seed@example.com") {
		t.Fatalf("seed values not returned: %s", getRecorder.Body.String())
	}
	if saved := string(stub.saved()); !strings.Contains(saved, "seed@example.com") {
		t.Fatalf("local file was not seeded into the database: %s", saved)
	}
}

func TestAccountVaultDatabaseErrorSurfaces(t *testing.T) {
	stub := &stubAccountVaultBackend{saveErr: errStubVaultFailure}
	handler, authIndex := newAccountVaultDBTestHandler(t, stub)

	putRecorder, putContext := newAccountVaultTestContext(http.MethodPut, "/vault", `{"auth_index":"`+authIndex+`","email":"x@example.com"}`)
	handler.PutAccountVaultEntry(putContext)
	if putRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("put status = %d, want %d body=%s", putRecorder.Code, http.StatusInternalServerError, putRecorder.Body.String())
	}
}
