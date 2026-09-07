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
)

// accountVaultBackend persists the vault in a shared backend (for example,
// PostgreSQL) instead of a local file. The database is the source of truth
// when available; the local file remains as a seed and best-effort mirror.
type accountVaultBackend interface {
	LoadAccountVault(ctx context.Context) ([]byte, error)
	SaveAccountVault(ctx context.Context, data []byte) error
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
	vault, errLoad := h.loadAccountVaultFile()
	if errLoad != nil {
		return accountVaultEntry{}, time.Time{}, false, errLoad
	}
	stored, exists := vault.Entries[authIndex]
	if !exists {
		return accountVaultEntry{}, time.Time{}, false, nil
	}
	return stored.Credentials, stored.UpdatedAt, true, nil
}

func (h *Handler) saveAccountVaultEntry(authIndex string, entry accountVaultEntry) (time.Time, error) {
	updatedAt := time.Now().UTC()
	vault, errLoad := h.loadAccountVaultFile()
	if errLoad != nil {
		return time.Time{}, errLoad
	}
	vault.Entries[authIndex] = accountVaultStoredEntry{
		Credentials: entry,
		UpdatedAt:   updatedAt,
	}
	if errWrite := h.writeAccountVaultFile(vault); errWrite != nil {
		return time.Time{}, errWrite
	}
	return updatedAt, nil
}

func (h *Handler) loadAccountVaultFile() (accountVaultFile, error) {
	backend := h.vaultBackend()
	if backend == nil {
		return h.loadAccountVaultLocalFile()
	}
	data, err := backend.LoadAccountVault(context.Background())
	if err != nil {
		return accountVaultFile{}, fmt.Errorf("load account vault: %w", err)
	}
	if len(bytes.TrimSpace(data)) != 0 {
		vault, err := decodeAccountVaultBytes(data)
		if err != nil {
			return accountVaultFile{}, err
		}
		h.mirrorAccountVaultFile(vault)
		return vault, nil
	}
	// Database is empty: seed it once from the local file when present, so an
	// existing .account-vault.json migrates automatically.
	vault, err := h.loadAccountVaultLocalFile()
	if err != nil {
		return accountVaultFile{}, err
	}
	if _, statErr := os.Stat(h.accountVaultPath()); statErr == nil {
		if seed, errMarshal := json.Marshal(vault); errMarshal == nil {
			if errSave := backend.SaveAccountVault(context.Background(), seed); errSave != nil {
				log.WithError(errSave).Warn("account vault seed from local file failed")
			}
		}
	}
	return vault, nil
}

func (h *Handler) writeAccountVaultFile(vault accountVaultFile) error {
	backend := h.vaultBackend()
	if backend == nil {
		return h.writeAccountVaultLocalFile(vault)
	}
	data, err := json.Marshal(vault)
	if err != nil {
		return fmt.Errorf("encode account vault: %w", err)
	}
	if err := backend.SaveAccountVault(context.Background(), data); err != nil {
		return fmt.Errorf("save account vault: %w", err)
	}
	h.mirrorAccountVaultFile(vault)
	return nil
}

// mirrorAccountVaultFile refreshes the local vault file copy. Failures only
// warn: the shared backend already holds the data.
func (h *Handler) mirrorAccountVaultFile(vault accountVaultFile) {
	if err := h.writeAccountVaultLocalFile(vault); err != nil {
		log.WithError(err).Warn("account vault local mirror write failed")
	}
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
