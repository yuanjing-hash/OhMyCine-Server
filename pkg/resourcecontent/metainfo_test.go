package resourcecontent

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
)

func TestBuildMetainfoPreservesRawInfoAndRefusesMissingV2Layers(t *testing.T) {
	raw := encoded(singleInfo("movie.mkv", 10))
	m, err := ParseInfo(raw, infoIdentity(raw), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	m.Trackers = [][]string{{"https://tracker.example/private-key"}}
	container, err := BuildMetainfo(m)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := ParseTorrent(container, Limits{})
	if err != nil || verified.Identity != m.Identity || !bytes.Equal(verified.InfoBytes, raw) {
		t.Fatal("resource hash changed", err)
	}
	m.Identity.V2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	m.Format = "hybrid"
	if _, err = BuildMetainfo(m); !errors.Is(err, ErrUnsupported) {
		t.Fatal("guessed v2 container", err)
	}
}

func TestV2ExecutionRequiresCorrectPieceLayers(t *testing.T) {
	first := sha256.Sum256([]byte("first piece"))
	second := sha256.Sum256([]byte("second piece"))
	layer := append(append([]byte(nil), first[:]...), second[:]...)
	root := sha256.Sum256(layer)
	info := map[string]any{"name": "large.mkv", "meta version": 2, "piece length": 16384, "file tree": map[string]any{"large.mkv": map[string]any{"": map[string]any{"length": 32768, "pieces root": root[:]}}}}
	container := map[string]any{"info": info}
	m, err := ParseTorrent(encoded(container), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateExecutableMetainfo(m); !errors.Is(err, ErrUnsupported) {
		t.Fatal("missing layers executable", err)
	}
	container["piece layers"] = map[string]any{string(root[:]): layer}
	m, err = ParseTorrent(encoded(container), Limits{})
	if err != nil || ValidateExecutableMetainfo(m) != nil {
		t.Fatal("correct original layers rejected", err)
	}
	bad := bytes.Clone(layer)
	bad[0] ^= 1
	container["piece layers"] = map[string]any{string(root[:]): bad}
	m, err = ParseTorrent(encoded(container), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateExecutableMetainfo(m); !errors.Is(err, ErrHashMismatch) {
		t.Fatal("wrong layer root executable", err)
	}
}
