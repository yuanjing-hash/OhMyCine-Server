package downloader

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// GroupCleanup is a persisted, generation-bound cleanup receipt. ProbeOnly
// never repeats an uncertain native removal. Files is a proven subset whose
// target publication and source policy were accepted by the durable authority.
type GroupCleanup struct {
	Key         string                    `json:"key"`
	Group       SelectionGroup            `json:"group"`
	Files       resourcecontent.Selection `json:"files"`
	DeleteFiles bool                      `json:"delete_files"`
	ProbeOnly   bool                      `json:"probe_only"`
}
type SelectionGroupCleaner interface {
	CleanupSelectionGroup(context.Context, Source, SelectionContext, GroupCleanup) (bool, error)
}

// RemoveSelectionGroupFiles removes exact verified files only. It never walks
// or recursively removes a package, follows links, or interprets a provider ID
// as a path. New lifecycle groups have different physical roots.
func RemoveSelectionGroupFiles(ctx context.Context, root string, binding SelectionContext, selection resourcecontent.Selection) (int, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || selection.Identity != binding.Selection.Identity {
		return 0, Error("downloader_selection_cleanup_invalid", false, nil)
	}
	if info, err := os.Lstat(root); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, Error("downloader_selection_cleanup_invalid", false, err)
	}
	for ancestor := root; ancestor != filepath.Dir(ancestor); ancestor = filepath.Dir(ancestor) {
		info, err := os.Lstat(ancestor)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return 0, Error("downloader_selection_cleanup_invalid", false, err)
		}
	}
	removed := 0
	for _, file := range selection.Files {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		relative := file.Path
		if binding.MultiFile {
			relative = binding.RootName + "/" + relative
		}
		if relative == "" || strings.ContainsAny(relative, "\\:\x00\r\n") || strings.HasPrefix(relative, "/") {
			return removed, Error("downloader_selection_cleanup_invalid", false, nil)
		}
		parts := strings.Split(relative, "/")
		candidate := root
		for index, part := range parts {
			if part == "" || part == "." || part == ".." {
				return removed, Error("downloader_selection_cleanup_invalid", false, nil)
			}
			candidate = filepath.Join(candidate, part)
			info, err := os.Lstat(candidate)
			if os.IsNotExist(err) {
				break
			}
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return removed, Error("downloader_selection_cleanup_invalid", false, err)
			}
			if index != len(parts)-1 {
				if !info.IsDir() {
					return removed, Error("downloader_selection_cleanup_invalid", false, nil)
				}
				continue
			}
			if !info.Mode().IsRegular() || info.Size() != file.Size {
				return removed, Error("downloader_selection_cleanup_changed", false, nil)
			}
			if err := CheckSelectionFence(ctx); err != nil {
				return removed, err
			}
			if err = os.Remove(candidate); err != nil {
				return removed, err
			}
			removed++
		}
		// Empty, exact ancestors are safe; unrelated files keep their parent.
		for directory := filepath.Dir(candidate); directory != root && directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
			if err := CheckSelectionFence(ctx); err != nil {
				return removed, err
			}
			if err := os.Remove(directory); err != nil {
				break
			}
		}
	}
	if err := CheckSelectionFence(ctx); err != nil {
		return removed, err
	}
	_ = os.Remove(root)
	return removed, nil
}
