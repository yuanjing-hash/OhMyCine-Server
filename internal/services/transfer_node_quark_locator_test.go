package services

import (
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestQuarkNodePlansUseFrozenDirectoryPaths(t *testing.T) {
	connectionID := uint(7)
	storage := models.Storage{ID: 9, Type: models.StorageTypeQuark, ConnectionID: &connectionID, RootPath: "storage-root"}
	source, err := cloud.EncodeSelectedShareSource("https://pan.quark.cn/s/abcd1234", cloud.ShareSelection{Version: 1, Files: []cloud.ShareTreeItem{{ID: "file-1", RelativePath: "movie.mkv", Size: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	task := models.DownloadTask{ID: "task-1", ProviderTag: "omc-task-1", TargetStorageType: models.StorageTypeLocal, StagingRelativePath: "/nested/intake"}
	plan, err := buildNodeStorageSourcePlan(storage, task, nodeprotocol.StorageSourceKindQuarkShareSelected, source, "intake-id")
	if err != nil || plan.DestinationRelativePath != task.StagingRelativePath || plan.OfflineDestinationID != "intake-id" {
		t.Fatalf("frozen source path missing: %v", err)
	}
	task.StagingRelativePath = ""
	if _, err = buildNodeStorageSourcePlan(storage, task, nodeprotocol.StorageSourceKindQuarkShareSelected, source, "intake-id"); err == nil {
		t.Fatal("child directory accepted without frozen path")
	}
	if plan, err = buildNodeStorageSourcePlan(storage, task, nodeprotocol.StorageSourceKindQuarkShareSelected, source, storage.RootPath); err != nil || plan.DestinationRelativePath != "/" {
		t.Fatalf("grant-root compatibility failed: %v", err)
	}

	rows := []models.RemoteUploadFile{{SourceFileToken: "file:one", TargetRelative: "Season 01/movie.mkv", Size: 10, SHA256: strings.Repeat("a", 64), ConflictAction: nodeprotocol.StorageConflictFailIfExists}}
	manifest := downloadpkg.Manifest{RemoteExportOperationKey: "export-1", RemoteExportDigest: strings.Repeat("b", 64)}
	task.TargetProviderRootID, task.TargetRelativeRoot = "library-id", "/nested/library"
	batches, err := buildRemoteUploadBatches(rows, storage, task, manifest)
	if err != nil || len(batches) != 1 || batches[0].plan.TargetRootRelativePath != task.TargetRelativeRoot || batches[0].plan.TargetRootID != task.TargetProviderRootID {
		t.Fatalf("frozen target path missing: %v", err)
	}
	task.TargetRelativeRoot = ""
	if _, err = buildRemoteUploadBatches(rows, storage, task, manifest); err == nil {
		t.Fatal("upload child accepted without frozen path")
	}
	task.TargetProviderRootID = storage.RootPath
	if batches, err = buildRemoteUploadBatches(rows, storage, task, manifest); err != nil || batches[0].plan.TargetRootRelativePath != "/" {
		t.Fatalf("upload root compatibility failed: %v", err)
	}
}
