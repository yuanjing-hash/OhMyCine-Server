package quark

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type directoryLocator struct {
	rootID, rootPath, relative string
}

// SetDirectoryLocator stores only an untrusted path hint. Each use independently
// checks the saved root and target identities against complete fresh listings.
// It lets a restarted task find its frozen intake directory without a tree scan.
func (d *Client) SetDirectoryLocator(rootID, rootPath, relative, expectedID string) error {
	if rootID == "" || expectedID == "" || rootPath == "" || relative == "" {
		return failure(CodeIdentity)
	}
	if _, err := pathSegments(rootPath); err != nil {
		return err
	}
	if _, err := pathSegments(relative); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.directoryLocators == nil {
		d.directoryLocators = map[string]directoryLocator{}
	}
	d.directoryLocators[expectedID] = directoryLocator{rootID, rootPath, relative}
	return nil
}

// ResolveScopedDirectory verifies both the configured Storage root and the
// directory below it. A display path never substitutes for either stable ID.
func (d *Client) ResolveScopedDirectory(ctx context.Context, rootID, rootPath, relative, expectedID string) (cloud.Item, error) {
	if rootPath == "" || relative == "" {
		return cloud.Item{}, failure(CodeIdentity)
	}
	parts, err := pathSegments(relative)
	if err != nil {
		return cloud.Item{}, err
	}
	root, err := d.resolveTV(ctx, rootPath)
	if err != nil {
		return cloud.Item{}, err
	}
	if root.ID != rootID || !root.IsDir {
		return cloud.Item{}, failure(CodeIdentity)
	}
	if len(parts) == 0 {
		if expectedID != rootID {
			return cloud.Item{}, failure(CodeIdentity)
		}
		return root, nil
	}
	item, err := d.ResolveScopedItem(ctx, rootID, "/"+strings.Join(parts, "/"), expectedID)
	if err == nil && !item.IsDir {
		return cloud.Item{}, failure(CodeIdentity)
	}
	return item, err
}

// ResolveScopedItem verifies a saved locator against fresh direct-child
// listings. TV download metadata does not include ancestry, so a cold driver
// must not infer it from Stat or search the account recursively. The expected
// stable identity prevents an old path from resolving a replacement file.
func (d *Client) ResolveScopedItem(ctx context.Context, rootID, relativePath, expectedID string) (cloud.Item, error) {
	auth, err := d.authorization(ctx, RouteTV)
	if err != nil {
		return cloud.Item{}, err
	}
	if _, err = parseID(rootID, RouteTV, auth.AccountID, false); err != nil {
		return cloud.Item{}, err
	}
	if _, err = parseID(expectedID, RouteTV, auth.AccountID, false); err != nil {
		return cloud.Item{}, err
	}
	if !strings.HasPrefix(relativePath, "/") || strings.HasSuffix(relativePath, "/") {
		return cloud.Item{}, failure(CodeIdentity)
	}
	parts, err := pathSegments(relativePath)
	if err != nil || len(parts) == 0 {
		return cloud.Item{}, failure(CodeIdentity)
	}
	parent := rootID
	var found cloud.Item
	for index, part := range parts {
		entries, err := d.tvDirectory(ctx, parent)
		if err != nil {
			return cloud.Item{}, err
		}
		matches := 0
		for _, entry := range entries {
			if entry.Name == part {
				found = entry
				matches++
			}
		}
		if matches != 1 || found.ParentID != parent || (index < len(parts)-1 && !found.IsDir) {
			return cloud.Item{}, failure(CodeIdentity)
		}
		parent = found.ID
	}
	if found.ID != expectedID {
		return cloud.Item{}, failure(CodeIdentity)
	}
	return found, nil
}
