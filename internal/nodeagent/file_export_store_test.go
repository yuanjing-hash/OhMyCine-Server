package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestStoreFileExportIsPagedTaskBoundAndIdempotent(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := FileExportRecord{
		Summary: nodeprotocol.FileExportSummary{OperationKey: "export:task-1", TaskID: "task-1", Name: "Movie", ManifestDigest: strings.Repeat("a", 64), TotalFiles: 1, TotalBytes: 3, ChunkSize: nodeprotocol.FileChunkSize, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		Files: []FileExportRecordFile{{
			Public:   nodeprotocol.FileExportFile{FileToken: "file:opaque-1", RelativePath: "Movie/Movie.mkv", Size: 3, SHA256: strings.Repeat("b", 64), ChunkCount: 1},
			NodePath: filepath.Join(t.TempDir(), "private", "Movie.mkv"),
			Chunks:   []nodeprotocol.FileChunkDigest{{Index: 0, Offset: 0, Size: 3, SHA256: strings.Repeat("c", 64)}},
		}},
	}
	if err := store.SaveFileExport(context.Background(), "server-1", record); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFileExport(context.Background(), "server-1", record); err != nil {
		t.Fatalf("idempotent save failed: %v", err)
	}
	conflict := record
	conflict.Summary.ManifestDigest = strings.Repeat("d", 64)
	if err := store.SaveFileExport(context.Background(), "server-1", conflict); !errors.Is(err, ErrPlanConflict) {
		t.Fatalf("changed export reused operation key: %v", err)
	}

	page, err := store.FileExportManifest(context.Background(), "server-1", record.Summary.OperationKey, 1, 100, now)
	if err != nil || len(page.Files) != 1 || page.Files[0].RelativePath != "Movie/Movie.mkv" || page.HasMore {
		t.Fatalf("manifest page=%+v err=%v", page, err)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "node_path") {
		t.Fatalf("public manifest leaked node path: %s", raw)
	}
	chunks, err := store.FileExportChunks(context.Background(), "server-1", record.Summary.OperationKey, "file:opaque-1", 1, 10, now)
	if err != nil || len(chunks.Chunks) != 1 || chunks.Chunks[0].Offset != 0 {
		t.Fatalf("chunk page=%+v err=%v", chunks, err)
	}
	file, err := store.FileExportRangeFile(context.Background(), "server-1", record.Summary.OperationKey, "task-1", "file:opaque-1", now)
	if err != nil || file.NodePath == "" {
		t.Fatalf("range file=%+v err=%v", file, err)
	}
	if _, err := store.FileExportRangeFile(context.Background(), "server-1", record.Summary.OperationKey, "another-task", "file:opaque-1", now); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("cross-task read was not hidden: %v", err)
	}
	if _, err := store.FileExportManifest(context.Background(), "server-1", record.Summary.OperationKey, 1, 100, record.Summary.ExpiresAt); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("expired export remained readable: %v", err)
	}
	if _, err := store.RenewFileExport(context.Background(), "server-1", record.Summary.OperationKey, "another-task", record.Summary.ExpiresAt, record.Summary.ExpiresAt.Add(time.Hour)); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("cross-task renewal was not hidden: %v", err)
	}
	renewed, err := store.RenewFileExport(context.Background(), "server-1", record.Summary.OperationKey, "task-1", record.Summary.ExpiresAt, record.Summary.ExpiresAt.Add(time.Hour))
	if err != nil || !renewed.ExpiresAt.Equal(record.Summary.ExpiresAt.Add(time.Hour)) {
		t.Fatalf("renewed export=%+v err=%v", renewed, err)
	}
	if page, err := store.FileExportManifest(context.Background(), "server-1", record.Summary.OperationKey, 1, 100, record.Summary.ExpiresAt); err != nil || len(page.Files) != 1 {
		t.Fatalf("renewed manifest page=%+v err=%v", page, err)
	}
}
