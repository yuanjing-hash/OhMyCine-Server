package resourcecontent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const SelectionVersion = 1

// Selection is an immutable acquisition intent bound to verified resource
// facts. It is private execution data, not a browser's permission to acquire
// arbitrary native indices. The owning service supplies actor/source authority.
type Selection struct {
	Version  int            `json:"version"`
	Identity Identity       `json:"identity"`
	Files    []SelectedFile `json:"files"`
}

type SelectedFile struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
}

// Digest is deterministic across input ordering and cannot collide through
// delimiter ambiguity. It includes schema version, complete identities and
// each selected file's original index/path/size. It never changes its input.
func (s Selection) Digest() (string, error) {
	if err := s.validateShape(); err != nil {
		return "", err
	}
	copySelection := s
	copySelection.Files = append([]SelectedFile(nil), s.Files...)
	sort.Slice(copySelection.Files, func(i, j int) bool { return copySelection.Files[i].Index < copySelection.Files[j].Index })
	raw, err := json.Marshal(copySelection)
	if err != nil {
		return "", invalid("selection_invalid", ErrInvalidMetainfo)
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

func (s Selection) ValidateAgainst(manifest Manifest) error {
	if err := s.validateShape(); err != nil {
		return err
	}
	if s.Identity != manifest.Identity {
		return invalid("selection_identity_mismatch", ErrHashMismatch)
	}
	actual := make(map[int]File, len(manifest.Files))
	for _, file := range manifest.Files {
		if _, duplicate := actual[file.Index]; duplicate {
			return invalid("manifest_index_duplicate", ErrInvalidMetainfo)
		}
		actual[file.Index] = file
	}
	for _, selected := range s.Files {
		file, exists := actual[selected.Index]
		if !exists || file.Padding || file.Path != selected.Path || file.Size != selected.Size {
			return invalid("selection_file_mismatch", ErrInvalidMetainfo)
		}
	}
	return nil
}

func (s Selection) validateShape() error {
	if s.Version != SelectionVersion || len(s.Files) == 0 || len(s.Files) > DefaultLimits().MaxFiles {
		return invalid("selection_invalid", ErrInvalidMetainfo)
	}
	if s.Identity.V1 == "" && s.Identity.V2 == "" {
		return invalid("selection_identity_invalid", ErrHashMismatch)
	}
	for _, identity := range []struct {
		value string
		size  int
	}{{s.Identity.V1, 20}, {s.Identity.V2, 32}} {
		if identity.value == "" {
			continue
		}
		raw, err := hex.DecodeString(identity.value)
		if err != nil || len(raw) != identity.size || strings.ToLower(identity.value) != identity.value {
			return invalid("selection_identity_invalid", ErrHashMismatch)
		}
	}
	indexes := make(map[int]bool, len(s.Files))
	paths := make(map[string]bool, len(s.Files))
	for _, file := range s.Files {
		if file.Index < 0 || file.Index >= DefaultLimits().MaxFiles || file.Size < 0 || len(file.Path) > DefaultLimits().MaxPathBytes || indexes[file.Index] || paths[file.Path] {
			return invalid("selection_file_invalid", ErrInvalidMetainfo)
		}
		for _, part := range strings.Split(file.Path, "/") {
			if !safeComponent([]byte(part)) {
				return invalid("selection_path_unsafe", ErrUnsafePath)
			}
		}
		indexes[file.Index], paths[file.Path] = true, true
	}
	return nil
}
