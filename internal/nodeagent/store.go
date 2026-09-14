package nodeagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	_ "modernc.org/sqlite"
)

var (
	ErrOperationNotFound = errors.New("node_operation_not_found")
	ErrPlanConflict      = errors.New(nodeprotocol.ErrorPlanConflict)
	ErrLeaseConflict     = errors.New("node_lease_conflict")
)

type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("node database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create node database directory: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open node database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS node_schema_migrations (version INTEGER PRIMARY KEY, applied_at DATETIME NOT NULL)`); err != nil {
		return fmt.Errorf("bootstrap node migrations: %w", err)
	}
	migrations := []struct {
		version    int
		statements []string
	}{
		{1, []string{
			`CREATE TABLE operations (operation_key TEXT PRIMARY KEY, paired_server_id TEXT NOT NULL, plan_digest TEXT NOT NULL, plan_json TEXT NOT NULL, kind TEXT NOT NULL, task_id TEXT NOT NULL, status TEXT NOT NULL, phase TEXT NOT NULL, lease_epoch INTEGER NOT NULL, lease_expires_at DATETIME NOT NULL, progress REAL, error_code TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, UNIQUE(paired_server_id, operation_key))`,
			`CREATE INDEX idx_node_operations_status_lease ON operations(status, lease_expires_at)`,
			`CREATE TABLE paired_identity (id INTEGER PRIMARY KEY CHECK(id=1), server_id TEXT NOT NULL, server_certificate_fingerprint TEXT NOT NULL, node_certificate_fingerprint TEXT NOT NULL, paired_at DATETIME NOT NULL, revocation_epoch INTEGER NOT NULL DEFAULT 1)`,
			`CREATE TABLE enrollment_challenges (nonce TEXT PRIMARY KEY, server_id TEXT NOT NULL, expires_at DATETIME NOT NULL, consumed_at DATETIME)`,
			`CREATE INDEX idx_node_enrollment_challenges_expiry ON enrollment_challenges(expires_at)`,
		}},
		{2, []string{
			`CREATE TABLE credential_grants (grant_id TEXT PRIMARY KEY, paired_server_id TEXT NOT NULL, task_id TEXT NOT NULL, operation_key TEXT NOT NULL, resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL, credential_revision INTEGER NOT NULL, envelope_json TEXT NOT NULL, expires_at DATETIME NOT NULL, created_at DATETIME NOT NULL, UNIQUE(paired_server_id, grant_id))`,
			`CREATE INDEX idx_node_credential_grants_expiry ON credential_grants(expires_at)`,
			`CREATE TABLE provider_receipts (request_id TEXT PRIMARY KEY, paired_server_id TEXT NOT NULL, operation_key TEXT NOT NULL, request_digest TEXT NOT NULL, action TEXT NOT NULL, status TEXT NOT NULL, response_json TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, UNIQUE(paired_server_id, request_id))`,
			`CREATE INDEX idx_node_provider_receipts_operation ON provider_receipts(paired_server_id, operation_key)`,
			`CREATE TABLE managed_downloads (id INTEGER PRIMARY KEY AUTOINCREMENT, paired_server_id TEXT NOT NULL, downloader_id TEXT NOT NULL, task_id TEXT NOT NULL, provider_task_id TEXT NOT NULL, tag TEXT NOT NULL DEFAULT '', downloader_save_path TEXT NOT NULL, node_local_root TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, UNIQUE(paired_server_id, downloader_id, provider_task_id))`,
			`CREATE INDEX idx_node_managed_downloads_task ON managed_downloads(paired_server_id,task_id)`,
		}},
		{3, []string{
			`CREATE TABLE file_exports (operation_key TEXT PRIMARY KEY, paired_server_id TEXT NOT NULL, task_id TEXT NOT NULL, name TEXT NOT NULL, manifest_digest TEXT NOT NULL, total_files INTEGER NOT NULL, total_bytes INTEGER NOT NULL, chunk_size INTEGER NOT NULL, created_at DATETIME NOT NULL, expires_at DATETIME NOT NULL, UNIQUE(paired_server_id, operation_key))`,
			`CREATE INDEX idx_node_file_exports_task ON file_exports(paired_server_id,task_id,expires_at)`,
			`CREATE TABLE file_export_files (operation_key TEXT NOT NULL REFERENCES file_exports(operation_key) ON DELETE CASCADE, file_token TEXT NOT NULL UNIQUE, ordinal INTEGER NOT NULL, relative_path TEXT NOT NULL, node_path TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, chunk_count INTEGER NOT NULL, PRIMARY KEY(operation_key,file_token), UNIQUE(operation_key,ordinal))`,
			`CREATE TABLE file_export_chunks (operation_key TEXT NOT NULL, file_token TEXT NOT NULL, chunk_index INTEGER NOT NULL, chunk_offset INTEGER NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, PRIMARY KEY(operation_key,file_token,chunk_index), FOREIGN KEY(operation_key,file_token) REFERENCES file_export_files(operation_key,file_token) ON DELETE CASCADE)`,
			`CREATE INDEX idx_node_file_export_chunks_page ON file_export_chunks(operation_key,file_token,chunk_index)`,
		}},
		{4, []string{
			`CREATE TABLE storage_upload_files (operation_key TEXT NOT NULL REFERENCES operations(operation_key) ON DELETE CASCADE, source_file_token TEXT NOT NULL, plan_digest TEXT NOT NULL, status TEXT NOT NULL, size INTEGER NOT NULL, target_parent_id TEXT NOT NULL DEFAULT '', target_item_id TEXT NOT NULL DEFAULT '', target_sha1 TEXT NOT NULL DEFAULT '', updated_at DATETIME NOT NULL, PRIMARY KEY(operation_key,source_file_token))`,
			`CREATE INDEX idx_node_storage_upload_status ON storage_upload_files(operation_key,status)`,
		}},
		{5, []string{
			`CREATE TABLE storage_source_runs (operation_key TEXT PRIMARY KEY REFERENCES operations(operation_key) ON DELETE CASCADE, paired_server_id TEXT NOT NULL, task_id TEXT NOT NULL, plan_digest TEXT NOT NULL, storage_id TEXT NOT NULL, status TEXT NOT NULL, provider_task_id TEXT NOT NULL DEFAULT '', output_root_id TEXT NOT NULL DEFAULT '', manifest_digest TEXT NOT NULL DEFAULT '', manifest_ready INTEGER NOT NULL DEFAULT 0, total_files INTEGER NOT NULL DEFAULT 0, total_bytes INTEGER NOT NULL DEFAULT 0, downloaded_files INTEGER NOT NULL DEFAULT 0, downloaded_bytes INTEGER NOT NULL DEFAULT 0, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, UNIQUE(paired_server_id,operation_key))`,
			`CREATE INDEX idx_node_storage_source_runs_status ON storage_source_runs(paired_server_id,status,updated_at)`,
			`CREATE TABLE storage_source_files (operation_key TEXT NOT NULL REFERENCES storage_source_runs(operation_key) ON DELETE CASCADE, ordinal INTEGER NOT NULL, provider_file_id TEXT NOT NULL, relative_path TEXT NOT NULL, relative_key TEXT NOT NULL, size INTEGER NOT NULL, source_sha1 TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', sha256 TEXT NOT NULL DEFAULT '', chunk_count INTEGER NOT NULL, updated_at DATETIME NOT NULL, PRIMARY KEY(operation_key,ordinal), UNIQUE(operation_key,provider_file_id), UNIQUE(operation_key,relative_key))`,
			`CREATE INDEX idx_node_storage_source_files_status ON storage_source_files(operation_key,status,ordinal)`,
			`CREATE TABLE storage_source_chunks (operation_key TEXT NOT NULL, file_ordinal INTEGER NOT NULL, chunk_index INTEGER NOT NULL, chunk_offset INTEGER NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL, created_at DATETIME NOT NULL, PRIMARY KEY(operation_key,file_ordinal,chunk_index), FOREIGN KEY(operation_key,file_ordinal) REFERENCES storage_source_files(operation_key,ordinal) ON DELETE CASCADE)`,
			`CREATE INDEX idx_node_storage_source_chunks_file ON storage_source_chunks(operation_key,file_ordinal,chunk_index)`,
		}},
		{6, []string{
			`ALTER TABLE storage_source_runs ADD COLUMN cleanup_status TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE storage_source_runs ADD COLUMN cleaned_at DATETIME`,
		}},
		{7, []string{
			`CREATE TABLE http_request_nonces (nonce TEXT PRIMARY KEY, expires_at DATETIME NOT NULL)`,
			`CREATE INDEX idx_http_request_nonces_expiry ON http_request_nonces(expires_at)`,
		}},
	}
	for _, migration := range migrations {
		var applied int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_schema_migrations WHERE version=?`, migration.version).Scan(&applied); err != nil {
			return fmt.Errorf("read node migration %d: %w", migration.version, err)
		}
		if applied != 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply node migration %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_schema_migrations(version,applied_at) VALUES(?,?)`, migration.version, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record node migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit node migration %d: %w", migration.version, err)
		}
	}
	return nil
}

func (s *Store) SaveCredentialGrant(ctx context.Context, serverID string, envelope nodeprotocol.CredentialGrantEnvelope, now time.Time) error {
	if strings.TrimSpace(serverID) == "" || !envelope.ExpiresAt.After(now) {
		return errors.New(nodeprotocol.ErrorCredentialExpired)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO credential_grants(grant_id,paired_server_id,task_id,operation_key,resource_kind,resource_id,credential_revision,envelope_json,expires_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(grant_id) DO UPDATE SET envelope_json=excluded.envelope_json,credential_revision=excluded.credential_revision,expires_at=excluded.expires_at
		WHERE credential_grants.paired_server_id=excluded.paired_server_id
		AND credential_grants.task_id=excluded.task_id AND credential_grants.operation_key=excluded.operation_key
		AND credential_grants.resource_kind=excluded.resource_kind AND credential_grants.resource_id=excluded.resource_id`,
		envelope.GrantID, serverID, envelope.TaskID, envelope.OperationKey, envelope.ResourceKind, envelope.ResourceID, envelope.CredentialRevision, string(raw), envelope.ExpiresAt.UTC(), now.UTC())
	return err
}

func (s *Store) CredentialGrant(ctx context.Context, serverID, grantID string, now time.Time) (nodeprotocol.CredentialGrantEnvelope, error) {
	var raw string
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `SELECT envelope_json,expires_at FROM credential_grants WHERE grant_id=? AND paired_server_id=?`, grantID, serverID).Scan(&raw, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nodeprotocol.CredentialGrantEnvelope{}, errors.New(nodeprotocol.ErrorCredentialExpired)
	}
	if err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, err
	}
	if !expiresAt.After(now) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM credential_grants WHERE grant_id=? AND paired_server_id=?`, grantID, serverID)
		return nodeprotocol.CredentialGrantEnvelope{}, errors.New(nodeprotocol.ErrorCredentialExpired)
	}
	var envelope nodeprotocol.CredentialGrantEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nodeprotocol.CredentialGrantEnvelope{}, errors.New("node_credential_envelope_invalid")
	}
	return envelope, nil
}

type ProviderReceipt struct {
	RequestID     string
	ServerID      string
	OperationKey  string
	RequestDigest string
	Action        string
	Status        string
	ResponseJSON  string
	ErrorCode     string
}

func (s *Store) ProviderReceipt(ctx context.Context, serverID, requestID string) (ProviderReceipt, error) {
	var receipt ProviderReceipt
	err := s.db.QueryRowContext(ctx, `SELECT request_id,paired_server_id,operation_key,request_digest,action,status,response_json,error_code FROM provider_receipts WHERE request_id=? AND paired_server_id=?`, requestID, serverID).
		Scan(&receipt.RequestID, &receipt.ServerID, &receipt.OperationKey, &receipt.RequestDigest, &receipt.Action, &receipt.Status, &receipt.ResponseJSON, &receipt.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderReceipt{}, ErrOperationNotFound
	}
	return receipt, err
}

func (s *Store) BeginProviderReceipt(ctx context.Context, serverID string, request nodeprotocol.DownloaderActionRequest, digest string, now time.Time) (ProviderReceipt, bool, error) {
	return s.BeginActionReceipt(ctx, serverID, request.RequestID, request.OperationKey, request.Action, digest, now)
}

func (s *Store) BeginActionReceipt(ctx context.Context, serverID, requestID, operationKey, action, digest string, now time.Time) (ProviderReceipt, bool, error) {
	existing, err := s.ProviderReceipt(ctx, serverID, requestID)
	if err == nil {
		if existing.RequestDigest != digest || existing.OperationKey != operationKey || existing.Action != action {
			return ProviderReceipt{}, false, ErrPlanConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrOperationNotFound) {
		return ProviderReceipt{}, false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO provider_receipts(request_id,paired_server_id,operation_key,request_digest,action,status,created_at,updated_at) VALUES(?,?,?,?,?,'started',?,?)`, requestID, serverID, operationKey, digest, action, now.UTC(), now.UTC())
	if err != nil {
		if existing, getErr := s.ProviderReceipt(ctx, serverID, requestID); getErr == nil && existing.RequestDigest == digest && existing.OperationKey == operationKey && existing.Action == action {
			return existing, false, nil
		}
		return ProviderReceipt{}, false, err
	}
	return ProviderReceipt{RequestID: requestID, ServerID: serverID, OperationKey: operationKey, RequestDigest: digest, Action: action, Status: "started"}, true, nil
}

func (s *Store) CompleteProviderReceipt(ctx context.Context, serverID, requestID string, response nodeprotocol.DownloaderActionResponse, errorCode string, now time.Time) error {
	return s.CompleteActionReceipt(ctx, serverID, requestID, response, errorCode, now)
}

func (s *Store) CompleteActionReceipt(ctx context.Context, serverID, requestID string, response any, errorCode string, now time.Time) error {
	raw := ""
	status := "failed"
	if errorCode == "" {
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		raw, status = string(encoded), "completed"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE provider_receipts SET status=?,response_json=?,error_code=?,updated_at=? WHERE request_id=? AND paired_server_id=?`, status, raw, errorCode, now.UTC(), requestID, serverID)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrOperationNotFound
	}
	return nil
}

func (s *Store) UpdateActionReceipt(ctx context.Context, serverID, requestID string, response any, now time.Time) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE provider_receipts SET status='started',response_json=?,error_code='',updated_at=? WHERE request_id=? AND paired_server_id=?`, string(raw), now.UTC(), requestID, serverID)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrOperationNotFound
	}
	return nil
}

func (s *Store) RefreshCompletedActionReceipt(ctx context.Context, serverID, requestID, operationKey string, response any, now time.Time) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE provider_receipts SET response_json=?,updated_at=? WHERE request_id=? AND paired_server_id=? AND operation_key=? AND status='completed'`, string(raw), now.UTC(), requestID, serverID, operationKey)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrOperationNotFound
	}
	return nil
}

func (s *Store) CompleteStorageAction(ctx context.Context, serverID, requestID, operationKey, planDigest string, response nodeprotocol.StorageActionResponse, now time.Time) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	receiptResult, err := tx.ExecContext(ctx, `UPDATE provider_receipts SET status='completed',response_json=?,error_code='',updated_at=? WHERE request_id=? AND paired_server_id=? AND operation_key=? AND request_digest<>''`, string(raw), now.UTC(), requestID, serverID, operationKey)
	if err != nil {
		return err
	}
	if changed, _ := receiptResult.RowsAffected(); changed != 1 {
		return ErrOperationNotFound
	}
	one := float64(1)
	operationResult, err := tx.ExecContext(ctx, `UPDATE operations SET status=?,phase=?,error_code='',progress=?,revision=revision+1,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND status NOT IN (?,?)`, nodeprotocol.OperationCompleted, nodeprotocol.PhaseCompleted, one, now.UTC(), operationKey, serverID, planDigest, nodeprotocol.OperationCompleted, nodeprotocol.OperationCancelled)
	if err != nil {
		return err
	}
	if changed, _ := operationResult.RowsAffected(); changed != 1 {
		return ErrOperationNotFound
	}
	return tx.Commit()
}

type ManagedDownload struct {
	ServerID           string
	DownloaderID       string
	TaskID             string
	ProviderTaskID     string
	Tag                string
	DownloaderSavePath string
	NodeLocalRoot      string
}

func (s *Store) SaveManagedDownload(ctx context.Context, download ManagedDownload, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO managed_downloads(paired_server_id,downloader_id,task_id,provider_task_id,tag,downloader_save_path,node_local_root,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(paired_server_id,downloader_id,provider_task_id) DO UPDATE SET task_id=excluded.task_id,tag=excluded.tag,downloader_save_path=excluded.downloader_save_path,node_local_root=excluded.node_local_root,updated_at=excluded.updated_at`,
		download.ServerID, download.DownloaderID, download.TaskID, download.ProviderTaskID, download.Tag, download.DownloaderSavePath, download.NodeLocalRoot, now.UTC(), now.UTC())
	return err
}

func (s *Store) AliasManagedDownload(ctx context.Context, serverID, downloaderID, oldProviderID, newProviderID string, now time.Time) error {
	download, err := s.ManagedDownload(ctx, serverID, downloaderID, oldProviderID)
	if err != nil {
		return err
	}
	download.ProviderTaskID = newProviderID
	return s.SaveManagedDownload(ctx, download, now)
}

func (s *Store) ManagedDownload(ctx context.Context, serverID, downloaderID, providerTaskID string) (ManagedDownload, error) {
	var download ManagedDownload
	err := s.db.QueryRowContext(ctx, `SELECT paired_server_id,downloader_id,task_id,provider_task_id,tag,downloader_save_path,node_local_root FROM managed_downloads WHERE paired_server_id=? AND downloader_id=? AND provider_task_id=?`, serverID, downloaderID, providerTaskID).
		Scan(&download.ServerID, &download.DownloaderID, &download.TaskID, &download.ProviderTaskID, &download.Tag, &download.DownloaderSavePath, &download.NodeLocalRoot)
	if errors.Is(err, sql.ErrNoRows) {
		return ManagedDownload{}, ErrOperationNotFound
	}
	return download, err
}

type PairedIdentity struct {
	ServerID                     string
	ServerCertificateFingerprint string
	NodeCertificateFingerprint   string
	PairedAt                     time.Time
}

func (s *Store) Identity(ctx context.Context) (PairedIdentity, error) {
	var identity PairedIdentity
	err := s.db.QueryRowContext(ctx, `SELECT server_id,server_certificate_fingerprint,node_certificate_fingerprint,paired_at FROM paired_identity WHERE id=1`).Scan(&identity.ServerID, &identity.ServerCertificateFingerprint, &identity.NodeCertificateFingerprint, &identity.PairedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PairedIdentity{}, ErrOperationNotFound
	}
	return identity, err
}

func (s *Store) SaveChallenge(ctx context.Context, nonce, serverID string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO enrollment_challenges(nonce,server_id,expires_at) VALUES(?,?,?) ON CONFLICT(nonce) DO NOTHING`, nonce, serverID, expiresAt.UTC())
	return err
}

func (s *Store) CompleteEnrollment(ctx context.Context, nonce, serverID, serverFingerprint, nodeFingerprint string, now time.Time) (PairedIdentity, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PairedIdentity{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var expiresAt time.Time
	var consumed sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT expires_at,consumed_at FROM enrollment_challenges WHERE nonce=? AND server_id=?`, nonce, serverID).Scan(&expiresAt, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return PairedIdentity{}, errors.New("node_enrollment_challenge_invalid")
	}
	if err != nil {
		return PairedIdentity{}, err
	}
	if consumed.Valid || !expiresAt.After(now) {
		return PairedIdentity{}, errors.New("node_enrollment_challenge_expired")
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM paired_identity`).Scan(&count); err != nil {
		return PairedIdentity{}, err
	}
	if count != 0 {
		return PairedIdentity{}, errors.New("node_already_paired")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO paired_identity(id,server_id,server_certificate_fingerprint,node_certificate_fingerprint,paired_at,revocation_epoch) VALUES(1,?,?,?,?,1)`, serverID, serverFingerprint, nodeFingerprint, now.UTC()); err != nil {
		return PairedIdentity{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enrollment_challenges SET consumed_at=? WHERE nonce=? AND consumed_at IS NULL`, now.UTC(), nonce); err != nil {
		return PairedIdentity{}, err
	}
	if err := tx.Commit(); err != nil {
		return PairedIdentity{}, err
	}
	return PairedIdentity{ServerID: serverID, ServerCertificateFingerprint: serverFingerprint, NodeCertificateFingerprint: nodeFingerprint, PairedAt: now.UTC()}, nil
}

func (s *Store) Put(ctx context.Context, serverID, digest string, plan nodeprotocol.OperationPlan, now time.Time) (nodeprotocol.OperationResponse, bool, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return nodeprotocol.OperationResponse{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nodeprotocol.OperationResponse{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	existing, err := getOperation(ctx, tx, plan.OperationKey)
	if err == nil {
		if existing.PlanDigest != digest || existing.ServerID != serverID {
			return nodeprotocol.OperationResponse{}, false, ErrPlanConflict
		}
		return existing.Response, false, tx.Commit()
	}
	if !errors.Is(err, ErrOperationNotFound) {
		return nodeprotocol.OperationResponse{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operations(operation_key,paired_server_id,plan_digest,plan_json,kind,task_id,status,phase,lease_epoch,lease_expires_at,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, plan.OperationKey, serverID, digest, string(encoded), plan.Kind, plan.TaskID, nodeprotocol.OperationPending, nodeprotocol.PhaseAccepted, plan.LeaseEpoch, plan.LeaseExpiresAt.UTC(), 1, now.UTC(), now.UTC())
	if err != nil {
		return nodeprotocol.OperationResponse{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return nodeprotocol.OperationResponse{}, false, err
	}
	result, err := s.Get(ctx, serverID, plan.OperationKey)
	return result, true, err
}

func (s *Store) Get(ctx context.Context, serverID, key string) (nodeprotocol.OperationResponse, error) {
	record, err := getOperation(ctx, s.db, key)
	if err == nil && record.ServerID != serverID {
		return nodeprotocol.OperationResponse{}, ErrOperationNotFound
	}
	return record.Response, err
}

func (s *Store) OperationTaskMatches(ctx context.Context, serverID, key, taskID string) (bool, error) {
	var storedTask string
	err := s.db.QueryRowContext(ctx, `SELECT task_id FROM operations WHERE operation_key=? AND paired_server_id=?`, key, serverID).Scan(&storedTask)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrOperationNotFound
	}
	return err == nil && storedTask == taskID, err
}

// ActiveOperationPlan returns the exact immutable plan only while its current
// persisted lease is valid. Callers use it immediately before starting each
// privileged provider mutation instead of trusting a previously decoded plan.
func (s *Store) ActiveOperationPlan(ctx context.Context, serverID, key, digest string, now time.Time) (nodeprotocol.OperationPlan, error) {
	var raw, storedDigest, storedServer, status string
	var leaseExpiresAt time.Time
	err := s.db.QueryRowContext(ctx, `SELECT paired_server_id,plan_digest,plan_json,status,lease_expires_at FROM operations WHERE operation_key=?`, key).Scan(&storedServer, &storedDigest, &raw, &status, &leaseExpiresAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && storedServer != serverID {
		return nodeprotocol.OperationPlan{}, ErrOperationNotFound
	}
	if err != nil {
		return nodeprotocol.OperationPlan{}, err
	}
	if storedDigest != digest {
		return nodeprotocol.OperationPlan{}, ErrPlanConflict
	}
	if !leaseExpiresAt.After(now) || status == nodeprotocol.OperationCompleted || status == nodeprotocol.OperationFailed || status == nodeprotocol.OperationCancelled {
		return nodeprotocol.OperationPlan{}, ErrLeaseConflict
	}
	var plan nodeprotocol.OperationPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return nodeprotocol.OperationPlan{}, errors.New("node_operation_plan_invalid")
	}
	return plan, nil
}

func (s *Store) UpdateOperationExecution(ctx context.Context, serverID, key, digest, status, phase, errorCode string, progress *float64, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET status=?,phase=?,error_code=?,progress=?,revision=revision+1,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND status NOT IN (?,?)`, status, phase, errorCode, progress, now.UTC(), key, serverID, digest, nodeprotocol.OperationCompleted, nodeprotocol.OperationCancelled)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrOperationNotFound
	}
	return nil
}

type StorageUploadCheckpoint struct {
	OperationKey    string
	SourceFileToken string
	PlanDigest      string
	Status          string
	Size            int64
	TargetParentID  string
	TargetItemID    string
	TargetSHA1      string
}

func (s *Store) StorageUploadCheckpoint(ctx context.Context, operationKey, sourceFileToken string) (StorageUploadCheckpoint, error) {
	var result StorageUploadCheckpoint
	err := s.db.QueryRowContext(ctx, `SELECT operation_key,source_file_token,plan_digest,status,size,target_parent_id,target_item_id,target_sha1 FROM storage_upload_files WHERE operation_key=? AND source_file_token=?`, operationKey, sourceFileToken).Scan(&result.OperationKey, &result.SourceFileToken, &result.PlanDigest, &result.Status, &result.Size, &result.TargetParentID, &result.TargetItemID, &result.TargetSHA1)
	if errors.Is(err, sql.ErrNoRows) {
		return StorageUploadCheckpoint{}, ErrOperationNotFound
	}
	return result, err
}

func (s *Store) PrepareStorageUploadFile(ctx context.Context, operationKey, sourceFileToken, planDigest string, size int64, now time.Time) (StorageUploadCheckpoint, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO storage_upload_files(operation_key,source_file_token,plan_digest,status,size,updated_at) VALUES(?,?,?,'pending',?,?) ON CONFLICT(operation_key,source_file_token) DO NOTHING`, operationKey, sourceFileToken, planDigest, size, now.UTC())
	if err != nil {
		return StorageUploadCheckpoint{}, err
	}
	checkpoint, err := s.StorageUploadCheckpoint(ctx, operationKey, sourceFileToken)
	if err != nil {
		return StorageUploadCheckpoint{}, err
	}
	if checkpoint.PlanDigest != planDigest || checkpoint.Size != size {
		return StorageUploadCheckpoint{}, ErrPlanConflict
	}
	return checkpoint, nil
}

func (s *Store) UpdateStorageUploadFile(ctx context.Context, operationKey, sourceFileToken, planDigest, status, parentID, itemID, sha1 string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE storage_upload_files SET status=?,target_parent_id=?,target_item_id=?,target_sha1=?,updated_at=? WHERE operation_key=? AND source_file_token=? AND plan_digest=?`, status, parentID, itemID, strings.ToUpper(strings.TrimSpace(sha1)), now.UTC(), operationKey, sourceFileToken, planDigest)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrPlanConflict
	}
	return nil
}

func (s *Store) RenewLease(ctx context.Context, serverID, key, digest string, epoch uint64, expiresAt, now time.Time) (nodeprotocol.OperationResponse, error) {
	if epoch == 0 || expiresAt.IsZero() || expiresAt.After(now.Add(15*time.Minute)) {
		return nodeprotocol.OperationResponse{}, ErrLeaseConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET lease_epoch=?,lease_expires_at=?,revision=revision+1,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND lease_epoch<=? AND status NOT IN (?,?,?)`, epoch, expiresAt.UTC(), now.UTC(), key, serverID, digest, epoch, nodeprotocol.OperationCompleted, nodeprotocol.OperationFailed, nodeprotocol.OperationCancelled)
	if err != nil {
		return nodeprotocol.OperationResponse{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return nodeprotocol.OperationResponse{}, ErrLeaseConflict
	}
	return s.Get(ctx, serverID, key)
}

func (s *Store) Cancel(ctx context.Context, serverID, key string, now time.Time) (nodeprotocol.OperationResponse, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE operations SET status=?,phase=?,revision=revision+1,updated_at=? WHERE operation_key=? AND paired_server_id=? AND status NOT IN (?,?,?)`, nodeprotocol.OperationCancelled, nodeprotocol.OperationCancelled, now.UTC(), key, serverID, nodeprotocol.OperationCompleted, nodeprotocol.OperationFailed, nodeprotocol.OperationCancelled)
	if err != nil {
		return nodeprotocol.OperationResponse{}, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		if _, getErr := s.Get(ctx, serverID, key); getErr != nil {
			return nodeprotocol.OperationResponse{}, getErr
		}
	}
	return s.Get(ctx, serverID, key)
}

type operationRecord struct {
	ServerID   string
	PlanDigest string
	Response   nodeprotocol.OperationResponse
}
type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getOperation(ctx context.Context, db rowQueryer, key string) (operationRecord, error) {
	var record operationRecord
	var progress sql.NullFloat64
	err := db.QueryRowContext(ctx, `SELECT paired_server_id,operation_key,plan_digest,status,phase,lease_epoch,lease_expires_at,progress,error_code,revision,created_at,updated_at FROM operations WHERE operation_key=?`, key).Scan(&record.ServerID, &record.Response.OperationKey, &record.PlanDigest, &record.Response.Status, &record.Response.Phase, &record.Response.LeaseEpoch, &record.Response.LeaseExpiresAt, &progress, &record.Response.ErrorCode, &record.Response.Revision, &record.Response.CreatedAt, &record.Response.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return operationRecord{}, ErrOperationNotFound
	}
	if err != nil {
		return operationRecord{}, err
	}
	record.Response.PlanDigest = record.PlanDigest
	if progress.Valid {
		record.Response.Progress = &progress.Float64
	}
	return record, nil
}
