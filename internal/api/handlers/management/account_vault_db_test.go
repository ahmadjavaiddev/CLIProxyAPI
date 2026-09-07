package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type stubAccountVaultBackend struct {
	mu      sync.Mutex
	rows    map[string][]byte
	saves   int
	loadErr error
	saveErr error
}

func newStubAccountVaultBackend() *stubAccountVaultBackend {
	return &stubAccountVaultBackend{rows: make(map[string][]byte)}
}

func (s *stubAccountVaultBackend) LoadAccountVaultRows(context.Context) (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := make(map[string][]byte, len(s.rows))
	for id, data := range s.rows {
		out[id] = append([]byte(nil), data...)
	}
	return out, nil
}

func (s *stubAccountVaultBackend) SaveAccountVaultRow(_ context.Context, authIndex string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.rows[authIndex] = append([]byte(nil), data...)
	s.saves++
	return nil
}

func (s *stubAccountVaultBackend) DeleteAccountVaultRow(_ context.Context, authIndex string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, authIndex)
	return nil
}

func (s *stubAccountVaultBackend) savedRow(authIndex string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.rows[authIndex]...)
}

var errStubVaultFailure = errors.New("stub vault failure")

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

func TestAccountVaultUsesDatabaseRows(t *testing.T) {
	stub := newStubAccountVaultBackend()
	handler, authIndex := newAccountVaultDBTestHandler(t, stub)

	putRecorder, putContext := newAccountVaultTestContext(http.MethodPut, "/vault", `{"auth_index":"`+authIndex+`","email":"db@example.com","password":"db-secret"}`)
	handler.PutAccountVaultEntry(putContext)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("put status = %d body=%s", putRecorder.Code, putRecorder.Body.String())
	}
	if saved := string(stub.savedRow(authIndex)); !strings.Contains(saved, "db@example.com") || !strings.Contains(saved, "db-secret") {
		t.Fatalf("database row does not hold the vault entry: %s", saved)
	}

	// Change the database behind the handler's back: reads must follow the DB,
	// proving it is the source of truth rather than the local mirror.
	stub.mu.Lock()
	stub.rows[authIndex] = []byte(`{"credentials":{"email":"changed@example.com"},"updated_at":"2026-01-01T00:00:00Z"}`)
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

func TestAccountVaultSplitsLegacyBlobIntoRows(t *testing.T) {
	stub := newStubAccountVaultBackend()
	handler, authIndex := newAccountVaultDBTestHandler(t, stub)

	blob, errMarshal := json.Marshal(accountVaultFile{
		Version: accountVaultVersion,
		Entries: map[string]accountVaultStoredEntry{
			authIndex: {Credentials: accountVaultEntry{Email: "legacy@example.com", Password: "legacy-secret"}},
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal blob: %v", errMarshal)
	}
	stub.rows[accountVaultLegacyKey] = blob

	getRecorder, getContext := newAccountVaultTestContext(http.MethodGet, "/vault", "")
	getContext.Request.URL.RawQuery = "auth_index=" + authIndex
	handler.GetAccountVaultEntry(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	if !strings.Contains(getRecorder.Body.String(), "legacy@example.com") {
		t.Fatalf("legacy blob values not returned: %s", getRecorder.Body.String())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if _, ok := stub.rows[accountVaultLegacyKey]; ok {
		t.Fatal("legacy blob row was not removed after split")
	}
	if saved := string(stub.rows[authIndex]); !strings.Contains(saved, "legacy-secret") {
		t.Fatalf("legacy blob was not split into rows: %v", stub.rows)
	}
}

func TestAccountVaultSeedsRowsFromLocalFile(t *testing.T) {
	stub := newStubAccountVaultBackend()
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
	if saved := string(stub.savedRow(authIndex)); !strings.Contains(saved, "seed-secret") {
		t.Fatalf("local file was not seeded into rows: %s", saved)
	}
}

func TestAccountVaultRowErrorSurfaces(t *testing.T) {
	stub := newStubAccountVaultBackend()
	stub.saveErr = errStubVaultFailure
	handler, authIndex := newAccountVaultDBTestHandler(t, stub)

	putRecorder, putContext := newAccountVaultTestContext(http.MethodPut, "/vault", `{"auth_index":"`+authIndex+`","email":"x@example.com"}`)
	handler.PutAccountVaultEntry(putContext)
	if putRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("put status = %d, want %d body=%s", putRecorder.Code, http.StatusInternalServerError, putRecorder.Body.String())
	}
}
