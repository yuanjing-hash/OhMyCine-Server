package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
)

// Artifact sources are already limited to 20 MiB. Recovery must not hash a
// substituted video or another unbounded file under an artifact's old name.
const catalogArtifactReceiptMaxBytes int64 = 32 << 20

type artifactPhysicalFingerprint struct {
	Exists      bool
	Fingerprint string
	Size        int64
}

func inspectArtifactReceiptFile(ctx context.Context, root, rootIdentity, relative string) (artifactPhysicalFingerprint, error) {
	var result artifactPhysicalFingerprint
	if err := ctx.Err(); err != nil {
		return result, err
	}
	target, exists, err := safeCleanupTarget(root, rootIdentity, relative)
	if err != nil || !exists {
		return result, err
	}
	before, err := os.Lstat(target)
	if err != nil {
		return result, err
	}
	if !before.Mode().IsRegular() || storagefs.IsReparsePoint(target, before) || before.Size() < 0 || before.Size() > catalogArtifactReceiptMaxBytes {
		return result, ErrCatalogFence
	}
	file, err := os.Open(target)
	if err != nil {
		return result, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return result, ErrCatalogFence
	}
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n, err := file.Read(buffer)
		read += int64(n)
		if read > catalogArtifactReceiptMaxBytes {
			return result, ErrCatalogBudget
		}
		_, _ = hash.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, err
		}
	}
	after, err := file.Stat()
	if err != nil || after.Size() != before.Size() || read != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return result, ErrCatalogFence
	}
	verifiedTarget, exists, err := safeCleanupTarget(root, rootIdentity, relative)
	if err != nil || !exists || verifiedTarget != target {
		return result, ErrCatalogFence
	}
	final, err := os.Lstat(target)
	if err != nil || !os.SameFile(opened, final) || final.Size() != before.Size() || !final.ModTime().Equal(before.ModTime()) {
		return result, ErrCatalogFence
	}
	return artifactPhysicalFingerprint{Exists: true, Fingerprint: hex.EncodeToString(hash.Sum(nil)), Size: read}, nil
}
