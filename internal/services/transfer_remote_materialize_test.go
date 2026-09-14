package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type fakeRemoteFileReader struct {
	content []byte
	chunks  []downloadpkg.RemoteFileChunk
	mu      sync.Mutex
	reads   []int
}

func (f *fakeRemoteFileReader) RemoteFileChunks(context.Context, downloadpkg.Manifest, downloadpkg.File) ([]downloadpkg.RemoteFileChunk, error) {
	return append([]downloadpkg.RemoteFileChunk(nil), f.chunks...), nil
}

func (f *fakeRemoteFileReader) ReadRemoteFileChunk(_ context.Context, _ downloadpkg.Manifest, _ downloadpkg.File, chunk downloadpkg.RemoteFileChunk) ([]byte, error) {
	f.mu.Lock()
	f.reads = append(f.reads, chunk.Index)
	f.mu.Unlock()
	return append([]byte(nil), f.content[chunk.Offset:chunk.Offset+chunk.Size]...), nil
}

func TestRemoteNodeMaterializationResumesVerifiedChunksAndPersistsCompletion(t *testing.T) {
	queue, _, download, _, _ := transferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictOverwrite, false)
	service := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	manifestSeed := downloadpkg.Manifest{Name: "Movie", Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.mkv", Size: minimumAutomaticTransferVideoBytes}}}
	if err := service.Enqueue(download, manifestSeed); err != nil {
		t.Fatal(err)
	}
	var transfer models.TransferTask
	if err := queue.db.Where("download_task_id = ?", download.ID).First(&transfer).Error; err != nil {
		t.Fatal(err)
	}

	content := make([]byte, nodeprotocol.FileChunkSize+113)
	for index := range content {
		content[index] = byte(index % 251)
	}
	fileSum := sha256.Sum256(content)
	file := downloadpkg.File{RelativePath: "Movie.mkv", Size: int64(len(content)), SHA256: hex.EncodeToString(fileSum[:]), RemoteFileToken: "file:resume-test"}
	chunks := remoteTestChunks(content)
	expires := time.Now().UTC().Add(time.Hour)
	manifest := downloadpkg.Manifest{Name: "Movie", Files: []downloadpkg.File{file}, Complete: true, RemoteExportOperationKey: "qb:file_export:resume-test", RemoteExportDigest: hex.EncodeToString(fileSum[:]), RemoteExportExpiresAt: &expires}
	reader := &fakeRemoteFileReader{content: content, chunks: chunks}
	worker := NewTransferWorker(service)
	checkpoint, err := worker.loadOrCreateRemoteFileCheckpoint(context.Background(), transfer, download, manifest, file, len(chunks))
	if err != nil {
		t.Fatal(err)
	}
	bitmapSet(checkpoint.CompletedBitmap, 0)
	if err := worker.persistRemoteBitmap(context.Background(), &checkpoint); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	destination := filepath.Join(root, file.RelativePath)
	partial := destination + ".partial"
	if err := os.WriteFile(partial, content, 0o600); err != nil {
		t.Fatal(err)
	}
	// Remove the second chunk so only the verified first chunk may be skipped.
	if output, err := os.OpenFile(partial, os.O_WRONLY, 0o600); err != nil {
		t.Fatal(err)
	} else {
		if _, err := output.WriteAt(make([]byte, chunks[1].Size), chunks[1].Offset); err != nil {
			t.Fatal(err)
		}
		_ = output.Close()
	}
	if err := worker.pullRemoteFile(context.Background(), materializeTestRuntime{}, reader, manifest, file, chunks, root, destination, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(reader.reads) != 1 || reader.reads[0] != 1 {
		t.Fatalf("read chunks=%v, want only missing chunk 1", reader.reads)
	}
	if err := verifyRemoteMaterializedFile(destination, file); err != nil {
		t.Fatal(err)
	}
	var saved models.RemoteTransferFile
	if err := queue.db.First(&saved, checkpoint.ID).Error; err != nil || saved.Status != "completed" || !bitmapComplete(saved.CompletedBitmap, len(chunks)) {
		t.Fatalf("saved checkpoint=%+v err=%v", saved, err)
	}
	reader.reads = nil
	if err := worker.pullRemoteFile(context.Background(), materializeTestRuntime{}, reader, manifest, file, chunks, root, destination, &saved); err != nil {
		t.Fatal(err)
	}
	if len(reader.reads) != 0 {
		t.Fatalf("completed file was downloaded again: %v", reader.reads)
	}
}

func TestRemoteNodeMaterializationRedownloadsCorruptCompletedChunk(t *testing.T) {
	content := make([]byte, nodeprotocol.FileChunkSize+7)
	for index := range content {
		content[index] = byte((index + 17) % 253)
	}
	chunks := remoteTestChunks(content)
	bitmap := make([]byte, (len(chunks)+7)/8)
	bitmapSet(bitmap, 0)
	checkpoint := models.RemoteTransferFile{CompletedBitmap: bitmap}
	partial, err := os.CreateTemp(t.TempDir(), "remote-partial-")
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	if err := partial.Truncate(int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := reconcileRemoteBitmap(partial, &checkpoint, chunks); err != nil {
		t.Fatal(err)
	}
	if bitmapHas(checkpoint.CompletedBitmap, 0) {
		t.Fatal("corrupt completed chunk remained trusted")
	}
}

func TestRemoteNodeMaterializationDoesNotTrustMissingCompletedFile(t *testing.T) {
	queue, _, download, _, _ := transferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictOverwrite, false)
	service := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	if err := service.Enqueue(download, downloadpkg.Manifest{Name: "Movie", Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.mkv", Size: minimumAutomaticTransferVideoBytes}}}); err != nil {
		t.Fatal(err)
	}
	var transfer models.TransferTask
	if err := queue.db.Where("download_task_id = ?", download.ID).First(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	content := []byte("verified remote media")
	sum := sha256.Sum256(content)
	file := downloadpkg.File{RelativePath: "Movie.mkv", Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), RemoteFileToken: "file:missing-completed"}
	manifest := downloadpkg.Manifest{Files: []downloadpkg.File{file}}
	checkpoint := models.RemoteTransferFile{TransferTaskID: transfer.ID, DownloadTaskID: download.ID, OperationKey: "qb:file_export:missing", ManifestDigest: strings.Repeat("a", 64), FileToken: file.RemoteFileToken, RelativePath: file.RelativePath, Size: file.Size, SHA256: file.SHA256, ChunkSize: nodeprotocol.FileChunkSize, ChunkCount: 1, CompletedBitmap: []byte{1}, Status: "completed", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}

	remaining, err := remoteMaterializationRemainingBytes(queue.db, checkpoint.TransferTaskID, t.TempDir(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != uint64(len(content)) {
		t.Fatalf("remaining=%d, want %d for a missing completed file", remaining, len(content))
	}
}

func remoteTestChunks(content []byte) []downloadpkg.RemoteFileChunk {
	result := make([]downloadpkg.RemoteFileChunk, 0, (len(content)+int(nodeprotocol.FileChunkSize)-1)/int(nodeprotocol.FileChunkSize))
	for offset := int64(0); offset < int64(len(content)); offset += nodeprotocol.FileChunkSize {
		size := min(nodeprotocol.FileChunkSize, int64(len(content))-offset)
		sum := sha256.Sum256(content[offset : offset+size])
		result = append(result, downloadpkg.RemoteFileChunk{Index: len(result), Offset: offset, Size: size, SHA256: hex.EncodeToString(sum[:])})
	}
	return result
}
