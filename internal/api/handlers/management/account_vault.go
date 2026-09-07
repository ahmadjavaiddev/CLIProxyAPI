package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	accountVaultVersion = 1
	accountVaultMaxSize = 4 << 20
	// accountVaultLegacyKey is the id of the pre-row whole-document vault row.
	// New code splits it into per-account rows on first read and deletes it.
	accountVaultLegacyKey = "vault"
)

// accountVaultBackend persists one vault row per account in a shared backend
// (for example, PostgreSQL) instead of a local file. The database is the
// source of truth when available; the local file remains as a seed and
// best-effort mirror.
type accountVaultBackend interface {
	LoadAccountVaultRows(ctx context.Context) (map[string][]byte, error)
	SaveAccountVaultRow(ctx context.Context, authIndex string, data []byte) error
	DeleteAccountVaultRow(ctx context.Context, authIndex string) error
}

// vaultBackend returns the shared vault backend: an explicit override when
// set, otherwise the global token store when it implements the interface,
// otherwise nil for file-only mode.
func (h *Handler) vaultBackend() accountVaultBackend {
	if h == nil {
		return nil
	}
	if h.vaultStore != nil {
		return h.vaultStore
	}
	if h.tokenStore == nil {
		return nil
	}
	backend, _ := h.tokenStore.(accountVaultBackend)
	return backend
}

type accountVaultFile struct {
	Version int                                `json:"version"`
	Entries map[string]accountVaultStoredEntry `json:"entries"`
}

type accountVaultStoredEntry struct {
	Credentials accountVaultEntry `json:"credentials"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

type accountVaultEntry struct {
	Email      string `json:"email,omitempty"`
	Password   string `json:"password,omitempty"`
	TOTPSecret string `json:"totp_secret,omitempty"`
}

type accountVaultPatch struct {
	AuthIndex  string  `json:"auth_index"`
	Email      *string `json:"email"`
	Password   *string `json:"password"`
	TOTPSecret *string `json:"totp_secret"`
}

func (h *Handler) GetAccountVaultEntry(c *gin.Context) {
	authIndex, ok := h.validAccountVaultAuthIndex(c, c.Query("auth_index"))
	if !ok {
		return
	}

	h.vaultMu.Lock()
	defer h.vaultMu.Unlock()
	entry, updatedAt, exists, errLoad := h.loadAccountVaultEntry(authIndex)
	if errLoad != nil {
		h.writeAccountVaultError(c, errLoad)
		return
	}
	h.writeAccountVaultMetadata(c, entry, updatedAt, exists)
}

func (h *Handler) PutAccountVaultEntry(c *gin.Context) {
	var patch accountVaultPatch
	if errBind := c.ShouldBindJSON(&patch); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	authIndex, ok := h.validAccountVaultAuthIndex(c, patch.AuthIndex)
	if !ok {
		return
	}

	h.vaultMu.Lock()
	defer h.vaultMu.Unlock()
	entry, _, _, errLoad := h.loadAccountVaultEntry(authIndex)
	if errLoad != nil {
		h.writeAccountVaultError(c, errLoad)
		return
	}
	if patch.Email != nil {
		entry.Email = strings.TrimSpace(*patch.Email)
	}
	if patch.Password != nil {
		entry.Password = *patch.Password
	}
	if patch.TOTPSecret != nil {
		entry.TOTPSecret = strings.TrimSpace(*patch.TOTPSecret)
	}
	if errValidate := validateAccountVaultEntry(entry); errValidate != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errValidate.Error()})
		return
	}
	updatedAt, errSave := h.saveAccountVaultEntry(authIndex, entry)
	if errSave != nil {
		h.writeAccountVaultError(c, errSave)
		return
	}
	h.writeAccountVaultMetadata(c, entry, updatedAt, true)
}

func (h *Handler) validAccountVaultAuthIndex(c *gin.Context, rawAuthIndex string) (string, bool) {
	authIndex := strings.TrimSpace(rawAuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return "", false
	}
	if h.authByIndex(authIndex) == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return "", false
	}
	return authIndex, true
}

func (h *Handler) writeAccountVaultMetadata(c *gin.Context, entry accountVaultEntry, updatedAt time.Time, exists bool) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"exists":      exists,
		"email":       entry.Email,
		"password":    entry.Password,
		"totp_secret": entry.TOTPSecret,
		"updated_at":  updatedAt,
	})
}

func (h *Handler) loadAccountVaultEntry(authIndex string) (accountVaultEntry, time.Time, bool, error) {
	if h.vaultBackend() == nil {
		vault, errLoad := h.loadAccountVaultLocalFile()
		if errLoad != nil {
			return accountVaultEntry{}, time.Time{}, false, errLoad
		}
		stored, exists := vault.Entries[authIndex]
		if !exists {
			return accountVaultEntry{}, time.Time{}, false, nil
		}
		return stored.Credentials, stored.UpdatedAt, true, nil
	}
	entries, err := h.backendVaultEntries()
	if err != nil {
		return accountVaultEntry{}, time.Time{}, false, err
	}
	stored, exists := entries[authIndex]
	if !exists {
		return accountVaultEntry{}, time.Time{}, false, nil
	}
	return stored.Credentials, stored.UpdatedAt, true, nil
}

func (h *Handler) saveAccountVaultEntry(authIndex string, entry accountVaultEntry) (time.Time, error) {
	backend := h.vaultBackend()
	updatedAt := time.Now().UTC()
	stored := accountVaultStoredEntry{Credentials: entry, UpdatedAt: updatedAt}
	if backend == nil {
		vault, errLoad := h.loadAccountVaultLocalFile()
		if errLoad != nil {
			return time.Time{}, errLoad
		}
		vault.Entries[authIndex] = stored
		if errWrite := h.writeAccountVaultLocalFile(vault); errWrite != nil {
			return time.Time{}, errWrite
		}
		return updatedAt, nil
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return time.Time{}, fmt.Errorf("encode account vault: %w", err)
	}
	if err := backend.SaveAccountVaultRow(context.Background(), authIndex, data); err != nil {
		return time.Time{}, fmt.Errorf("save account vault: %w", err)
	}
	h.refreshVaultMirror(backend)
	return updatedAt, nil
}

// backendVaultEntries returns the effective vault entries from the shared
// backend, running one-time migrations first: a legacy whole-document row is
// split into per-account rows, and a local file seeds an empty database.
func (h *Handler) backendVaultEntries() (map[string]accountVaultStoredEntry, error) {
	backend := h.vaultBackend()
	ctx := context.Background()
	rows, err := backend.LoadAccountVaultRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("load account vault: %w", err)
	}
	if raw, ok := rows[accountVaultLegacyKey]; ok {
		h.migrateLegacyVaultBlob(backend, raw)
		if refetched, err := backend.LoadAccountVaultRows(ctx); err == nil {
			rows = refetched
		}
	}
	if len(rows) == 0 {
		if seeded, err := h.seedVaultRowsFromFile(backend); err != nil {
			log.WithError(err).Warn("account vault seed from local file failed")
		} else if seeded {
			if refetched, err := backend.LoadAccountVaultRows(ctx); err == nil {
				rows = refetched
			}
		}
	}
	entries := make(map[string]accountVaultStoredEntry, len(rows))
	for id, raw := range rows {
		if id == accountVaultLegacyKey {
			continue
		}
		stored, err := decodeAccountVaultRow(raw)
		if err != nil {
			log.WithError(err).Warn("skipping unreadable account vault row")
			continue
		}
		entries[id] = stored
	}
	return entries, nil
}

// migrateLegacyVaultBlob splits a pre-row whole-document vault row into
// per-account rows and removes the legacy row. It is idempotent.
func (h *Handler) migrateLegacyVaultBlob(backend accountVaultBackend, raw []byte) {
	vault, err := decodeAccountVaultBytes(raw)
	if err != nil {
		log.WithError(err).Warn("legacy account vault blob is unreadable; leaving it in place")
		return
	}
	ctx := context.Background()
	for authIndex, stored := range vault.Entries {
		data, err := json.Marshal(stored)
		if err != nil {
			log.WithError(err).Warn("legacy account vault entry encode failed")
			return
		}
		if err := backend.SaveAccountVaultRow(ctx, authIndex, data); err != nil {
			log.WithError(err).Warn("legacy account vault split failed")
			return
		}
	}
	if err := backend.DeleteAccountVaultRow(ctx, accountVaultLegacyKey); err != nil {
		log.WithError(err).Warn("legacy account vault cleanup failed")
	}
}

// seedVaultRowsFromFile copies a local vault file into an empty database.
// It reports whether seeding happened.
func (h *Handler) seedVaultRowsFromFile(backend accountVaultBackend) (bool, error) {
	if _, statErr := os.Stat(h.accountVaultPath()); statErr != nil {
		return false, nil
	}
	vault, err := h.loadAccountVaultLocalFile()
	if err != nil {
		return false, err
	}
	ctx := context.Background()
	for authIndex, stored := range vault.Entries {
		data, err := json.Marshal(stored)
		if err != nil {
			return false, err
		}
		if err := backend.SaveAccountVaultRow(ctx, authIndex, data); err != nil {
			return false, err
		}
	}
	return len(vault.Entries) > 0, nil
}

// refreshVaultMirror rewrites the local vault file from the database.
// Failures only warn: the database already holds the data.
func (h *Handler) refreshVaultMirror(backend accountVaultBackend) {
	rows, err := backend.LoadAccountVaultRows(context.Background())
	if err != nil {
		log.WithError(err).Warn("account vault mirror refresh failed")
		return
	}
	vault := accountVaultFile{Version: accountVaultVersion, Entries: make(map[string]accountVaultStoredEntry, len(rows))}
	for id, raw := range rows {
		if id == accountVaultLegacyKey {
			continue
		}
		stored, err := decodeAccountVaultRow(raw)
		if err != nil {
			log.WithError(err).Warn("skipping unreadable account vault row")
			continue
		}
		vault.Entries[id] = stored
	}
	if err := h.writeAccountVaultLocalFile(vault); err != nil {
		log.WithError(err).Warn("account vault local mirror write failed")
	}
}

func decodeAccountVaultRow(data []byte) (accountVaultStoredEntry, error) {
	if uint64(len(data)) > accountVaultMaxSize {
		return accountVaultStoredEntry{}, fmt.Errorf("decode account vault: row exceeds %d bytes", accountVaultMaxSize)
	}
	var stored accountVaultStoredEntry
	if errDecode := json.Unmarshal(data, &stored); errDecode != nil {
		return accountVaultStoredEntry{}, fmt.Errorf("decode account vault: %w", errDecode)
	}
	return stored, nil
}

func (h *Handler) loadAccountVaultLocalFile() (accountVaultFile, error) {
	vault := accountVaultFile{Version: accountVaultVersion, Entries: make(map[string]accountVaultStoredEntry)}
	file, errOpen := os.Open(h.accountVaultPath())
	if errors.Is(errOpen, os.ErrNotExist) {
		return vault, nil
	}
	if errOpen != nil {
		return accountVaultFile{}, fmt.Errorf("open account vault: %w", errOpen)
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Error("account vault file close failed")
		}
	}()
	return decodeAccountVaultReader(io.LimitReader(file, accountVaultMaxSize))
}

func decodeAccountVaultBytes(data []byte) (accountVaultFile, error) {
	if uint64(len(data)) > accountVaultMaxSize {
		return accountVaultFile{}, fmt.Errorf("decode account vault: document exceeds %d bytes", accountVaultMaxSize)
	}
	return decodeAccountVaultReader(bytes.NewReader(data))
}

func decodeAccountVaultReader(reader io.Reader) (accountVaultFile, error) {
	vault := accountVaultFile{Version: accountVaultVersion, Entries: make(map[string]accountVaultStoredEntry)}
	if errDecode := json.NewDecoder(reader).Decode(&vault); errDecode != nil {
		return accountVaultFile{}, fmt.Errorf("decode account vault: %w", errDecode)
	}
	if vault.Version != accountVaultVersion {
		return accountVaultFile{}, fmt.Errorf("unsupported account vault version %d", vault.Version)
	}
	if vault.Entries == nil {
		vault.Entries = make(map[string]accountVaultStoredEntry)
	}
	return vault, nil
}

func (h *Handler) writeAccountVaultLocalFile(vault accountVaultFile) error {
	path := h.accountVaultPath()
	directory := filepath.Dir(path)
	temporary, errCreate := os.CreateTemp(directory, ".account-vault-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create account vault temporary file: %w", errCreate)
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	temporaryClosed := false
	defer func() {
		if !temporaryClosed {
			if errClose := temporary.Close(); errClose != nil {
				log.WithError(errClose).Error("account vault temporary file close failed")
			}
		}
		if removeTemporary {
			if errRemove := os.Remove(temporaryPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.WithError(errRemove).Error("account vault temporary file cleanup failed")
			}
		}
	}()
	if errChmod := temporary.Chmod(0o600); errChmod != nil {
		return fmt.Errorf("secure account vault temporary file: %w", errChmod)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if errEncode := encoder.Encode(vault); errEncode != nil {
		return fmt.Errorf("encode account vault: %w", errEncode)
	}
	if errSync := temporary.Sync(); errSync != nil {
		return fmt.Errorf("sync account vault: %w", errSync)
	}
	errClose := temporary.Close()
	temporaryClosed = true
	if errClose != nil {
		return fmt.Errorf("close account vault temporary file: %w", errClose)
	}
	if errRename := os.Rename(temporaryPath, path); errRename != nil {
		return fmt.Errorf("replace account vault: %w", errRename)
	}
	removeTemporary = false
	return nil
}

func (h *Handler) accountVaultPath() string {
	configPath := strings.TrimSpace(h.configFilePath)
	if configPath == "" {
		return filepath.Join(".", ".account-vault.json")
	}
	return filepath.Join(filepath.Dir(configPath), ".account-vault.json")
}

func (h *Handler) writeAccountVaultError(c *gin.Context, err error) {
	log.WithError(err).Error("account vault operation failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Account vault operation failed"})
}

func validateAccountVaultEntry(entry accountVaultEntry) error {
	if len(entry.Email) > 320 {
		return errors.New("email must be 320 characters or fewer")
	}
	if len(entry.Password) > 4096 {
		return errors.New("password must be 4096 characters or fewer")
	}
	if len(entry.TOTPSecret) > 4096 {
		return errors.New("2FA secret must be 4096 characters or fewer")
	}
	return nil
}
