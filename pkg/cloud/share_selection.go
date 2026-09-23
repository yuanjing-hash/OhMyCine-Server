package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
)

const MaxSharePreviewEntries = 2000
const MaxShareSelectedFiles = 500
const MaxSelectedShareSourceBytes = 24 << 10

// ShareBrowseDriver lists one complete bounded directory within a share.
// Implementations must fail if they cannot prove the directory is complete.
type ShareBrowseDriver interface {
	ShareReceiveDriver
	InspectShareDirectory(context.Context, string, string) (ShareSnapshot, error)
}

type ShareTreeItem struct {
	ID           string `json:"id"`
	RelativePath string `json:"relative_path"`
	IsDir        bool   `json:"is_dir"`
	Size         int64  `json:"size"`
}

type ShareSelection struct {
	Version int             `json:"version"`
	Files   []ShareTreeItem `json:"files"`
}

// SelectedShareSource is only persisted in encrypted sources / sealed grants.
type SelectedShareSource struct {
	URL       string         `json:"url"`
	Selection ShareSelection `json:"selection"`
}

func safeSharePath(value string) bool {
	if value == "" || len(value) > 2048 || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\:") || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." || part == "" || strings.TrimSpace(part) != part || strings.ContainsFunc(part, unicode.IsControl) {
			return false
		}
	}
	return true
}

func (selection ShareSelection) Validate() error {
	if selection.Version != 1 || len(selection.Files) == 0 || len(selection.Files) > MaxShareSelectedFiles {
		return errors.New("share_selection_invalid")
	}
	ids, paths := map[string]bool{}, map[string]bool{}
	total := int64(0)
	for _, file := range selection.Files {
		key := strings.ToLower(file.RelativePath)
		if file.ID == "" || len(file.ID) > 128 || strings.ContainsAny(file.ID, ",/\\\x00\r\n") || !safeSharePath(file.RelativePath) || file.IsDir || file.Size < 0 || ids[file.ID] || paths[key] || total > math.MaxInt64-file.Size {
			return errors.New("share_selection_invalid")
		}
		ids[file.ID], paths[key] = true, true
		total += file.Size
	}
	for key := range paths {
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			if paths[parent] {
				return errors.New("share_selection_invalid")
			}
		}
	}
	return nil
}

func (selection ShareSelection) Digest() (string, error) {
	if err := selection.Validate(); err != nil {
		return "", err
	}
	files := append([]ShareTreeItem(nil), selection.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	raw, err := json.Marshal(ShareSelection{Version: 1, Files: files})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func EncodeSelectedShareSource(raw string, selection ShareSelection) (string, error) {
	if err := selection.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(SelectedShareSource{URL: raw, Selection: selection})
	if err != nil || len(data) > MaxSelectedShareSourceBytes {
		return "", errors.New("share_selection_invalid")
	}
	return string(data), nil
}
func DecodeSelectedShareSource(raw string) (SelectedShareSource, error) {
	var source SelectedShareSource
	if len(raw) > MaxSelectedShareSourceBytes {
		return source, errors.New("share_selection_invalid")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return source, errors.New("share_selection_invalid")
	}
	if decoder.Decode(new(any)) != io.EOF || source.URL == "" || len(source.URL) > 8192 {
		return source, errors.New("share_selection_invalid")
	}
	return source, source.Selection.Validate()
}

// InspectShareTree returns complete metadata or an error, never a partial total.
func InspectShareTree(ctx context.Context, driver ShareBrowseDriver, raw string) (ShareSnapshot, []ShareTreeItem, error) {
	return inspectShareTree(ctx, driver, raw, nil)
}

func inspectShareTree(ctx context.Context, driver ShareBrowseDriver, raw string, visit func(string) bool) (ShareSnapshot, []ShareTreeItem, error) {
	type directory struct {
		id, relative string
		depth        int
	}
	queue := []directory{{id: "0"}}
	seenIDs, seenPaths := map[string]bool{"0": true}, map[string]bool{}
	entries := []ShareTreeItem{}
	var root ShareSnapshot
	for index := 0; index < len(queue); index++ {
		if err := ctx.Err(); err != nil {
			return root, nil, err
		}
		if index >= 200 {
			return root, nil, Error(CodeShareTooLarge, false, nil)
		}
		current := queue[index]
		snapshot, err := driver.InspectShareDirectory(ctx, raw, current.id)
		if err != nil {
			return root, nil, err
		}
		if index == 0 {
			root = snapshot
		} else if snapshot.ShareCode != root.ShareCode || snapshot.ReceiveCode != root.ReceiveCode {
			return root, nil, Error(CodeResponseInvalid, false, nil)
		}
		for _, item := range snapshot.Items {
			relative := item.Name
			if current.relative != "" {
				relative = current.relative + "/" + item.Name
			}
			key := strings.ToLower(relative)
			if !safeSharePath(relative) || strings.Contains(item.Name, "/") || item.ID == "" || seenIDs[item.ID] || seenPaths[key] || item.Size < 0 {
				return root, nil, Error(CodeResponseInvalid, false, nil)
			}
			seenIDs[item.ID], seenPaths[key] = true, true
			entry := ShareTreeItem{ID: item.ID, RelativePath: relative, IsDir: item.IsDir, Size: item.Size}
			if item.IsDir {
				entry.Size = 0
				if current.depth >= 15 {
					return root, nil, Error(CodeShareTooLarge, false, nil)
				}
				if visit == nil || visit(relative) {
					queue = append(queue, directory{id: item.ID, relative: relative, depth: current.depth + 1})
				}
			}
			entries = append(entries, entry)
			if len(entries) > MaxSharePreviewEntries {
				return root, nil, Error(CodeShareTooLarge, false, nil)
			}
		}
	}
	return root, entries, nil
}

// ReceiveSelectedShare reconstructs relative directories and receives leaf IDs
// only. It reconciles partial/lost-ACK output and never broadens selection.
func ReceiveSelectedShare(ctx context.Context, driver ShareBrowseDriver, raw string, selection ShareSelection, destination string) error {
	if err := selection.Validate(); err != nil {
		return err
	}
	mutation, ok := driver.(MutationDriver)
	if !ok {
		return Error(CodeUnavailable, false, nil)
	}
	expected := map[string]ShareTreeItem{}
	directories := map[string]bool{".": true}
	for _, file := range selection.Files {
		expected[file.RelativePath] = file
		for parent := path.Dir(file.RelativePath); parent != "."; parent = path.Dir(parent) {
			directories[parent] = true
		}
	}
	// Inspect existing output before source I/O: completed selections remain
	// recoverable even when a publisher has subsequently removed the share.
	scan := func() (map[string]Item, map[string]string, error) {
		found := map[string]Item{}
		dirs := map[string]string{".": destination}
		pending := []string{"."}
		for index := 0; index < len(pending); index++ {
			rel := pending[index]
			seen := map[string]bool{}
			for offset := int64(0); ; {
				if err := ctx.Err(); err != nil {
					return nil, nil, err
				}
				page, err := driver.List(ctx, dirs[rel], PageRequest{Offset: offset, Limit: 500})
				if err != nil {
					return nil, nil, err
				}
				if page.HasMore && len(page.Items) == 0 {
					return nil, nil, Error(CodeResponseInvalid, false, nil)
				}
				for _, item := range page.Items {
					if item.ID == "" || item.ParentID != dirs[rel] || item.Name == "" || strings.ContainsAny(item.Name, "/\\") || seen[item.ID] {
						return nil, nil, Error(CodeResponseInvalid, false, nil)
					}
					seen[item.ID] = true
					child := item.Name
					if rel != "." {
						child = rel + "/" + child
					}
					if !safeSharePath(child) {
						return nil, nil, Error(CodeResponseInvalid, false, nil)
					}
					if item.IsDir {
						if !directories[child] || dirs[child] != "" {
							return nil, nil, Error(CodeMutationUnknown, false, errors.New("share_selection_output_conflict"))
						}
						dirs[child] = item.ID
						pending = append(pending, child)
					} else {
						want, exists := expected[child]
						if !exists || want.Size != item.Size {
							return nil, nil, Error(CodeMutationUnknown, false, errors.New("share_selection_output_conflict"))
						}
						if _, exists := found[child]; exists {
							return nil, nil, Error(CodeMutationUnknown, false, errors.New("share_selection_output_conflict"))
						}
						found[child] = item
					}
				}
				if len(seen) > MaxSharePreviewEntries {
					return nil, nil, Error(CodeShareTooLarge, false, nil)
				}
				if !page.HasMore {
					break
				}
				offset += int64(len(page.Items))
			}
		}
		return found, dirs, nil
	}
	found, dirs, err := scan()
	if err != nil {
		return err
	}
	if len(found) == len(expected) {
		return nil
	}
	snapshot, tree, err := inspectShareTree(ctx, driver, raw, func(relative string) bool { return directories[relative] })
	if err != nil {
		return err
	}
	current := map[string]ShareTreeItem{}
	for _, entry := range tree {
		if !entry.IsDir {
			current[entry.RelativePath] = entry
		}
	}
	for _, file := range selection.Files {
		if current[file.RelativePath] != file {
			return Error(CodeShareInvalid, false, errors.New("share_selection_changed"))
		}
	}
	ordered := make([]string, 0, len(directories))
	for dir := range directories {
		ordered = append(ordered, dir)
	}
	sort.Strings(ordered)
	for _, dir := range ordered {
		if dirs[dir] != "" {
			continue
		}
		parent := path.Dir(dir)
		item, err := mutation.CreateDirectory(ctx, dirs[parent], path.Base(dir))
		if err != nil {
			return err
		}
		if item.ID == "" || item.ParentID != dirs[parent] || !item.IsDir || item.Name != path.Base(dir) {
			return Error(CodeResponseInvalid, false, nil)
		}
		dirs[dir] = item.ID
	}
	groups := map[string][]ShareItem{}
	for _, file := range selection.Files {
		if _, exists := found[file.RelativePath]; !exists {
			parent := path.Dir(file.RelativePath)
			groups[parent] = append(groups[parent], ShareItem{ID: file.ID, Name: path.Base(file.RelativePath), Size: file.Size})
		}
	}
	for _, dir := range ordered {
		files := groups[dir]
		for start := 0; start < len(files); start += 100 {
			end := min(start+100, len(files))
			batch := ShareSnapshot{ShareCode: snapshot.ShareCode, ReceiveCode: snapshot.ReceiveCode, Title: snapshot.Title, Items: files[start:end]}
			receiveErr := driver.ReceiveShare(ctx, batch, dirs[dir])
			complete := false
			for attempt := 0; attempt < 5; attempt++ {
				found, _, err = scan()
				if err != nil {
					return err
				}
				complete = true
				for _, file := range files[start:end] {
					rel := file.Name
					if dir != "." {
						rel = dir + "/" + rel
					}
					if _, ok := found[rel]; !ok {
						complete = false
					}
				}
				if complete {
					break
				}
				timer := time.NewTimer(time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			if !complete {
				if receiveErr != nil {
					return receiveErr
				}
				return Error(CodeShareUnknown, true, errors.New("share_selection_result_unknown"))
			}
		}
	}
	found, _, err = scan()
	if err != nil {
		return err
	}
	if len(found) != len(expected) {
		return Error(CodeShareUnknown, true, errors.New("share_selection_result_unknown"))
	}
	return nil
}
