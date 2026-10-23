package services

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type quarkCatalogActivationFixture struct {
	service       *MediaLibraryService
	db            *gorm.DB
	actor         Actor
	storage       models.Storage
	profile       models.MediaClassificationProfile
	compatibility *updater.CatalogCompatibility
	path          string
	runtime       string
	executable    string
}

func newQuarkCatalogActivationFixture(t *testing.T) *quarkCatalogActivationFixture {
	t.Helper()
	f := &quarkCatalogActivationFixture{runtime: t.TempDir()}
	f.path = filepath.Join(f.runtime, "activation.db")
	var err error
	f.executable, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.compatibility, err = updater.CheckStartupCompatibility(f.path, f.runtime, f.executable, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.compatibility.ReserveFreshDatabase(); err != nil {
		t.Fatal(err)
	}
	f.db, err = database.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := f.db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	if err := database.Migrate(f.db); err != nil {
		t.Fatal(err)
	}
	if err := f.compatibility.CompleteInitialization(); err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "activation", UsernameNormalized: "activation", DisplayName: "Activation", PasswordHash: "disabled-fixture", Status: models.UserStatusActive}
	if err := f.db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	f.actor = Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesCreate: {}}}
	connection := models.Connection{Name: "activation", NameNormalized: "activation", Provider: models.ConnectionProviderQuark, Enabled: true}
	if err := f.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	f.storage = models.Storage{Name: "activation", NameNormalized: "activation", Type: models.StorageTypeQuark, ConnectionID: &connection.ID, RootPath: "private-root", RootPathNormalized: "private-root", Enabled: true, Capabilities: `{}`}
	if err := f.db.Create(&f.storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Where("code = ?", "default-v1").First(&f.profile).Error; err != nil {
		t.Fatal(err)
	}
	f.service = NewMediaLibraryService(f.db, NewAuditService(f.db), zerolog.Nop())
	f.service.SetCatalogCompatibility(f.compatibility)
	f.service.SetConnectionService(NewConnectionService(f.db, f.service.audit, nil, cloud.NewRegistry(), zerolog.Nop()))
	t.Cleanup(f.service.Close)
	return f
}

func (f *quarkCatalogActivationFixture) input() MediaLibraryInput {
	input := testLibraryInput("Quark activation", f.storage, f.profile, false)
	input.ProviderRootID = f.storage.RootPath
	return input
}

func TestQuarkCatalogActivationSurvivesSameSourceRestart(t *testing.T) {
	f := newQuarkCatalogActivationFixture(t)
	created, err := f.service.Create(context.Background(), f.actor, f.input(), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	format, err := database.ReadCatalogFormat(context.Background(), f.db)
	if err != nil || format != CatalogFormat {
		t.Fatalf("created catalog format = %d, error = %v", format, err)
	}
	pool, _ := f.db.DB()
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := updater.CheckStartupCompatibility(f.path, f.runtime, f.executable, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := database.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := reopened.DB()
	t.Cleanup(func() { _ = reader.Close() })
	format, err = database.ReadCatalogFormat(context.Background(), reopened)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ValidateDatabaseFormat(format); err != nil {
		t.Fatalf("same-source restart rejected successful first Quark creation: %s (database format %d)", updater.ErrorCode(err), format)
	}
	var retained models.MediaLibrary
	if err := reopened.First(&retained, created.ID).Error; err != nil || retained.ProviderRootID != f.storage.RootPath {
		t.Fatalf("created library did not survive restart: %v", err)
	}
	for _, model := range []any{&models.CatalogHead{}, &models.CatalogSnapshot{}, &models.CatalogHeadLayer{}} {
		var count int64
		if err := reopened.Model(model).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("initial catalog identity changed across restart: %T count %d, error %v", model, count, err)
		}
	}
	// Startup stays read-only even when an otherwise valid origin survives.
	if err := os.Remove(f.path + ".catalog-format.json"); err != nil {
		t.Fatal(err)
	}
	unprotected, err := updater.CheckStartupCompatibility(f.path, f.runtime, f.executable, nil)
	if err != nil || !unprotected.CanActivateCatalog() {
		t.Fatalf("genuine initialization authority lost: %v", err)
	}
	if err := unprotected.ValidateDatabaseFormat(format); updater.ErrorCode(err) != updater.CodeFormatIncompatible {
		t.Fatalf("startup accepted missing rollback protection: %v", err)
	}
	if _, err := os.Stat(f.path + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup repaired a sidecar: %v", err)
	}
}

func assertNoQuarkCatalogActivation(t *testing.T, f *quarkCatalogActivationFixture) {
	t.Helper()
	for _, model := range []any{&models.MediaLibrary{}, &models.CatalogHead{}, &models.CatalogSnapshot{}, &models.CatalogHeadLayer{}, &models.MediaLibraryStructureAutoState{}} {
		var count int64
		if err := f.db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("failed activation committed %T: count %d, error %v", model, count, err)
		}
	}
	format, err := database.ReadCatalogFormat(context.Background(), f.db)
	if err != nil || format != 0 {
		t.Fatalf("failed activation raised database format: %d, error %v", format, err)
	}
}

func TestQuarkCatalogActivationRejectsMissingWrongAndIncompatibleAuthority(t *testing.T) {
	for _, scenario := range []string{"nil", "missing_origin", "wrong_database", "future_floor", "invalid_floor"} {
		t.Run(scenario, func(t *testing.T) {
			f := newQuarkCatalogActivationFixture(t)
			wanted := updater.CodeCompatibilityRequired
			switch scenario {
			case "nil":
				f.service.SetCatalogCompatibility(nil)
			case "missing_origin":
				if err := os.Remove(f.path + ".catalog-origin.json"); err != nil {
					t.Fatal(err)
				}
				unproven, err := updater.CheckStartupCompatibility(f.path, f.runtime, f.executable, nil)
				if err != nil || unproven.CanActivateCatalog() {
					t.Fatalf("existing unproven database gained authority: %v", err)
				}
				f.service.SetCatalogCompatibility(unproven)
			case "wrong_database":
				other := newQuarkCatalogActivationFixture(t)
				f.service.SetCatalogCompatibility(other.compatibility)
			case "future_floor", "invalid_floor":
				payload := []byte(`{"schema":1,"catalog":2}`)
				if scenario == "invalid_floor" {
					payload = []byte(`{"schema":1,"catalog":1,"unexpected":true}`)
				}
				if err := os.WriteFile(f.path+".catalog-format.json", payload, 0o600); err != nil {
					t.Fatal(err)
				}
				wanted = updater.CodeFormatIncompatible
			}
			if _, err := f.service.Create(context.Background(), f.actor, f.input(), RequestContext{}); updater.ErrorCode(err) != wanted {
				t.Fatalf("unlawful activation result = %v, wanted %s", err, wanted)
			}
			assertNoQuarkCatalogActivation(t, f)
			if scenario != "future_floor" && scenario != "invalid_floor" {
				if _, err := os.Stat(f.path + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused authority wrote a sidecar: %v", err)
				}
			}
		})
	}
}

func TestQuarkCatalogActivationRefusesFilesystemPersistenceFailure(t *testing.T) {
	f := newQuarkCatalogActivationFixture(t)
	// GORM traces Scan after closing its result rows. Close only this isolated
	// DB and remove its parent at that boundary, after the real path lookup and
	// before the real Ensure writer. This works on Windows without ACL changes.
	moved := f.runtime + "-moved"
	displaced := false
	defer func() {
		if displaced {
			if err := os.Rename(moved, f.runtime); err != nil {
				t.Errorf("restore isolated fixture directory: %v", err)
			}
		}
	}()
	f.service.db = f.db.Session(&gorm.Session{Logger: catalogActivationTrace{Interface: f.db.Logger, afterDatabaseList: func() {
		pool, _ := f.db.DB()
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.runtime, moved); err != nil {
			t.Fatal(err)
		}
		displaced = true
	}}})
	if _, err := f.service.Create(context.Background(), f.actor, f.input(), RequestContext{}); updater.ErrorCode(err) != updater.CodeCompatibilityRequired {
		t.Fatalf("failed persistence allowed activation or failed before compatibility: %v", err)
	}
	if !displaced {
		t.Fatal("real filesystem writer fault was not installed")
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.Base(f.path)+".catalog-format.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed persistence wrote a sidecar: %v", err)
	}
	if err := os.Rename(moved, f.runtime); err != nil {
		t.Fatal(err)
	}
	displaced = false
	var err error
	f.db, err = database.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := f.db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	assertNoQuarkCatalogActivation(t, f)
}

type catalogActivationTrace struct {
	logger.Interface
	afterDatabaseList func()
}

func (trace catalogActivationTrace) Trace(ctx context.Context, begin time.Time, query func() (string, int64), err error) {
	sql, _ := query()
	trace.Interface.Trace(ctx, begin, query, err)
	if err == nil && strings.TrimSpace(sql) == "PRAGMA database_list" {
		trace.afterDatabaseList()
	}
}

func TestQuarkCatalogActivationPersistsBeforeTransactionAndRetainsFloorOnRollback(t *testing.T) {
	f := newQuarkCatalogActivationFixture(t)
	begins := 0
	session := f.db.Session(&gorm.Session{NewDB: true, Context: context.Background()})
	session.Statement.ConnPool = &catalogActivationBeginPool{ConnPool: session.Statement.ConnPool, beforeBegin: func() {
		begins++
		if err := f.compatibility.ValidateDatabaseFormat(CatalogFormat); err != nil {
			t.Errorf("write transaction began before durable protection: %v", err)
		}
	}}
	f.service.db = session
	blocked := errors.New("isolated catalog insertion failure")
	observed := false
	callback := "test:catalog-activation-failure"
	if err := f.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "catalog_heads" {
			return
		}
		observed = true
		if err := f.compatibility.ValidateDatabaseFormat(CatalogFormat); err != nil {
			t.Errorf("first catalog write preceded durable protection: %v", err)
		}
		tx.AddError(blocked)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Create(context.Background(), f.actor, f.input(), RequestContext{}); !errors.Is(err, blocked) || !observed {
		t.Fatalf("real catalog insertion failure was not observed: %v", err)
	}
	if begins != 1 {
		t.Fatalf("expected one observed outer write transaction, got %d", begins)
	}
	assertNoQuarkCatalogActivation(t, f)
	if err := f.compatibility.ValidateDatabaseFormat(CatalogFormat); err != nil {
		t.Fatalf("rollback lowered durable format protection: %v", err)
	}
	if err := f.db.Callback().Create().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Create(context.Background(), f.actor, f.input(), RequestContext{}); err != nil {
		t.Fatalf("ordinary retry after rollback failed: %v", err)
	}
}

type catalogActivationBeginPool struct {
	gorm.ConnPool
	beforeBegin func()
}

func (pool *catalogActivationBeginPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	pool.beforeBegin()
	return pool.ConnPool.(gorm.TxBeginner).BeginTx(ctx, options)
}

func TestQuarkCatalogActivationRejectsFilesystemWorkInsideTransaction(t *testing.T) {
	f := newQuarkCatalogActivationFixture(t)
	err := f.db.Transaction(func(tx *gorm.DB) error {
		service := NewMediaLibraryService(tx, NewAuditService(tx), zerolog.Nop())
		service.SetConnectionService(f.service.connections)
		service.SetCatalogCompatibility(f.compatibility)
		defer service.Close()
		_, err := service.Create(context.Background(), f.actor, f.input(), RequestContext{})
		return err
	})
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("transaction accepted filesystem activation: %v", err)
	}
	assertNoQuarkCatalogActivation(t, f)
	if _, err := os.Stat(f.path + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction touched rollback protection: %v", err)
	}
}

func TestQuarkCatalogActivationRejectsUnpreparedSourceTransition(t *testing.T) {
	f := newQuarkCatalogActivationFixture(t)
	f.storage.Type = models.StorageTypePan115
	if err := f.db.Save(&f.storage).Error; err != nil {
		t.Fatal(err)
	}
	// Emulate a source kind changing after the preflight read. The creation
	// transaction must never reach Quark activation with a skipped Ensure.
	callback := "test:catalog-source-transition"
	if err := f.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_libraries" {
			tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Model(&models.Storage{}).Where("id = ?", f.storage.ID).Update("type", models.StorageTypeQuark).Error)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.db.Callback().Create().Remove(callback) }()
	if _, err := f.service.Create(context.Background(), f.actor, f.input(), RequestContext{}); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("unprepared Quark source transition reached activation: %v", err)
	}
	assertNoQuarkCatalogActivation(t, f)
	if _, err := os.Stat(f.path + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprepared source transition wrote a sidecar: %v", err)
	}
}

func TestNonQuarkLibraryCreationDoesNotActivateCatalogFormat(t *testing.T) {
	for _, storageType := range []string{models.StorageTypeLocal, models.StorageTypePan115} {
		t.Run(storageType, func(t *testing.T) {
			f := newQuarkCatalogActivationFixture(t)
			f.service.SetCatalogCompatibility(nil)
			f.storage.Type = storageType
			if storageType == models.StorageTypeLocal {
				f.storage.RootPath, f.storage.RootPathNormalized = f.runtime, f.runtime
				f.storage.ConnectionID = nil
			}
			if err := f.db.Save(&f.storage).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.Create(context.Background(), f.actor, f.input(), RequestContext{}); err != nil {
				t.Fatalf("existing library creation now requires catalog authority: %v", err)
			}
			format, err := database.ReadCatalogFormat(context.Background(), f.db)
			if err != nil || format != 0 {
				t.Fatalf("non-Quark creation activated format: %d, error %v", format, err)
			}
			var heads int64
			if err := f.db.Model(&models.CatalogHead{}).Count(&heads).Error; err != nil || heads != 0 {
				t.Fatalf("non-Quark creation initialized a catalog: %d, error %v", heads, err)
			}
			if _, err := os.Stat(f.path + ".catalog-format.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("non-Quark creation touched rollback protection: %v", err)
			}
		})
	}
}
