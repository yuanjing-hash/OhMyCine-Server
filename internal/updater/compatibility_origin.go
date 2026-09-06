package updater

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// initializedOrigin is private provenance, not a format activation or a claim
// that an external deployment manager supports rollback. Existing managed
// legacy deployments still need a separately verified operator capability.
type initializedOrigin struct {
	Schema            int    `json:"schema"`
	HelperProtocol    int    `json:"helper_protocol"`
	CatalogCapability int    `json:"catalog_capability"`
	DatabasePath      string `json:"database_path"`
	RuntimeDirectory  string `json:"runtime_directory"`
	FileIdentity      string `json:"file_identity"`
	InitializerSHA256 string `json:"initializer_sha256"`
}

func (c *CatalogCompatibility) originPath() string { return c.databasePath + ".catalog-origin.json" }

// ReserveFreshDatabase closes the absent-file TOCTOU gap before database.Open.
// It only creates a genuinely new empty DB file, never truncates or classifies
// an existing empty DB as fresh. Main calls this after read-only preflight.
func (c *CatalogCompatibility) ReserveFreshDatabase() error {
	if c == nil {
		return coded(CodeCompatibilityRequired, errors.New("startup compatibility was not checked"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.freshInstall {
		return nil
	}
	if c.reservedIdentity != "" {
		return coded(CodeCompatibilityRequired, errors.New("fresh database was already reserved"))
	}
	if err := os.MkdirAll(filepath.Dir(c.databasePath), 0o700); err != nil {
		return coded(CodeCompatibilityRequired, errors.New("fresh database directory could not be created"))
	}
	file, err := os.OpenFile(c.databasePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return coded(CodeCompatibilityRequired, errors.New("database appeared after preflight; fresh initialization refused"))
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return coded(CodeCompatibilityRequired, errors.New("fresh database reservation could not be flushed"))
	}
	if err := file.Close(); err != nil {
		return coded(CodeCompatibilityRequired, errors.New("fresh database reservation could not be closed"))
	}
	identity, err := databaseFileIdentity(c.databasePath)
	if err != nil {
		return coded(CodeCompatibilityRequired, errors.New("fresh database identity cannot be read"))
	}
	c.reservedIdentity = identity
	return nil
}

// CompleteInitialization preserves clean-install provenance without raising
// the catalog floor. Call only after migrations/core initialization/listener
// setup succeeded. A failed first boot leaves no provenance and cannot later
// reinterpret that existing database as fresh.
func (c *CatalogCompatibility) CompleteInitialization() error {
	if c == nil {
		return coded(CodeCompatibilityRequired, errors.New("startup compatibility was not checked"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.freshInstall {
		return nil
	}
	if c.reservedIdentity == "" {
		return coded(CodeCompatibilityRequired, errors.New("fresh database ownership was not reserved"))
	}
	identity, err := databaseFileIdentity(c.databasePath)
	if err != nil || identity != c.reservedIdentity {
		return coded(CodeCompatibilityRequired, errors.New("fresh database identity changed during initialization"))
	}
	digest, err := hashFile(c.executable, MaxCandidateBytes)
	if err != nil {
		return coded(CodeCompatibilityRequired, errors.New("initializing binary identity cannot be verified"))
	}
	origin := initializedOrigin{Schema: 1, HelperProtocol: 1, CatalogCapability: CatalogFormat, DatabasePath: c.databasePath, RuntimeDirectory: c.runtimeDirectory, FileIdentity: identity, InitializerSHA256: digest}
	if err := durableCompatibilityJSON(c.originPath(), origin); err != nil {
		return coded(CodeCompatibilityRequired, errors.New("fresh initialization provenance could not be persisted"))
	}
	c.freshInstall = false
	return nil
}

func (c *CatalogCompatibility) checkInitializedOrigin() error {
	var origin initializedOrigin
	err := readCompatibilityJSON(c.originPath(), &origin)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return coded(CodeCompatibilityRequired, errors.New("initialization provenance cannot be read"))
	}
	digest, decodeErr := hex.DecodeString(origin.InitializerSHA256)
	if origin.Schema != 1 || origin.HelperProtocol != 1 || origin.CatalogCapability < 1 || origin.CatalogCapability > CatalogFormat || decodeErr != nil || len(digest) != 32 || len(origin.FileIdentity) == 0 || len(origin.FileIdentity) > 128 {
		return coded(CodeCompatibilityRequired, errors.New("initialization provenance is invalid"))
	}
	if !equalCanonicalPath(origin.DatabasePath, c.databasePath) || !equalCanonicalPath(origin.RuntimeDirectory, c.runtimeDirectory) {
		return coded(CodeCompatibilityRequired, errors.New("initialization provenance belongs to another database or runtime"))
	}
	identity, err := databaseFileIdentity(c.databasePath)
	if err != nil || identity != origin.FileIdentity {
		return coded(CodeCompatibilityRequired, errors.New("initialized database identity changed"))
	}
	c.allowed = true
	return nil
}
