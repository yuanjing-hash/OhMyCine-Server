package downloader

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"strings"
)

// ObserveRequiredOutput inspects metadata only. The caller first proves this
// exact output root belongs to its original account/task operation. Enumeration
// is bounded and never provides completion without explicit per-leaf assertions.
func ObserveRequiredOutput(ctx context.Context, driver cloud.Driver, rootID string, plan OfflineRequiredPlan) (Manifest, bool) {
	if _, err := plan.Digest(); err != nil {
		return Manifest{}, false
	}
	reader, ok := driver.(cloud.FinalizedFileReader)
	if !ok {
		return Manifest{}, false
	}
	root, err := driver.Stat(ctx, rootID)
	if err != nil || root.ID != rootID || root.Name != plan.RootName || root.IsDir != plan.MultiFile {
		return Manifest{}, false
	}
	expected := map[string]int64{}
	for _, f := range plan.Files {
		expected[f.RelativePath] = f.Size
	}
	result := Manifest{Name: plan.RootName, Complete: true}
	var leaves []cloud.Item
	accept := func(item cloud.Item, relative string) bool {
		key := relative
		size, wanted := expected[key]
		if !wanted {
			return true
		}
		if item.IsDir || item.Size != size {
			return false
		}
		result.Files = append(result.Files, File{RelativePath: relative, Size: item.Size, ProviderItemID: item.ID, ProviderParentID: item.ParentID, SHA1: item.SHA1})
		leaves = append(leaves, item)
		delete(expected, key)
		return true
	}
	if !root.IsDir {
		if plan.MultiFile || !accept(root, root.Name) {
			return Manifest{}, false
		}
	} else {
		type dir struct {
			id, prefix string
			depth      int
		}
		pending := []dir{{root.ID, "", 0}}
		seen := map[string]bool{root.ID: true}
		entries := 0
		for len(pending) > 0 {
			current := pending[0]
			pending = pending[1:]
			if current.depth > 128 {
				return Manifest{}, false
			}
			for offset := int64(0); ; {
				page, err := driver.List(ctx, current.id, cloud.PageRequest{Offset: offset, Limit: 200})
				if err != nil || len(page.Items) == 0 && page.HasMore {
					return Manifest{}, false
				}
				for _, item := range page.Items {
					entries++
					if entries > 5000 || item.ID == "" || item.ParentID != current.id || seen[item.ID] || item.Name == "" || item.Name == "." || item.Name == ".." || strings.ContainsAny(item.Name, "/\\\x00\r\n") {
						return Manifest{}, false
					}
					seen[item.ID] = true
					relative := item.Name
					if current.prefix != "" {
						relative = current.prefix + "/" + item.Name
					}
					if item.IsDir {
						pending = append(pending, dir{item.ID, relative, current.depth + 1})
					} else if !accept(item, relative) {
						return Manifest{}, false
					}
				}
				offset += int64(len(page.Items))
				if !page.HasMore {
					break
				}
			}
		}
	}
	if len(expected) != 0 || len(leaves) == 0 {
		return Manifest{}, false
	}
	assertions, err := reader.FinalizedFiles(ctx, leaves)
	if err != nil || len(assertions) != len(leaves) {
		return Manifest{}, false
	}
	wanted := map[string]cloud.Item{}
	for _, leaf := range leaves {
		wanted[leaf.ID] = leaf
	}
	for _, assertion := range assertions {
		leaf, exists := wanted[assertion.Item.ID]
		if !exists || assertion.State != cloud.OfflineFinalizationComplete || assertion.Item.IsDir || leaf.ParentID != assertion.Item.ParentID || leaf.Name != assertion.Item.Name || leaf.Size != assertion.Item.Size {
			return Manifest{}, false
		}
		delete(wanted, leaf.ID)
	}
	return result, len(wanted) == 0
}
