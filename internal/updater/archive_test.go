package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writeZip(t *testing.T, entries map[string][]byte, modes map[string]os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for name, payload := range entries {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		if mode := modes[name]; mode != 0 {
			header.SetMode(mode)
		}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractZipCandidateAndRejectAdversarialMembers(t *testing.T) {
	names, _ := AssetNames("1.2.3", "windows", "amd64")
	archive := writeZip(t, map[string][]byte{names.TopLevel + "/ohmycine-server.exe": []byte("new-server"), names.TopLevel + "/README.md": []byte("readme")}, nil)
	destination := filepath.Join(t.TempDir(), "candidate.exe")
	if err := ExtractCandidate(archive, destination, names); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(destination); string(data) != "new-server" {
		t.Fatalf("unexpected candidate: %q", data)
	}

	for name, entries := range map[string]map[string][]byte{
		"traversal": {names.TopLevel + "/../ohmycine-server.exe": []byte("bad")},
		"backslash": {names.TopLevel + "\\ohmycine-server.exe": []byte("bad")},
		"wrong top": {"other/ohmycine-server.exe": []byte("bad")},
	} {
		t.Run(name, func(t *testing.T) {
			bad := writeZip(t, entries, nil)
			if err := ExtractCandidate(bad, filepath.Join(t.TempDir(), "out"), names); ErrorCode(err) != CodeArchiveInvalid {
				t.Fatalf("expected invalid archive, got %v", err)
			}
		})
	}
	symlinkName := names.TopLevel + "/ohmycine-server.exe"
	symlink := writeZip(t, map[string][]byte{symlinkName: []byte("target")}, map[string]os.FileMode{symlinkName: os.ModeSymlink | 0o777})
	if err := ExtractCandidate(symlink, filepath.Join(t.TempDir(), "out"), names); ErrorCode(err) != CodeArchiveInvalid {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func writeTarGz(t *testing.T, entries []tar.Header, contents [][]byte) string {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for i := range entries {
		header := entries[i]
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if len(contents[i]) > 0 {
			if _, err := tw.Write(contents[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "release.tar.gz")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTarCandidateRejectsLinksAndDuplicates(t *testing.T) {
	names, _ := AssetNames("1.2.3", "linux", "amd64")
	candidate := names.TopLevel + "/ohmycine-server"
	archive := writeTarGz(t, []tar.Header{{Name: candidate, Mode: 0o755, Size: 3, Typeflag: tar.TypeReg}}, [][]byte{[]byte("new")})
	destination := filepath.Join(t.TempDir(), "candidate")
	if err := ExtractCandidate(archive, destination, names); err != nil {
		t.Fatal(err)
	}
	link := writeTarGz(t, []tar.Header{{Name: candidate, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}}, [][]byte{nil})
	if err := ExtractCandidate(link, filepath.Join(t.TempDir(), "out"), names); ErrorCode(err) != CodeArchiveInvalid {
		t.Fatalf("expected link rejection, got %v", err)
	}
	duplicate := writeTarGz(t, []tar.Header{{Name: candidate, Size: 1, Typeflag: tar.TypeReg}, {Name: candidate, Size: 1, Typeflag: tar.TypeReg}}, [][]byte{[]byte("a"), []byte("b")})
	if err := ExtractCandidate(duplicate, filepath.Join(t.TempDir(), "out"), names); ErrorCode(err) != CodeArchiveInvalid {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}
}
