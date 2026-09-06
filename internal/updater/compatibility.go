package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const CatalogFormat = 1
const CompatibilityFlag = "--ohmycine-update-compatibility"
const CompatibilityCommand = "--ohmycine-update-capabilities"

// Explicit declaration in checksum-verified official binaries. It gates the
// command invocation because legacy main ignores unknown flags and starts DB.
const compatibilityMarker = "OMC_CAPABILITY_BEGIN:catalog-floor=1;helper-handoff=1;readonly-probe=1:OMC_CAPABILITY_END"

type binaryCompatibility struct {
	Marker         string `json:"marker"`
	CatalogFormat  int    `json:"catalog_format"`
	HelperProtocol int    `json:"helper_protocol"`
}

// RunCompatibilityCommand must run before configuration, logging or DB startup.
func RunCompatibilityCommand(arguments []string, output io.Writer) (bool, error) {
	if len(arguments) < 2 || arguments[1] != CompatibilityCommand {
		return false, nil
	}
	if len(arguments) != 2 {
		return true, coded(CodeCompatibilityRequired, errors.New("invalid capability command"))
	}
	return true, json.NewEncoder(output).Encode(binaryCompatibility{Marker: compatibilityMarker, CatalogFormat: CatalogFormat, HelperProtocol: 1})
}

func verifyBinaryCompatibility(ctx context.Context, executable string) error {
	file, err := os.Open(executable)
	if err != nil {
		return coded(CodeCompatibilityRequired, errors.New("candidate capability cannot be read"))
	}
	defer func() { _ = file.Close() }()
	// Stream bounded chunks with overlap; do not allocate an entire executable.
	buffer := make([]byte, 64<<10)
	tail := []byte(nil)
	remaining := MaxCandidateBytes
	found := false
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := file.Read(buffer)
		remaining -= int64(n)
		window := append(tail, buffer[:n]...)
		if bytes.Contains(window, []byte(compatibilityMarker)) {
			found = true
			break
		}
		keep := len(compatibilityMarker) - 1
		if len(window) < keep {
			keep = len(window)
		}
		tail = append([]byte(nil), window[len(window)-keep:]...)
		if readErr != nil {
			if readErr != io.EOF {
				return coded(CodeCompatibilityRequired, errors.New("candidate capability cannot be read"))
			}
			break
		}
	}
	if !found {
		return coded(CodeCompatibilityRequired, errors.New("candidate has no safe capability command"))
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, executable, CompatibilityCommand)
	output := &limitedCompatibilityOutput{}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return coded(CodeCompatibilityRequired, errors.New("candidate capability command failed"))
	}
	var capabilities binaryCompatibility
	decoder := json.NewDecoder(bytes.NewReader(output.data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&capabilities) != nil || decoder.Decode(&struct{}{}) != io.EOF || capabilities.Marker != compatibilityMarker || capabilities.CatalogFormat < CatalogFormat || capabilities.HelperProtocol != 1 {
		return coded(CodeCompatibilityRequired, errors.New("candidate capability response is invalid"))
	}
	return nil
}

type limitedCompatibilityOutput struct{ data []byte }

func (w *limitedCompatibilityOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > 4096 {
		return 0, errors.New("capability output exceeds limit")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

type formatFloor struct {
	Schema  int `json:"schema"`
	Catalog int `json:"catalog"`
}

// This is private local handoff state, not an API DTO. An old helper cannot
// authorize conversion merely by starting a new binary or returning health OK.
type compatibilityReceipt struct {
	Schema          int    `json:"schema"`
	OperationID     string `json:"operation_id"`
	Nonce           string `json:"nonce"`
	CandidateSHA256 string `json:"candidate_sha256"`
	DatabasePath    string `json:"database_path"`
	Completed       bool   `json:"completed"`
}

// CatalogCompatibility is a startup-scoped capability. It must be checked
// before database.Open and passed explicitly to the converter. It does not
// activate any catalog or perform database migrations by itself.
type CatalogCompatibility struct {
	databasePath     string
	runtimeDirectory string
	executable       string
	freshInstall     bool
	reservedIdentity string
	allowed          bool
	mu               sync.Mutex
}

func (c *CatalogCompatibility) CanActivateCatalog() bool { return c != nil && c.allowed }

// ValidateCatalogDatabase binds conversion authority to the exact database
// checked at startup; a capability obtained for another store is not reusable.
func (c *CatalogCompatibility) ValidateCatalogDatabase(databasePath string) error {
	if !c.CanActivateCatalog() {
		return coded(CodeCompatibilityRequired, errors.New("catalog activation capability is unavailable"))
	}
	canonical, err := compatibilityAbsolutePath(databasePath)
	if err != nil || !equalCanonicalPath(canonical, c.databasePath) {
		return coded(CodeCompatibilityRequired, errors.New("catalog activation database does not match startup"))
	}
	return nil
}

// EnsureCatalogFormat durably raises the rollback floor BEFORE the first
// new-format transaction. A crash between the marker and transaction may deny
// an otherwise possible rollback, but can never permit an unsafe one. The
// database must additionally record its format in the activation transaction.
func (c *CatalogCompatibility) EnsureCatalogFormat() error {
	if !c.CanActivateCatalog() {
		return coded(CodeCompatibilityRequired, errors.New("catalog conversion requires a compatible helper handoff"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.freshInstall {
		return coded(CodeCompatibilityRequired, errors.New("fresh database initialization has not completed"))
	}
	floor, err := readCatalogFloor(c.databasePath)
	if err != nil {
		return err
	}
	if floor > CatalogFormat {
		return coded(CodeFormatIncompatible, errors.New("catalog format is newer than this binary"))
	}
	if floor == CatalogFormat {
		return nil
	}
	if err := durableCompatibilityJSON(c.databasePath+".catalog-format.json", formatFloor{Schema: 1, Catalog: CatalogFormat}); err != nil {
		return coded(CodeCompatibilityRequired, errors.New("catalog format floor could not be persisted"))
	}
	return nil
}

// ValidateDatabaseFormat must be called after opening the database and before
// starting workers. Missing sidecar protection for an activated database is an
// error, not permission to fall back to the legacy catalog.
func (c *CatalogCompatibility) ValidateDatabaseFormat(databaseFormat int) error {
	if c == nil {
		return coded(CodeCompatibilityRequired, errors.New("startup compatibility was not checked"))
	}
	floor, err := readCatalogFloor(c.databasePath)
	if err != nil {
		return err
	}
	if databaseFormat < 0 || databaseFormat > CatalogFormat || databaseFormat > floor {
		return coded(CodeFormatIncompatible, errors.New("database catalog format is not protected or supported"))
	}
	return nil
}

func compatibilityPath(runtimeDirectory string) string {
	return filepath.Join(runtimeDirectory, "updates", "catalog-compatibility.json")
}

// CheckStartupCompatibility is read-only. Existing databases started without
// explicit proof remain usable in legacy mode, but cannot convert. New clean
// installs and databases already behind a durable floor need no old-helper
// promise. Never infer capability from a version string or elapsed time.
func CheckStartupCompatibility(databasePath, runtimeDirectory, executable string, arguments []string) (*CatalogCompatibility, error) {
	databasePath, err := compatibilityAbsolutePath(databasePath)
	if err != nil {
		return nil, err
	}
	runtimeDirectory, err = compatibilityAbsolutePath(runtimeDirectory)
	if err != nil {
		return nil, err
	}
	floor, err := readCatalogFloor(databasePath)
	if err != nil {
		return nil, err
	}
	if floor > CatalogFormat {
		return nil, coded(CodeFormatIncompatible, errors.New("catalog format is newer than this binary"))
	}
	guard := &CatalogCompatibility{databasePath: databasePath, runtimeDirectory: runtimeDirectory, executable: executable, allowed: floor == CatalogFormat}
	_, statErr := os.Lstat(databasePath)
	if errors.Is(statErr, os.ErrNotExist) {
		guard.allowed = true
		guard.freshInstall = true
	} else if statErr != nil {
		return nil, coded(CodeCompatibilityRequired, errors.New("database identity cannot be inspected"))
	}
	if err := guard.checkInitializedOrigin(); err != nil {
		return nil, err
	}
	nonce := ""
	for i, argument := range arguments {
		if argument != CompatibilityFlag {
			continue
		}
		if nonce != "" || i+1 >= len(arguments) || !operationIDPattern.MatchString(arguments[i+1]) {
			return nil, coded(CodeCompatibilityRequired, errors.New("helper capability argument is invalid"))
		}
		nonce = arguments[i+1]
	}
	var receipt compatibilityReceipt
	err = readCompatibilityJSON(compatibilityPath(runtimeDirectory), &receipt)
	if errors.Is(err, os.ErrNotExist) && nonce == "" {
		return guard, nil
	}
	if err != nil {
		return nil, coded(CodeCompatibilityRequired, errors.New("helper capability cannot be read"))
	}
	if receipt.Schema != 1 || !operationIDPattern.MatchString(receipt.OperationID) || !operationIDPattern.MatchString(receipt.Nonce) {
		return nil, coded(CodeCompatibilityRequired, errors.New("helper capability is invalid"))
	}
	// A receipt for another configured database never authorizes this one.
	if !equalCanonicalPath(receipt.DatabasePath, databasePath) {
		return guard, nil
	}
	digest, err := hashFile(executable, MaxCandidateBytes)
	if err != nil {
		return nil, coded(CodeCompatibilityRequired, errors.New("binary identity cannot be verified"))
	}
	if receipt.CandidateSHA256 != digest {
		return guard, nil
	}
	if nonce != "" && nonce != receipt.Nonce {
		return nil, coded(CodeCompatibilityRequired, errors.New("helper capability does not match this start"))
	}
	if receipt.Completed || nonce == receipt.Nonce {
		guard.allowed = true
	}
	return guard, nil
}

// StripCompatibilityArguments removes the one-time startup proof before args
// are persisted into a future update plan; proofs must not be replayed there.
func StripCompatibilityArguments(arguments []string) []string {
	result := make([]string, 0, len(arguments))
	for i := 0; i < len(arguments); i++ {
		if arguments[i] == CompatibilityFlag {
			i++
			continue
		}
		result = append(result, arguments[i])
	}
	return result
}

func readCatalogFloor(databasePath string) (int, error) {
	var floor formatFloor
	err := readCompatibilityJSON(databasePath+".catalog-format.json", &floor)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil || floor.Schema != 1 || floor.Catalog < 1 {
		return 0, coded(CodeFormatIncompatible, errors.New("catalog format floor is unreadable or invalid"))
	}
	return floor.Catalog, nil
}

func readCompatibilityJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("compatibility state is not a regular file")
	}
	return readJSON(path, value)
}

// atomicWriteJSON flushes file contents and uses WRITE_THROUGH on Windows.
// Directory fsync is additionally mandatory on Unix for a crash-safe floor;
// the generic update-state writer intentionally has weaker best-effort sync.
func durableCompatibilityJSON(path string, value any) error {
	if err := atomicWriteJSON(path, value, 0o600); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func compatibilityAbsolutePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.ContainsRune(path, '\x00') {
		return "", coded(CodeCompatibilityRequired, errors.New("compatibility path is invalid"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", coded(CodeCompatibilityRequired, errors.New("compatibility path cannot be resolved"))
	}
	// Resolve existing ancestors without creating anything during preflight.
	current := abs
	var tail []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) || filepath.Dir(current) == current {
			return "", coded(CodeCompatibilityRequired, errors.New("compatibility path cannot be resolved"))
		}
		tail = append(tail, filepath.Base(current))
		current = filepath.Dir(current)
	}
}

func equalCanonicalPath(left, right string) bool {
	left, err := compatibilityAbsolutePath(left)
	if err != nil {
		return false
	}
	right, err = compatibilityAbsolutePath(right)
	if err != nil {
		return false
	}
	return left == right || samePath(left, right)
}
