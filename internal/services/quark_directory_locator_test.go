package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

type coldQuarkDirectoryDriver struct {
	*fakeCloudDriver
	got [4]string
}

func (d *coldQuarkDirectoryDriver) ResolveScopedDirectory(_ context.Context, root, path, relative, id string) (cloud.Item, error) {
	d.got = [4]string{root, path, relative, id}
	return cloud.Item{ID: id, IsDir: true}, nil
}

func TestQuarkDirectoryAdmissionUsesFrozenLocatorInsteadOfColdStat(t *testing.T) {
	d := &coldQuarkDirectoryDriver{fakeCloudDriver: &fakeCloudDriver{}}
	storage := models.Storage{Type: models.StorageTypeQuark, RootPath: "canonical-root", RootDisplayPath: "/library"}
	item, err := providerDirectoryWithinStorage(context.Background(), d, storage, "/nested/intake", "canonical-intake")
	if err != nil || item.ID != "canonical-intake" || d.statCalls != 0 || d.got != [4]string{"canonical-root", "/library", "/nested/intake", "canonical-intake"} {
		t.Fatalf("cold admission did not verify exact path: %v", err)
	}
}

func TestQuarkTaskClientRetainsFrozenIntakeAfterDownloaderEdit(t *testing.T) {
	f, _, _ := quarkIntakeFixture(t)
	var storage models.Storage
	if err := f.db.First(&storage, f.storage.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.downloads.downloader.connections.drivers[*storage.ConnectionID] = &quarkPreviewFixtureDriver{&previewFixtureDriver{f.driver}}
	var captured downloadpkg.Config
	f.downloads.downloader.registry = downloadpkg.NewRegistry()
	if err := f.downloads.downloader.registry.Register(models.DownloaderTypeQuarkShare, downloadpkg.Capabilities{ShareReceive: true}, func(config downloadpkg.Config) (downloadpkg.Client, error) {
		captured = config
		return &stubDownloadClient{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	record := f.downloader
	record.Type = models.DownloaderTypeQuarkShare
	record.ProviderDirectoryID, record.ProviderDirectoryPath = "changed-intake", "/changed"
	task := models.DownloadTask{StagingStorageID: &storage.ID, StagingProviderDirectoryID: "frozen-intake", StagingRelativePath: "/frozen"}
	if _, err := f.downloads.downloader.clientForTask(context.Background(), record, task, ""); err != nil {
		t.Fatal(err)
	}
	if captured.ProviderDirectoryID != "frozen-intake" || captured.ProviderDirectoryPath != "/frozen" || captured.ProviderStorageRootID != storage.RootPath || captured.ProviderStorageRootPath != storage.RootDisplayPath {
		t.Fatal("task intake drifted to edited downloader")
	}
	otherStorage := storage.ID + 1
	record.StorageID = &otherStorage
	if _, err := f.downloads.downloader.clientForTask(context.Background(), record, task, ""); ErrorCode(err) != CodeDownloaderUnavailable {
		t.Fatal("task changed storage without rejection", err)
	}
}
