package resourcecontent

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ParseTorrent reads real content facts from the original bounded metainfo.
// It preserves the exact info dictionary used by both BitTorrent hash formats.
func ParseTorrent(raw []byte, requested Limits) (Manifest, error) {
	limits := normalizeLimits(requested)
	if len(raw) == 0 || len(raw) > limits.MaxMetainfoBytes {
		return Manifest{}, invalid("metainfo_size", ErrLimitExceeded)
	}
	owned := bytes.Clone(raw)
	root, err := decode(owned, limits)
	if err != nil {
		return Manifest{}, err
	}
	if root.kind != 'd' || root.get("info") == nil || root.get("info").kind != 'd' {
		return Manifest{}, invalid("info_missing", ErrInvalidMetainfo)
	}
	m, err := parseInfo(root.get("info"), limits)
	if err != nil {
		return Manifest{}, err
	}
	m.MetainfoBytes = owned
	m.Trackers = readTrackers(root)
	return m, nil
}

// ParseInfo validates metadata obtained from peers before producing file facts.
// A nonempty expected full hash is required, even when the handshake succeeded.
func ParseInfo(raw []byte, expected Identity, requested Limits) (Manifest, error) {
	limits := normalizeLimits(requested)
	if len(raw) == 0 || len(raw) > limits.MaxInfoBytes {
		return Manifest{}, invalid("info_size", ErrLimitExceeded)
	}
	if expected.V1 == "" && expected.V2 == "" {
		return Manifest{}, invalid("expected_hash_missing", ErrHashMismatch)
	}
	owned := bytes.Clone(raw)
	v, err := decode(owned, limits)
	if err != nil {
		return Manifest{}, err
	}
	m, err := parseInfo(v, limits)
	if err != nil {
		return Manifest{}, err
	}
	if err = m.ValidateIdentity(expected); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func parseInfo(info *value, limits Limits) (Manifest, error) {
	if info == nil || info.kind != 'd' {
		return Manifest{}, invalid("info_invalid", ErrInvalidMetainfo)
	}
	if len(info.raw) > limits.MaxInfoBytes {
		return Manifest{}, invalid("info_size", ErrLimitExceeded)
	}
	name := info.get("name.utf-8")
	if name == nil {
		name = info.get("name")
	}
	if name == nil || name.kind != 's' || !safeComponent(name.data) || len(name.data) > limits.MaxPathBytes {
		return Manifest{}, invalid("name_unsafe", ErrUnsafePath)
	}
	m := Manifest{Name: string(name.data), InfoBytes: info.raw, Files: []File{}}
	if p := info.get("private"); p != nil {
		if p.kind != 'i' || p.n != 0 && p.n != 1 {
			return Manifest{}, invalid("private_invalid", ErrInvalidMetainfo)
		}
		m.Private = p.n == 1
	}
	pl := info.get("piece length")
	if pl == nil || pl.kind != 'i' || pl.n <= 0 || pl.n > 1<<30 {
		return Manifest{}, invalid("piece_length_invalid", ErrInvalidMetainfo)
	}
	m.PieceLength = pl.n
	metaVersion := int64(0)
	if v := info.get("meta version"); v != nil {
		if v.kind != 'i' {
			return Manifest{}, invalid("meta_version_invalid", ErrInvalidMetainfo)
		}
		metaVersion = v.n
	}
	if metaVersion != 0 && metaVersion != 2 {
		return Manifest{}, unsupportedVersion(metaVersion)
	}
	hasV2 := metaVersion == 2
	hasV1 := !hasV2 || info.get("pieces") != nil || info.get("files") != nil || info.get("length") != nil
	var v1, v2 []File
	var err error
	if hasV1 {
		v1, m.MultiFile, err = readV1Files(info, m.Name, limits)
		if err != nil {
			return Manifest{}, err
		}
		total, e := sumFiles(v1)
		if e != nil {
			return Manifest{}, e
		}
		pieces := info.get("pieces")
		if pieces == nil || pieces.kind != 's' || len(pieces.data)%20 != 0 || int64(len(pieces.data)/20) != pieceCount(total, pl.n) {
			return Manifest{}, invalid("pieces_invalid", ErrInvalidMetainfo)
		}
		hash := sha1.Sum(info.raw)
		m.Identity.V1 = hex.EncodeToString(hash[:])
		m.Format, m.Files, m.TotalSize = "v1", v1, total
	}
	if hasV2 {
		if pl.n < 16384 || pl.n&(pl.n-1) != 0 {
			return Manifest{}, invalid("v2_piece_length_invalid", ErrInvalidMetainfo)
		}
		tree := info.get("file tree")
		if tree == nil || tree.kind != 'd' {
			return Manifest{}, invalid("file_tree_missing", ErrInvalidMetainfo)
		}
		if err = walkV2(tree, nil, m.Name, &v2, limits); err != nil {
			return Manifest{}, err
		}
		if len(v2) == 0 {
			return Manifest{}, invalid("files_empty", ErrInvalidMetainfo)
		}
		total, e := sumFiles(v2)
		if e != nil {
			return Manifest{}, e
		}
		hash := sha256.Sum256(info.raw)
		m.Identity.V2 = hex.EncodeToString(hash[:])
		if hasV1 {
			if err = compareHybrid(v1, v2); err != nil {
				return Manifest{}, err
			}
			j := 0
			for i := range m.Files {
				if !m.Files[i].Padding {
					m.Files[i].PiecesRoot = v2[j].PiecesRoot
					j++
				}
			}
			m.Format = "hybrid"
		} else {
			m.Format, m.Files, m.TotalSize = "v2", v2, total
			m.MultiFile = !(len(v2) == 1 && v2[0].Path == m.Name)
		}
	}
	if err := uniquePaths(m.Files); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func readV1Files(info *value, name string, limits Limits) ([]File, bool, error) {
	length, files := info.get("length"), info.get("files")
	if (length == nil) == (files == nil) {
		return nil, false, invalid("file_mode_invalid", ErrInvalidMetainfo)
	}
	if length != nil {
		if length.kind != 'i' || length.n < 0 {
			return nil, false, invalid("file_size_invalid", ErrInvalidMetainfo)
		}
		return []File{{Index: 0, Path: name, Size: length.n}}, false, nil
	}
	if files.kind != 'l' || len(files.list) == 0 || len(files.list) > limits.MaxFiles {
		return nil, true, invalid("files_limit", ErrLimitExceeded)
	}
	out := make([]File, 0, len(files.list))
	seen := map[string]bool{}
	for index, v := range files.list {
		if v.kind != 'd' || v.get("length") == nil || v.get("length").kind != 'i' || v.get("length").n < 0 {
			return nil, true, invalid("file_size_invalid", ErrInvalidMetainfo)
		}
		p := v.get("path.utf-8")
		if p == nil {
			p = v.get("path")
		}
		if p == nil || p.kind != 'l' || len(p.list) == 0 || len(p.list) > limits.MaxDepth {
			return nil, true, invalid("path_invalid", ErrUnsafePath)
		}
		parts := make([]string, 0, len(p.list))
		for _, part := range p.list {
			if part.kind != 's' || !safeComponent(part.data) {
				return nil, true, invalid("path_unsafe", ErrUnsafePath)
			}
			parts = append(parts, string(part.data))
		}
		filePath := strings.Join(parts, "/")
		if len(filePath) > limits.MaxPathBytes || seen[filePath] {
			return nil, true, invalid("path_duplicate_or_long", ErrUnsafePath)
		}
		seen[filePath] = true
		padding := false
		if v.get("symlink path") != nil {
			return nil, true, invalid("symlink_unsupported", ErrUnsupported)
		}
		if attr := v.get("attr"); attr != nil {
			if attr.kind != 's' {
				return nil, true, invalid("attr_invalid", ErrInvalidMetainfo)
			}
			padding = bytes.ContainsRune(attr.data, 'p')
			if bytes.ContainsRune(attr.data, 'l') {
				return nil, true, invalid("symlink_unsupported", ErrUnsupported)
			}
		}
		out = append(out, File{Index: index, Path: filePath, Size: v.get("length").n, Padding: padding})
	}
	return out, true, nil
}

func walkV2(tree *value, parts []string, rootName string, files *[]File, limits Limits) error {
	if tree == nil || tree.kind != 'd' || len(tree.dict) == 0 || len(parts) > limits.MaxDepth {
		return invalid("file_tree_invalid", ErrInvalidMetainfo)
	}
	if props := tree.get(""); props != nil {
		if len(tree.dict) != 1 || props.kind != 'd' || props.get("length") == nil || props.get("length").kind != 'i' || props.get("length").n < 0 {
			return invalid("file_tree_leaf_invalid", ErrInvalidMetainfo)
		}
		if props.get("symlink path") != nil {
			return invalid("symlink_unsupported", ErrUnsupported)
		}
		if attr := props.get("attr"); attr != nil {
			if attr.kind != 's' {
				return invalid("attr_invalid", ErrInvalidMetainfo)
			}
			if bytes.ContainsRune(attr.data, 'l') {
				return invalid("symlink_unsupported", ErrUnsupported)
			}
		}
		length := props.get("length").n
		root := props.get("pieces root")
		if root != nil && (root.kind != 's' || len(root.data) != 32) || length > 0 && root == nil {
			return invalid("pieces_root_invalid", ErrInvalidMetainfo)
		}
		p := strings.Join(parts, "/")
		if p == "" {
			p = rootName
		}
		if len(p) > limits.MaxPathBytes || len(*files) >= limits.MaxFiles {
			return invalid("file_tree_limit", ErrLimitExceeded)
		}
		file := File{Index: len(*files), Path: p, Size: length}
		if root != nil {
			file.PiecesRoot = hex.EncodeToString(root.data)
		}
		*files = append(*files, file)
		return nil
	}
	ordered := append([]pair(nil), tree.dict...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key < ordered[j].key })
	for _, child := range ordered {
		if !safeComponent([]byte(child.key)) {
			return invalid("path_unsafe", ErrUnsafePath)
		}
		if err := walkV2(child.v, append(append([]string(nil), parts...), child.key), rootName, files, limits); err != nil {
			return err
		}
	}
	return nil
}

func compareHybrid(v1, v2 []File) error {
	actual := make([]File, 0, len(v1))
	for _, file := range v1 {
		if !file.Padding {
			actual = append(actual, file)
		}
	}
	if len(actual) != len(v2) {
		return invalid("hybrid_files_mismatch", ErrInvalidMetainfo)
	}
	for i := range actual {
		if actual[i].Size != v2[i].Size || actual[i].Path != v2[i].Path {
			return invalid("hybrid_files_mismatch", ErrInvalidMetainfo)
		}
	}
	return nil
}

func uniquePaths(files []File) error {
	paths := make(map[string]bool, len(files))
	for _, file := range files {
		if paths[file.Path] {
			return invalid("path_collision", ErrUnsafePath)
		}
		paths[file.Path] = true
	}
	for filePath := range paths {
		for i, ch := range filePath {
			if ch == '/' && paths[filePath[:i]] {
				return invalid("path_collision", ErrUnsafePath)
			}
		}
	}
	return nil
}

func sumFiles(files []File) (int64, error) {
	var total int64
	for _, f := range files {
		if f.Size < 0 || f.Size > math.MaxInt64-total {
			return 0, invalid("total_size_overflow", ErrInvalidMetainfo)
		}
		total += f.Size
	}
	return total, nil
}

func pieceCount(total, length int64) int64 {
	n := total / length
	if total%length != 0 {
		n++
	}
	return n
}

func safeComponent(b []byte) bool {
	if len(b) == 0 || len(b) > 255 || !utf8.Valid(b) || bytes.Equal(b, []byte(".")) || bytes.Equal(b, []byte("..")) {
		return false
	}
	for _, ch := range string(b) {
		if ch == '/' || ch == '\\' || ch == ':' || unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

func readTrackers(root *value) [][]string {
	out := [][]string{}
	seen := map[string]bool{}
	count := 0
	add := func(v *value, tier *[]string) {
		if v == nil || v.kind != 's' || len(v.data) > 2048 || count >= 64 {
			return
		}
		raw := string(v.data)
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "udp" || seen[raw] {
			return
		}
		seen[raw], count = true, count+1
		*tier = append(*tier, raw)
	}
	if tiers := root.get("announce-list"); tiers != nil && tiers.kind == 'l' {
		for _, tier := range tiers.list {
			if tier.kind != 'l' {
				continue
			}
			values := []string{}
			for _, item := range tier.list {
				add(item, &values)
			}
			if len(values) > 0 {
				out = append(out, values)
			}
		}
	}
	values := []string{}
	add(root.get("announce"), &values)
	if len(values) > 0 {
		out = append(out, values)
	}
	return out
}
