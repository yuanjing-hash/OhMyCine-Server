package nodeagent

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type FileExportRecord struct {
	Summary nodeprotocol.FileExportSummary
	Files   []FileExportRecordFile
}

type FileExportRecordFile struct {
	Public   nodeprotocol.FileExportFile
	NodePath string
	Chunks   []nodeprotocol.FileChunkDigest
}

type FileExportRangeFile struct {
	OperationKey string
	TaskID       string
	FileToken    string
	NodePath     string
	Size         int64
	SHA256       string
	ExpiresAt    time.Time
}

func (s *Store) SaveFileExport(ctx context.Context, serverID string, record FileExportRecord) error {
	if serverID == "" || record.Summary.OperationKey == "" || record.Summary.TaskID == "" || record.Summary.Name == "" || record.Summary.ManifestDigest == "" || record.Summary.ChunkSize != nodeprotocol.FileChunkSize || len(record.Files) != record.Summary.TotalFiles || !record.Summary.ExpiresAt.After(record.Summary.CreatedAt) {
		return errors.New("node_file_export_invalid")
	}
	var totalBytes int64
	for _, file := range record.Files {
		if nodeprotocol.ValidateFileToken(file.Public.FileToken) != nil || file.Public.Size < 0 || file.Public.SHA256 == "" || file.Public.ChunkCount != len(file.Chunks) || file.NodePath == "" || totalBytes > record.Summary.TotalBytes-file.Public.Size {
			return errors.New("node_file_export_invalid")
		}
		totalBytes += file.Public.Size
	}
	if totalBytes != record.Summary.TotalBytes {
		return errors.New("node_file_export_invalid")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var existingServer, existingTask, existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT paired_server_id,task_id,manifest_digest FROM file_exports WHERE operation_key=?`, record.Summary.OperationKey).Scan(&existingServer, &existingTask, &existingDigest)
	if err == nil {
		if existingServer != serverID || existingTask != record.Summary.TaskID || existingDigest != record.Summary.ManifestDigest {
			return ErrPlanConflict
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO file_exports(operation_key,paired_server_id,task_id,name,manifest_digest,total_files,total_bytes,chunk_size,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, record.Summary.OperationKey, serverID, record.Summary.TaskID, record.Summary.Name, record.Summary.ManifestDigest, record.Summary.TotalFiles, record.Summary.TotalBytes, record.Summary.ChunkSize, record.Summary.CreatedAt.UTC(), record.Summary.ExpiresAt.UTC()); err != nil {
		return err
	}
	for ordinal, file := range record.Files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_export_files(operation_key,file_token,ordinal,relative_path,node_path,size,sha256,chunk_count) VALUES(?,?,?,?,?,?,?,?)`, record.Summary.OperationKey, file.Public.FileToken, ordinal, file.Public.RelativePath, file.NodePath, file.Public.Size, file.Public.SHA256, file.Public.ChunkCount); err != nil {
			return err
		}
		for _, chunk := range file.Chunks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO file_export_chunks(operation_key,file_token,chunk_index,chunk_offset,size,sha256) VALUES(?,?,?,?,?,?)`, record.Summary.OperationKey, file.Public.FileToken, chunk.Index, chunk.Offset, chunk.Size, chunk.SHA256); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) FileExportManifest(ctx context.Context, serverID, operationKey string, page, pageSize int, now time.Time) (nodeprotocol.FileExportManifestPage, error) {
	page, pageSize, err := nodeprotocol.NormalizeManifestPage(page, pageSize)
	if err != nil {
		return nodeprotocol.FileExportManifestPage{}, err
	}
	summary, err := s.fileExportSummary(ctx, serverID, operationKey, now)
	if err != nil {
		return nodeprotocol.FileExportManifestPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT file_token,relative_path,size,sha256,chunk_count FROM file_export_files WHERE operation_key=? ORDER BY ordinal LIMIT ? OFFSET ?`, operationKey, pageSize, (page-1)*pageSize)
	if err != nil {
		return nodeprotocol.FileExportManifestPage{}, err
	}
	defer func() { _ = rows.Close() }()
	files := make([]nodeprotocol.FileExportFile, 0, pageSize)
	for rows.Next() {
		var file nodeprotocol.FileExportFile
		if err := rows.Scan(&file.FileToken, &file.RelativePath, &file.Size, &file.SHA256, &file.ChunkCount); err != nil {
			return nodeprotocol.FileExportManifestPage{}, err
		}
		files = append(files, file)
	}
	if err := rows.Err(); err != nil {
		return nodeprotocol.FileExportManifestPage{}, err
	}
	return nodeprotocol.FileExportManifestPage{Summary: summary, Files: files, Page: page, PageSize: pageSize, HasMore: page*pageSize < summary.TotalFiles}, nil
}

// RenewFileExport extends only an already-persisted export owned by the exact
// paired Server and task. DownloaderAction has already authenticated a fresh,
// task-bound grant before this method is reached; no provider call or rehash is
// required. Range reads still verify every persisted chunk immediately before
// returning bytes.
func (s *Store) RenewFileExport(ctx context.Context, serverID, operationKey, taskID string, now, expiresAt time.Time) (nodeprotocol.FileExportSummary, error) {
	if serverID == "" || operationKey == "" || taskID == "" || !expiresAt.After(now) || expiresAt.After(now.Add(fileExportLifetime)) {
		return nodeprotocol.FileExportSummary{}, ErrOperationNotFound
	}
	result, err := s.db.ExecContext(ctx, `UPDATE file_exports SET expires_at=? WHERE operation_key=? AND paired_server_id=? AND task_id=?`, expiresAt.UTC(), operationKey, serverID, taskID)
	if err != nil {
		return nodeprotocol.FileExportSummary{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return nodeprotocol.FileExportSummary{}, ErrOperationNotFound
	}
	return s.fileExportSummary(ctx, serverID, operationKey, now)
}

func (s *Store) FileExportChunks(ctx context.Context, serverID, operationKey, fileToken string, page, pageSize int, now time.Time) (nodeprotocol.FileChunkDigestPage, error) {
	if err := nodeprotocol.ValidateFileToken(fileToken); err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	page, pageSize, err := nodeprotocol.NormalizeChunkPage(page, pageSize)
	if err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	summary, err := s.fileExportSummary(ctx, serverID, operationKey, now)
	if err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	var fileSHA string
	var fileSize int64
	var totalChunks int
	if err := s.db.QueryRowContext(ctx, `SELECT sha256,size,chunk_count FROM file_export_files WHERE operation_key=? AND file_token=?`, operationKey, fileToken).Scan(&fileSHA, &fileSize, &totalChunks); errors.Is(err, sql.ErrNoRows) {
		return nodeprotocol.FileChunkDigestPage{}, ErrOperationNotFound
	} else if err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT chunk_index,chunk_offset,size,sha256 FROM file_export_chunks WHERE operation_key=? AND file_token=? ORDER BY chunk_index LIMIT ? OFFSET ?`, operationKey, fileToken, pageSize, (page-1)*pageSize)
	if err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	defer func() { _ = rows.Close() }()
	chunks := make([]nodeprotocol.FileChunkDigest, 0, pageSize)
	for rows.Next() {
		var chunk nodeprotocol.FileChunkDigest
		if err := rows.Scan(&chunk.Index, &chunk.Offset, &chunk.Size, &chunk.SHA256); err != nil {
			return nodeprotocol.FileChunkDigestPage{}, err
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nodeprotocol.FileChunkDigestPage{}, err
	}
	return nodeprotocol.FileChunkDigestPage{OperationKey: operationKey, TaskID: summary.TaskID, FileToken: fileToken, FileSHA256: fileSHA, FileSize: fileSize, ChunkSize: summary.ChunkSize, TotalChunks: totalChunks, Chunks: chunks, Page: page, PageSize: pageSize, HasMore: page*pageSize < totalChunks}, nil
}

func (s *Store) FileExportRangeFile(ctx context.Context, serverID, operationKey, taskID, fileToken string, now time.Time) (FileExportRangeFile, error) {
	if err := nodeprotocol.ValidateFileToken(fileToken); err != nil || taskID == "" {
		return FileExportRangeFile{}, ErrOperationNotFound
	}
	var result FileExportRangeFile
	err := s.db.QueryRowContext(ctx, `SELECT e.operation_key,e.task_id,f.file_token,f.node_path,f.size,f.sha256,e.expires_at FROM file_exports e JOIN file_export_files f ON f.operation_key=e.operation_key WHERE e.operation_key=? AND e.paired_server_id=? AND e.task_id=? AND f.file_token=?`, operationKey, serverID, taskID, fileToken).Scan(&result.OperationKey, &result.TaskID, &result.FileToken, &result.NodePath, &result.Size, &result.SHA256, &result.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !result.ExpiresAt.After(now) {
		return FileExportRangeFile{}, ErrOperationNotFound
	}
	return result, err
}

func (s *Store) FileExportChunkDigest(ctx context.Context, serverID, operationKey, taskID, fileToken string, offset, size int64, now time.Time) (nodeprotocol.FileChunkDigest, error) {
	if _, err := s.FileExportRangeFile(ctx, serverID, operationKey, taskID, fileToken, now); err != nil {
		return nodeprotocol.FileChunkDigest{}, err
	}
	var result nodeprotocol.FileChunkDigest
	err := s.db.QueryRowContext(ctx, `SELECT chunk_index,chunk_offset,size,sha256 FROM file_export_chunks WHERE operation_key=? AND file_token=? AND chunk_offset=? AND size=?`, operationKey, fileToken, offset, size).Scan(&result.Index, &result.Offset, &result.Size, &result.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nodeprotocol.FileChunkDigest{}, ErrOperationNotFound
	}
	return result, err
}

func (s *Store) fileExportSummary(ctx context.Context, serverID, operationKey string, now time.Time) (nodeprotocol.FileExportSummary, error) {
	var result nodeprotocol.FileExportSummary
	err := s.db.QueryRowContext(ctx, `SELECT operation_key,task_id,name,manifest_digest,total_files,total_bytes,chunk_size,created_at,expires_at FROM file_exports WHERE operation_key=? AND paired_server_id=?`, operationKey, serverID).Scan(&result.OperationKey, &result.TaskID, &result.Name, &result.ManifestDigest, &result.TotalFiles, &result.TotalBytes, &result.ChunkSize, &result.CreatedAt, &result.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !result.ExpiresAt.After(now) {
		return nodeprotocol.FileExportSummary{}, ErrOperationNotFound
	}
	return result, err
}
