package nodeagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type StorageSourceRun struct {
	OperationKey    string
	ServerID        string
	TaskID          string
	PlanDigest      string
	StorageID       string
	Status          string
	ProviderTaskID  string
	OutputRootID    string
	ManifestDigest  string
	ManifestReady   bool
	TotalFiles      int64
	TotalBytes      int64
	DownloadedFiles int64
	DownloadedBytes int64
}

type StorageSourceFile struct {
	OperationKey   string
	Ordinal        int64
	ProviderFileID string
	RelativePath   string
	Size           int64
	SourceSHA1     string
	Status         string
	SHA256         string
	ChunkCount     int
}

func (s *Store) PrepareStorageSourceRun(ctx context.Context, serverID string, request nodeprotocol.StorageSourceActionRequest, now time.Time) (StorageSourceRun, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO storage_source_runs(operation_key,paired_server_id,task_id,plan_digest,storage_id,status,created_at,updated_at) VALUES(?,?,?,?,?,'accepted',?,?) ON CONFLICT(operation_key) DO NOTHING`, request.OperationKey, serverID, request.TaskID, request.PlanDigest, request.StorageID, now.UTC(), now.UTC())
	if err != nil {
		return StorageSourceRun{}, err
	}
	run, err := s.StorageSourceRun(ctx, serverID, request.OperationKey)
	if err != nil {
		return StorageSourceRun{}, err
	}
	if run.TaskID != request.TaskID || run.PlanDigest != request.PlanDigest || run.StorageID != request.StorageID {
		return StorageSourceRun{}, ErrPlanConflict
	}
	return run, nil
}

func (s *Store) StorageSourceRun(ctx context.Context, serverID, operationKey string) (StorageSourceRun, error) {
	var result StorageSourceRun
	var ready int
	err := s.db.QueryRowContext(ctx, `SELECT operation_key,paired_server_id,task_id,plan_digest,storage_id,status,provider_task_id,output_root_id,manifest_digest,manifest_ready,total_files,total_bytes,downloaded_files,downloaded_bytes FROM storage_source_runs WHERE operation_key=? AND paired_server_id=?`, operationKey, serverID).Scan(&result.OperationKey, &result.ServerID, &result.TaskID, &result.PlanDigest, &result.StorageID, &result.Status, &result.ProviderTaskID, &result.OutputRootID, &result.ManifestDigest, &ready, &result.TotalFiles, &result.TotalBytes, &result.DownloadedFiles, &result.DownloadedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return StorageSourceRun{}, ErrOperationNotFound
	}
	result.ManifestReady = ready != 0
	return result, err
}

func (s *Store) UpdateStorageSourceRun(ctx context.Context, serverID, operationKey, planDigest, status, providerTaskID, outputRootID string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE storage_source_runs SET status=?,provider_task_id=CASE WHEN ?<>'' THEN ? ELSE provider_task_id END,output_root_id=CASE WHEN ?<>'' THEN ? ELSE output_root_id END,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=?`, status, providerTaskID, providerTaskID, outputRootID, outputRootID, now.UTC(), operationKey, serverID, planDigest)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrPlanConflict
	}
	return nil
}

func (s *Store) ResetStorageSourceManifest(ctx context.Context, serverID, operationKey, planDigest string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var ready int
	if err := tx.QueryRowContext(ctx, `SELECT manifest_ready FROM storage_source_runs WHERE operation_key=? AND paired_server_id=? AND plan_digest=?`, operationKey, serverID, planDigest).Scan(&ready); err != nil {
		return err
	}
	if ready != 0 {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM storage_source_files WHERE operation_key=?`, operationKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE storage_source_runs SET status=?,total_files=0,total_bytes=0,updated_at=? WHERE operation_key=?`, nodeprotocol.StorageSourceEnumerating, now.UTC(), operationKey); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveStorageSourceFileBatch(ctx context.Context, operationKey string, files []StorageSourceFile, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, file := range files {
		if file.OperationKey != operationKey || file.Ordinal < 0 || !validStorageSourceProviderID(file.ProviderFileID) || normalizedExportPath(file.RelativePath) != file.RelativePath || file.Size <= 0 || !validSHA1(file.SourceSHA1) || file.ChunkCount < 1 {
			return errors.New("node_storage_source_manifest_invalid")
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO storage_source_files(operation_key,ordinal,provider_file_id,relative_path,relative_key,size,source_sha1,status,chunk_count,updated_at) VALUES(?,?,?,?,?,?,?,'pending',?,?) ON CONFLICT(operation_key,ordinal) DO NOTHING`, operationKey, file.Ordinal, file.ProviderFileID, file.RelativePath, strings.ToLower(file.RelativePath), file.Size, strings.ToUpper(file.SourceSHA1), file.ChunkCount, now.UTC())
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed == 0 {
			var providerID, relativePath, sha string
			var size int64
			if err := tx.QueryRowContext(ctx, `SELECT provider_file_id,relative_path,size,source_sha1 FROM storage_source_files WHERE operation_key=? AND ordinal=?`, operationKey, file.Ordinal).Scan(&providerID, &relativePath, &size, &sha); err != nil || providerID != file.ProviderFileID || relativePath != file.RelativePath || size != file.Size || !strings.EqualFold(sha, file.SourceSHA1) {
				return ErrPlanConflict
			}
		}
	}
	return tx.Commit()
}

func (s *Store) FinalizeStorageSourceManifest(ctx context.Context, serverID, operationKey, planDigest, manifestDigest string, totalFiles, totalBytes int64, now time.Time) error {
	if !nodeprotocol.ValidDigest(manifestDigest) || totalFiles < 1 || totalBytes < 1 {
		return errors.New("node_storage_source_manifest_invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count, sum, minOrdinal, maxOrdinal int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size),0),COALESCE(MIN(ordinal),0),COALESCE(MAX(ordinal),-1) FROM storage_source_files WHERE operation_key=?`, operationKey).Scan(&count, &sum, &minOrdinal, &maxOrdinal); err != nil {
		return err
	}
	if count != totalFiles || sum != totalBytes || minOrdinal != 0 || maxOrdinal != totalFiles-1 {
		return errors.New("node_storage_source_manifest_incomplete")
	}
	result, err := tx.ExecContext(ctx, `UPDATE storage_source_runs SET status=?,manifest_digest=?,manifest_ready=1,total_files=?,total_bytes=?,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=?`, nodeprotocol.StorageSourceDownloading, manifestDigest, totalFiles, totalBytes, now.UTC(), operationKey, serverID, planDigest)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrPlanConflict
	}
	return tx.Commit()
}

func (s *Store) StorageSourceFiles(ctx context.Context, operationKey string, afterOrdinal int64, limit int) ([]StorageSourceFile, error) {
	if limit < 1 || limit > 250 {
		return nil, errors.New("node_storage_source_page_invalid")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT operation_key,ordinal,provider_file_id,relative_path,size,source_sha1,status,sha256,chunk_count FROM storage_source_files WHERE operation_key=? AND ordinal>? ORDER BY ordinal LIMIT ?`, operationKey, afterOrdinal, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	files := make([]StorageSourceFile, 0, limit)
	for rows.Next() {
		var file StorageSourceFile
		if err := rows.Scan(&file.OperationKey, &file.Ordinal, &file.ProviderFileID, &file.RelativePath, &file.Size, &file.SourceSHA1, &file.Status, &file.SHA256, &file.ChunkCount); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (s *Store) StorageSourceChunks(ctx context.Context, operationKey string, ordinal int64) (map[int]nodeprotocol.FileChunkDigest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chunk_index,chunk_offset,size,sha256 FROM storage_source_chunks WHERE operation_key=? AND file_ordinal=? ORDER BY chunk_index`, operationKey, ordinal)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[int]nodeprotocol.FileChunkDigest)
	for rows.Next() {
		var chunk nodeprotocol.FileChunkDigest
		if err := rows.Scan(&chunk.Index, &chunk.Offset, &chunk.Size, &chunk.SHA256); err != nil {
			return nil, err
		}
		result[chunk.Index] = chunk
	}
	return result, rows.Err()
}

func (s *Store) SaveStorageSourceChunk(ctx context.Context, operationKey string, ordinal int64, chunk nodeprotocol.FileChunkDigest, now time.Time) error {
	if chunk.Index < 0 || chunk.Offset < 0 || chunk.Size < 1 || !nodeprotocol.ValidDigest(chunk.SHA256) {
		return errors.New("node_storage_source_chunk_invalid")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO storage_source_chunks(operation_key,file_ordinal,chunk_index,chunk_offset,size,sha256,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(operation_key,file_ordinal,chunk_index) DO UPDATE SET chunk_offset=excluded.chunk_offset,size=excluded.size,sha256=excluded.sha256,created_at=excluded.created_at`, operationKey, ordinal, chunk.Index, chunk.Offset, chunk.Size, chunk.SHA256, now.UTC())
	return err
}

func (s *Store) DeleteStorageSourceChunk(ctx context.Context, operationKey string, ordinal int64, chunkIndex int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM storage_source_chunks WHERE operation_key=? AND file_ordinal=? AND chunk_index=?`, operationKey, ordinal, chunkIndex)
	return err
}

func (s *Store) CompleteStorageSourceFile(ctx context.Context, serverID, operationKey, planDigest string, ordinal int64, sha256Digest string, now time.Time) error {
	if !nodeprotocol.ValidDigest(sha256Digest) {
		return errors.New("node_storage_source_file_invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT status,size FROM storage_source_files WHERE operation_key=? AND ordinal=?`, operationKey, ordinal).Scan(&status, &size); err != nil {
		return err
	}
	if status != "completed" {
		if _, err := tx.ExecContext(ctx, `UPDATE storage_source_files SET status='completed',sha256=?,updated_at=? WHERE operation_key=? AND ordinal=?`, sha256Digest, now.UTC(), operationKey, ordinal); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE storage_source_runs SET downloaded_files=downloaded_files+1,downloaded_bytes=downloaded_bytes+?,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=?`, size, now.UTC(), operationKey, serverID, planDigest); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE storage_source_files SET sha256=?,updated_at=? WHERE operation_key=? AND ordinal=?`, sha256Digest, now.UTC(), operationKey, ordinal); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteStorageSourceAction(ctx context.Context, serverID, requestID, operationKey, planDigest string, response nodeprotocol.StorageSourceActionResponse, now time.Time) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE storage_source_runs SET status=?,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND manifest_ready=1 AND downloaded_files=total_files AND downloaded_bytes=total_bytes`, nodeprotocol.StorageSourceCompleted, now.UTC(), operationKey, serverID, planDigest)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrPlanConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE provider_receipts SET status='completed',response_json=?,error_code='',updated_at=? WHERE request_id=? AND paired_server_id=? AND operation_key=? AND action=?`, string(raw), now.UTC(), requestID, serverID, operationKey, nodeprotocol.StorageSourceActionMaterialize)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrOperationNotFound
	}
	one := float64(1)
	result, err = tx.ExecContext(ctx, `UPDATE operations SET status=?,phase=?,error_code='',progress=?,revision=revision+1,updated_at=? WHERE operation_key=? AND paired_server_id=? AND plan_digest=? AND status NOT IN (?,?)`, nodeprotocol.OperationCompleted, nodeprotocol.PhaseCompleted, one, now.UTC(), operationKey, serverID, planDigest, nodeprotocol.OperationCompleted, nodeprotocol.OperationCancelled)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrOperationNotFound
	}
	return tx.Commit()
}

func validSHA1(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func validStorageSourceProviderID(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\\/\x00\r\n")
}
