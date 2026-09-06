package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactReceiptInspectionIsBoundedAndRootConstrained(t *testing.T) {
	root := t.TempDir()
	_, identity, err := canonicalProjectionRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("managed artifact\n")
	path := filepath.Join(root, "movie.nfo")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := inspectArtifactReceiptFile(context.Background(), root, identity, "/movie.nfo")
	want := sha256.Sum256(content)
	if err != nil || !actual.Exists || actual.Size != int64(len(content)) || actual.Fingerprint != hex.EncodeToString(want[:]) {
		t.Fatalf("fingerprint=%+v err=%v", actual, err)
	}
	missing, err := inspectArtifactReceiptFile(context.Background(), root, identity, "/missing.nfo")
	if err != nil || missing.Exists {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
	if _, err := inspectArtifactReceiptFile(context.Background(), root, identity, "../escape.nfo"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err := os.Truncate(path, catalogArtifactReceiptMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectArtifactReceiptFile(context.Background(), root, identity, "/movie.nfo"); err == nil {
		t.Fatal("oversized substituted file accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inspectArtifactReceiptFile(ctx, root, identity, "/missing.nfo"); err == nil {
		t.Fatal("cancelled recovery performed inspection")
	}
}
