package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type coldQuarkPlaybackDriver struct {
	*fakeCloudDriver
	calls                      int
	wantRoot, wantPath, wantID string
}

func (d *coldQuarkPlaybackDriver) Stat(context.Context, string) (cloud.Item, error) {
	return cloud.Item{}, cloud.Error(cloud.CodeUnavailable, false, nil)
}
func (d *coldQuarkPlaybackDriver) ResolveScopedItem(_ context.Context, root, path, id string) (cloud.Item, error) {
	d.calls++
	if root != d.wantRoot || path != d.wantPath || id != d.wantID {
		return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
	}
	return cloud.Item{ID: id, ParentID: "nested-parent", Name: "movie.mkv", Size: 128}, nil
}

func TestQuarkPlaybackUsesEffectiveCatalogPathWithColdDriver(t *testing.T) {
	store, library, recognition, entries := catalogFixture(t)
	catalogConvert(t, store, library, recognition, entries)
	entry := entries[0]
	driver := &coldQuarkPlaybackDriver{fakeCloudDriver: &fakeCloudDriver{}, wantRoot: "root", wantPath: entry.RelativePath, wantID: entry.ProviderID}
	svc := &SignedProxyService{db: store.writeDB, catalogStore: store}
	target := signedProxyTarget{LibraryID: library.ID, ProviderItemID: entry.ProviderID, ProviderRootID: "root", StorageType: models.StorageTypeQuark}
	item, err := svc.resolveQuarkArtifactItem(context.Background(), driver, target)
	if err != nil || item.ID != entry.ProviderID || driver.calls != 1 {
		t.Fatalf("cold artifact locator: item=%+v calls=%d err=%v", item, driver.calls, err)
	}
	// Anchor rows are not an alternate source of truth after conversion.
	if err := store.writeDB.Model(&models.MediaLibraryEntry{}).Where("id=?", entry.ID).Update("relative_path", "/wrong-anchor.mkv").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.resolveQuarkArtifactItem(context.Background(), driver, target); err != nil {
		t.Fatalf("played legacy anchor instead of pinned catalog: %v", err)
	}
	target.ProviderItemID = "not-in-catalog"
	if _, err := svc.resolveQuarkArtifactItem(context.Background(), driver, target); ErrorCode(err) != CodeProxyTargetUnavailable {
		t.Fatalf("unknown identity resolved: %v", err)
	}
}
