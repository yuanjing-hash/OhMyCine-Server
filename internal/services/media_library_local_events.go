package services

import (
	"context"
	"errors"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"strings"
)

func localEventRelativePath(root, name string) (string, error) {
	constrained, err := storagefs.Constrain(root, name)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, constrained)
	if err != nil {
		return "", err
	}
	if relative == "." {
		return "", errProviderChangeScopeUnproven
	}
	return "/" + filepath.ToSlash(relative), nil
}

func (s *MediaLibraryService) knownLocalCatalogPaths(ctx context.Context, id uint, storage models.Storage, library models.MediaLibrary, names []string) (map[string][]string, error) {
	root, err := medialibrary.ResolveRoot(storage.RootPath, library.RelativeRoot)
	if err != nil {
		return nil, err
	}
	known := map[string][]string{}
	err = s.withCatalogRead(ctx, []uint{id}, func(tx *gorm.DB, reader *CatalogReader) error {
		for _, name := range names {
			relative, err := localEventRelativePath(root, name)
			if err != nil {
				return err
			}
			prefix := relative + "/"
			var entries []models.MediaLibraryEntry
			if err := reader.Entries().Where("relative_path = ? OR substr(relative_path, 1, ?) = ?", relative, len([]rune(prefix)), prefix).Limit(maxPan115ScopedEntries + 1).Find(&entries).Error; err != nil {
				return err
			}
			var assets []models.MediaLibrarySourceAsset
			if err := reader.SourceAssets().Where("relative_path = ? OR substr(relative_path, 1, ?) = ?", relative, len([]rune(prefix)), prefix).Limit(maxPan115ScopedEntries + 1).Find(&assets).Error; err != nil {
				return err
			}
			if len(entries)+len(assets) > maxPan115ScopedEntries {
				return errProviderChangeScopeUnproven
			}
			for _, entry := range entries {
				known[relative] = append(known[relative], entry.ProviderID)
			}
			for _, asset := range assets {
				known[relative] = append(known[relative], asset.ProviderID)
			}
		}
		return nil
	})
	return known, err
}

func scanLocalEventScope(ctx context.Context, request MediaLibraryScanRequest) (medialibrary.Result, error) {
	if request.providerScope == nil || request.providerScope.Blocked || len(request.providerScope.LocalPaths) == 0 {
		return medialibrary.Result{}, errProviderChangeScopeUnproven
	}
	root, err := medialibrary.ResolveRoot(request.Storage.RootPath, request.Library.RelativeRoot)
	if err != nil {
		return medialibrary.Result{}, err
	}
	result := medialibrary.Result{Partial: true}
	seen := map[string]struct{}{}
	visited := 0
	allExtensions := append(append([]string{}, request.VideoExtensions...), request.AssetExtensions...)
	videos := map[string]bool{}
	for _, ext := range request.VideoExtensions {
		videos[strings.ToLower(ext)] = true
	}
	inspect := func(name string) error {
		if _, ok := seen[name]; ok {
			return nil
		}
		seen[name] = struct{}{}
		if len(seen) > maxPan115ScopedEntries {
			return errProviderChangeScopeUnproven
		}
		file, keep, err := medialibrary.InspectLocalFile(ctx, root, name, allExtensions, request.IgnorePatterns)
		if err != nil {
			return err
		}
		if !keep {
			return nil
		}
		result.Enumerated++
		if videos[strings.ToLower(filepath.Ext(name))] {
			result.Files = append(result.Files, file)
		} else {
			result.Assets = append(result.Assets, medialibrary.SourceAsset{ProviderID: file.ProviderID, RelativePath: file.RelativePath, Name: filepath.Base(name), Extension: strings.ToLower(filepath.Ext(name)), Size: file.Size, ModifiedAt: file.ModifiedAt, HashHint: file.ProviderID})
		}
		return nil
	}
	for _, name := range request.providerScope.LocalPaths {
		relative, err := localEventRelativePath(root, name)
		if err != nil {
			return medialibrary.Result{}, err
		}
		// Inspection validates every existing ancestor before reporting a missing
		// target, so deleted paths cannot use a symlink to escape the library.
		_, _, inspectErr := medialibrary.InspectLocalFile(ctx, root, name, allExtensions, request.IgnorePatterns)
		if inspectErr != nil {
			if !errors.Is(inspectErr, os.ErrNotExist) {
				return medialibrary.Result{}, inspectErr
			}
			result.DeletedProviderIDs = append(result.DeletedProviderIDs, request.knownLocalPaths[relative]...)
			continue
		}
		info, err := os.Lstat(name)
		if err != nil {
			return medialibrary.Result{}, err
		}
		if info.IsDir() {
			if !request.Library.Recursive {
				continue
			}
			err = filepath.WalkDir(name, func(current string, entry os.DirEntry, walkErr error) error {
				visited++
				if visited > maxPan115ScopedEntries {
					return errProviderChangeScopeUnproven
				}
				if walkErr != nil {
					return walkErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if medialibrary.IsUnsafeDirectory(current, entry) {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if entry.IsDir() {
					return nil
				}
				return inspect(current)
			})
		} else {
			err = inspect(name)
		}
		if err != nil {
			return medialibrary.Result{}, err
		}
	}
	return result, nil
}
