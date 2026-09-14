package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestBuildFileExportHashesFixedChunksWithoutPublishingPaths(t *testing.T) {
	managed := t.TempDir()
	downloadRoot := filepath.Join(managed, "downloads", "task-1")
	if err := os.MkdirAll(filepath.Join(downloadRoot, "Movie"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, nodeprotocol.FileChunkSize+3)
	for index := range content {
		content[index] = byte(index % 251)
	}
	filename := filepath.Join(downloadRoot, "Movie", "Movie.mkv")
	if err := os.WriteFile(filename, content, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record, err := buildFileExport(context.Background(), managed, "server-1", "export:task-1", ManagedDownload{TaskID: "task-1", NodeLocalRoot: downloadRoot}, downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie/Movie.mkv", Size: int64(len(content))}}}, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if record.Summary.TotalFiles != 1 || record.Summary.TotalBytes != int64(len(content)) || len(record.Files) != 1 || len(record.Files[0].Chunks) != 2 {
		t.Fatalf("unexpected export: %+v", record)
	}
	if record.Files[0].Public.FileToken == "" || record.Files[0].Public.SHA256 == "" || record.Files[0].Public.RelativePath != "Movie/Movie.mkv" {
		t.Fatalf("unsafe or incomplete public file: %+v", record.Files[0].Public)
	}
	if record.Files[0].Chunks[0].Size != nodeprotocol.FileChunkSize || record.Files[0].Chunks[1].Offset != nodeprotocol.FileChunkSize || record.Files[0].Chunks[1].Size != 3 {
		t.Fatalf("unexpected chunks: %+v", record.Files[0].Chunks)
	}
}

func TestBuildFileExportRejectsTraversalAndSymlinkEscape(t *testing.T) {
	managed := t.TempDir()
	downloadRoot := filepath.Join(managed, "downloads")
	if err := os.MkdirAll(downloadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, relative := range []string{"../secret.mkv", "/secret.mkv", `C:\secret.mkv`} {
		_, err := buildFileExport(context.Background(), managed, "server-1", "export:task-1", ManagedDownload{TaskID: "task-1", NodeLocalRoot: downloadRoot}, downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: relative, Size: 1}}}, now, now.Add(time.Hour))
		if err == nil {
			t.Fatalf("unsafe relative path %q was accepted", relative)
		}
	}
	if runtime.GOOS == "windows" {
		t.Skip("ordinary Windows users may not have symlink permission")
	}
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(downloadRoot, "link.mkv")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := buildFileExport(context.Background(), managed, "server-1", "export:task-2", ManagedDownload{TaskID: "task-2", NodeLocalRoot: downloadRoot}, downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "link.mkv", Size: 1}}}, now, now.Add(time.Hour)); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}
