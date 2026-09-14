package nodeagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestNodeStoreMigrationsCreateFreshSchemaAndReopenIdempotently(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}

	wantVersions := []int{1, 2, 3, 4, 5, 6, 7}
	versions, appliedAt := nodeMigrationLedger(t, store.db)
	if !reflect.DeepEqual(versions, wantVersions) {
		t.Fatalf("fresh migration versions=%v want=%v", versions, wantVersions)
	}
	for _, table := range []string{
		"operations",
		"paired_identity",
		"enrollment_challenges",
		"http_request_nonces",
		"credential_grants",
		"provider_receipts",
		"managed_downloads",
		"file_exports",
		"file_export_files",
		"file_export_chunks",
		"storage_upload_files",
		"storage_source_runs",
		"storage_source_files",
		"storage_source_chunks",
	} {
		assertNodeTableExists(t, store.db, table)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	gotVersions, gotAppliedAt := nodeMigrationLedger(t, reopened.db)
	if !reflect.DeepEqual(gotVersions, wantVersions) || !reflect.DeepEqual(gotAppliedAt, appliedAt) {
		t.Fatalf("reopen changed migration ledger: versions=%v applied_at=%v", gotVersions, gotAppliedAt)
	}
}

func TestNodeStoreMigrationUpgradesV3ToV4WithoutChangingV3Data(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	want := ManagedDownload{
		ServerID:           "server-1",
		DownloaderID:       "downloader-1",
		TaskID:             "task-1",
		ProviderTaskID:     "provider-1",
		Tag:                "omc-task-1",
		DownloaderSavePath: "/downloads/task-1",
		NodeLocalRoot:      filepath.Join(t.TempDir(), "downloads", "task-1"),
	}
	if err := store.SaveManagedDownload(context.Background(), want, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE http_request_nonces; DROP TABLE storage_source_chunks; DROP TABLE storage_source_files; DROP TABLE storage_source_runs; DROP TABLE storage_upload_files; DELETE FROM node_schema_migrations WHERE version IN (4,5,6,7)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upgraded.Close() }()
	versions, _ := nodeMigrationLedger(t, upgraded.db)
	if !reflect.DeepEqual(versions, []int{1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("upgrade versions=%v", versions)
	}
	for _, table := range []string{"file_exports", "file_export_files", "file_export_chunks", "storage_upload_files"} {
		assertNodeTableExists(t, upgraded.db, table)
	}
	got, err := upgraded.ManagedDownload(context.Background(), want.ServerID, want.DownloaderID, want.ProviderTaskID)
	if err != nil || got != want {
		t.Fatalf("v3 data changed during upgrade: got=%+v err=%v", got, err)
	}
}

func TestNodeStoreFailedMigrationRollsBackSchemaAndDoesNotRecordVersion(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE http_request_nonces; DROP TABLE storage_source_chunks; DROP TABLE storage_source_files; DROP TABLE storage_source_runs; DROP TABLE storage_upload_files; DELETE FROM node_schema_migrations WHERE version IN (4,5,6,7); CREATE TABLE storage_upload_files (broken TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if failed, err := OpenStore(databasePath); err == nil {
		_ = failed.Close()
		t.Fatal("broken v3 schema unexpectedly migrated")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(databasePath)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	versions, _ := nodeMigrationLedger(t, db)
	if !reflect.DeepEqual(versions, []int{1, 2, 3}) {
		t.Fatalf("failed migration was recorded: versions=%v", versions)
	}
	var created int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_node_storage_upload_status'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("failed migration did not roll back the storage upload index")
	}
	var columns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('storage_upload_files')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 1 {
		t.Fatalf("pre-existing failure fixture changed: columns=%d", columns)
	}
}

func TestNodeStoreFailedV6MigrationRollsBackFirstColumn(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE http_request_nonces; ALTER TABLE storage_source_runs DROP COLUMN cleanup_status; ALTER TABLE storage_source_runs DROP COLUMN cleaned_at; ALTER TABLE storage_source_runs ADD COLUMN cleaned_at DATETIME; DELETE FROM node_schema_migrations WHERE version IN (6,7)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if failed, err := OpenStore(databasePath); err == nil {
		_ = failed.Close()
		t.Fatal("broken v5 schema unexpectedly migrated")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(databasePath)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	versions, _ := nodeMigrationLedger(t, db)
	if !reflect.DeepEqual(versions, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("failed v6 migration was recorded: versions=%v", versions)
	}
	var cleanupColumns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('storage_source_runs') WHERE name='cleanup_status'`).Scan(&cleanupColumns); err != nil {
		t.Fatal(err)
	}
	if cleanupColumns != 0 {
		t.Fatal("failed v6 migration retained the first column")
	}
}

func nodeMigrationLedger(t *testing.T, db *sql.DB) ([]int, []time.Time) {
	t.Helper()
	rows, err := db.Query(`SELECT version,applied_at FROM node_schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var versions []int
	var appliedAt []time.Time
	for rows.Next() {
		var version int
		var applied time.Time
		if err := rows.Scan(&version, &applied); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
		appliedAt = append(appliedAt, applied)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return versions, appliedAt
}

func assertNodeTableExists(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("table %s count=%d", table, count)
	}
}

func TestStoreOperationIdempotencyAndConflict(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	plan := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: "op-1", TaskID: "task-1", NodeID: "node-1", Kind: "test", PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Minute), Payload: json.RawMessage(`{}`)}
	digest, _ := plan.Digest()
	first, created, err := store.Put(context.Background(), "server-1", digest, plan, now)
	if err != nil || !created {
		t.Fatalf("put: created=%v err=%v", created, err)
	}
	second, created, err := store.Put(context.Background(), "server-1", digest, plan, now)
	if err != nil || created || first.OperationKey != second.OperationKey {
		t.Fatalf("repeat: created=%v err=%v", created, err)
	}
	if _, _, err := store.Put(context.Background(), "server-1", "different", plan, now); !errors.Is(err, ErrPlanConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestStoreLeaseDoesNotRegress(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	plan := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: "op-1", TaskID: "task-1", NodeID: "node-1", Kind: "test", PlanRevision: 1, LeaseEpoch: 2, LeaseExpiresAt: now.Add(time.Minute), Payload: json.RawMessage(`{}`)}
	digest, _ := plan.Digest()
	_, _, _ = store.Put(context.Background(), "server-1", digest, plan, now)
	if _, err := store.RenewLease(context.Background(), "server-1", plan.OperationKey, digest, 1, now.Add(time.Minute), now); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("expected lease conflict, got %v", err)
	}
}
