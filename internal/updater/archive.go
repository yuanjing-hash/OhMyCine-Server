package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const (
	maxArchiveEntries     = 128
	maxExpandedArchive    = int64(768 << 20)
	archiveCopyBufferSize = 128 << 10
)

// ExtractCandidate validates the complete archive shape and writes only the
// expected Server binary to candidatePath.
func ExtractCandidate(archivePath, candidatePath string, names PlatformAssets) error {
	if names.TopLevel == "" || names.Binary == "" || names.Archive == "" {
		return coded(CodeArchiveInvalid, errors.New("archive identity is incomplete"))
	}
	if strings.HasSuffix(names.Archive, ".zip") {
		return extractZipCandidate(archivePath, candidatePath, names)
	}
	if strings.HasSuffix(names.Archive, ".tar.gz") {
		return extractTarCandidate(archivePath, candidatePath, names)
	}
	return coded(CodeArchiveInvalid, errors.New("archive format is unsupported"))
}

func expectedCandidatePath(names PlatformAssets) string {
	return names.TopLevel + "/" + names.Binary
}

func validateArchiveMember(name string, names PlatformAssets) (string, error) {
	name = strings.TrimSuffix(name, "/")
	clean, err := cleanArchivePath(name)
	if err != nil {
		return "", err
	}
	if clean != names.TopLevel && !strings.HasPrefix(clean, names.TopLevel+"/") {
		return "", coded(CodeArchiveInvalid, errors.New("archive entry escapes the expected top-level directory"))
	}
	return clean, nil
}

func extractZipCandidate(archivePath, candidatePath string, names PlatformAssets) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return coded(CodeArchiveInvalid, errors.New("zip archive cannot be opened"))
	}
	defer func() { _ = reader.Close() }()
	if len(reader.File) == 0 || len(reader.File) > maxArchiveEntries {
		return coded(CodeArchiveInvalid, errors.New("zip archive has an invalid entry count"))
	}
	expected := expectedCandidatePath(names)
	seen := make(map[string]struct{}, len(reader.File))
	var candidate *zip.File
	var expanded uint64
	for _, entry := range reader.File {
		clean, err := validateArchiveMember(entry.Name, names)
		if err != nil {
			return err
		}
		if _, duplicate := seen[clean]; duplicate {
			return coded(CodeArchiveInvalid, errors.New("zip archive has duplicate entries"))
		}
		seen[clean] = struct{}{}
		mode := entry.Mode()
		if mode&os.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
			return coded(CodeArchiveInvalid, errors.New("zip archive contains a link or special entry"))
		}
		if entry.UncompressedSize64 > uint64(maxExpandedArchive) || expanded > uint64(maxExpandedArchive)-entry.UncompressedSize64 {
			return coded(CodeArchiveInvalid, errors.New("zip archive expands beyond limit"))
		}
		expanded += entry.UncompressedSize64
		if clean == expected {
			if mode.IsDir() || candidate != nil {
				return coded(CodeArchiveInvalid, errors.New("zip archive candidate is duplicated or invalid"))
			}
			if entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > uint64(MaxCandidateBytes) {
				return coded(CodeCandidateTooLarge, errors.New("candidate size is invalid"))
			}
			candidate = entry
		}
	}
	if candidate == nil {
		return coded(CodeArchiveInvalid, errors.New("zip archive does not contain the expected candidate"))
	}
	source, err := candidate.Open()
	if err != nil {
		return coded(CodeArchiveInvalid, errors.New("zip candidate cannot be opened"))
	}
	defer func() { _ = source.Close() }()
	return writeCandidate(candidatePath, source, int64(candidate.UncompressedSize64), names.Binary)
}

func extractTarCandidate(archivePath, candidatePath string, names PlatformAssets) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return coded(CodeArchiveInvalid, err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(io.LimitReader(file, MaxArchiveBytes+1))
	if err != nil {
		return coded(CodeArchiveInvalid, errors.New("gzip archive cannot be opened"))
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	expected := expectedCandidatePath(names)
	seen := map[string]struct{}{}
	entries := 0
	expanded := int64(0)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return coded(CodeArchiveInvalid, errors.New("tar archive cannot be read"))
		}
		entries++
		if entries > maxArchiveEntries {
			return coded(CodeArchiveInvalid, errors.New("tar archive has too many entries"))
		}
		clean, err := validateArchiveMember(header.Name, names)
		if err != nil {
			return err
		}
		if _, duplicate := seen[clean]; duplicate {
			return coded(CodeArchiveInvalid, errors.New("tar archive has duplicate entries"))
		}
		seen[clean] = struct{}{}
		if header.Size < 0 || expanded > maxExpandedArchive-header.Size {
			return coded(CodeArchiveInvalid, errors.New("tar archive expands beyond limit"))
		}
		expanded += header.Size
		switch header.Typeflag {
		case tar.TypeDir:
			if clean == expected {
				return coded(CodeArchiveInvalid, errors.New("tar candidate is a directory"))
			}
		case tar.TypeReg, byte(0):
			if clean == expected {
				if found {
					return coded(CodeArchiveInvalid, errors.New("tar candidate is duplicated"))
				}
				if header.Size <= 0 || header.Size > MaxCandidateBytes {
					return coded(CodeCandidateTooLarge, errors.New("candidate size is invalid"))
				}
				if err := writeCandidate(candidatePath, reader, header.Size, names.Binary); err != nil {
					return err
				}
				found = true
			}
		default:
			return coded(CodeArchiveInvalid, errors.New("tar archive contains a link or special entry"))
		}
	}
	if entries == 0 || !found {
		return coded(CodeArchiveInvalid, errors.New("tar archive does not contain the expected candidate"))
	}
	return nil
}

func writeCandidate(candidatePath string, source io.Reader, expectedSize int64, binaryName string) error {
	if path.Base(binaryName) != binaryName {
		return coded(CodeArchiveInvalid, errors.New("candidate binary name is invalid"))
	}
	mode := os.FileMode(0o700)
	file, err := os.OpenFile(candidatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return coded(CodePersistence, err)
	}
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(candidatePath)
		}
	}()
	written, err := io.CopyBuffer(file, io.LimitReader(source, MaxCandidateBytes+1), make([]byte, archiveCopyBufferSize))
	if err != nil {
		return coded(CodeArchiveInvalid, err)
	}
	if written != expectedSize || written > MaxCandidateBytes {
		return coded(CodeCandidateTooLarge, fmt.Errorf("candidate size mismatch"))
	}
	if err := file.Sync(); err != nil {
		return coded(CodePersistence, err)
	}
	if err := file.Close(); err != nil {
		return coded(CodePersistence, err)
	}
	if err := os.Chmod(candidatePath, mode); err != nil {
		return coded(CodePersistence, err)
	}
	succeeded = true
	return nil
}
