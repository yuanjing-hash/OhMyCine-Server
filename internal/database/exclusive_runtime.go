package database

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"gorm.io/gorm"
)

var ErrExclusiveRuntime = errors.New("database_exclusive_runtime_unavailable")

const exclusiveRuntimeSetting = "ohmycine:database:exclusive-runtime"

// ExclusiveRuntime is an OS-held startup capability, not a database lease.
// The stable lock file is never unlinked: unlinking would let another process
// lock a different inode while this process still owns the original lock.
type ExclusiveRuntime struct {
	mu   sync.Mutex
	file *os.File
	path string
	id   string
}

func runtimeSamePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func runtimeCanonicalPath(path string) (string, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.ContainsAny(path, "?\x00") {
		return "", ErrExclusiveRuntime
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", ErrExclusiveRuntime
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", ErrExclusiveRuntime
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func runtimeOrdinaryFile(path string, allowMissing bool) error {
	info, err := os.Lstat(path)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || storage.IsReparsePoint(path, info) {
		return ErrExclusiveRuntime
	}
	f, err := os.Open(path)
	if err != nil {
		return ErrExclusiveRuntime
	}
	defer func() { _ = f.Close() }()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return ErrExclusiveRuntime
	}
	return runtimeSingleLink(f)
}

// AcquireExclusiveRuntime runs before opening/migrating the database or
// starting workers. It does not create or modify the actual SQLite file.
func AcquireExclusiveRuntime(path string) (*ExclusiveRuntime, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.ContainsAny(path, "?\x00") {
		return nil, ErrExclusiveRuntime
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ErrExclusiveRuntime
	}
	canonical, err := runtimeCanonicalPath(path)
	if err != nil {
		return nil, err
	}
	if err := runtimeOrdinaryFile(canonical, true); err != nil {
		return nil, err
	}
	lockPath := canonical + ".runtime.lock"
	if err := runtimeOrdinaryFile(lockPath, true); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, ErrExclusiveRuntime
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	info, err := os.Lstat(lockPath)
	actual, statErr := f.Stat()
	if err != nil || statErr != nil || !info.Mode().IsRegular() || storage.IsReparsePoint(lockPath, info) || !os.SameFile(info, actual) {
		return nil, ErrExclusiveRuntime
	}
	if err := runtimeSingleLink(f); err != nil {
		return nil, err
	}
	if err := lockRuntimeFile(f); err != nil {
		return nil, ErrExclusiveRuntime
	}
	ok = true
	return &ExclusiveRuntime{file: f, path: canonical, id: uuid.NewString()}, nil
}

func (r *ExclusiveRuntime) verifyDB(db *gorm.DB) error {
	if r == nil || db == nil {
		return ErrExclusiveRuntime
	}
	r.mu.Lock()
	path, live := r.path, r.file != nil
	r.mu.Unlock()
	if !live {
		return ErrExclusiveRuntime
	}
	// Never hold the capability mutex while acquiring a DB connection: a
	// concurrent transaction may own that connection and need this capability.
	var rows []struct {
		Name string
		File string
	}
	if err := db.Session(&gorm.Session{}).Raw("PRAGMA database_list").Scan(&rows).Error; err != nil {
		return ErrExclusiveRuntime
	}
	for _, row := range rows {
		if row.Name != "main" {
			continue
		}
		actual, err := runtimeCanonicalPath(row.File)
		if err == nil && runtimeSamePath(actual, path) {
			return nil
		}
	}
	return ErrExclusiveRuntime
}

func (r *ExclusiveRuntime) Bind(db *gorm.DB) (*gorm.DB, error) {
	if r == nil {
		return nil, ErrExclusiveRuntime
	}
	if err := r.verifyDB(db); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil, ErrExclusiveRuntime
	}
	if err := runtimeOrdinaryFile(r.path, false); err != nil {
		return nil, err
	}
	return db.Set(exclusiveRuntimeSetting, r).Session(&gorm.Session{}), nil
}

// An unbound fixture has no crash-recovery authority. A copied capability for
// another DB, or one whose OS lock was released, fails instead of returning ID.
func ExclusiveRuntimeID(db *gorm.DB) (string, error) {
	if db == nil {
		return "", ErrExclusiveRuntime
	}
	value, ok := db.Get(exclusiveRuntimeSetting)
	if !ok {
		return "", nil
	}
	r, ok := value.(*ExclusiveRuntime)
	if !ok || r == nil {
		return "", ErrExclusiveRuntime
	}
	if err := r.verifyDB(db); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return "", ErrExclusiveRuntime
	}
	return r.id, nil
}

func (r *ExclusiveRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
