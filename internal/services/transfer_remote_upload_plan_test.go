package services

import (
	"context"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func TestRemoteNodeUploadPlanFreezesConflictDecisionBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		policy string
		action string
		name   string
	}{
		{policy: models.MediaLibraryConflictAsk, action: remoteUploadFailIfExists, name: "电影/Movie (2024).mkv"},
		{policy: models.MediaLibraryConflictSkip, action: remoteUploadSkipIfExists, name: "电影/Movie (2024).mkv"},
		{policy: models.MediaLibraryConflictOverwrite, action: remoteUploadReplace, name: "电影/Movie (2024).mkv"},
		{policy: models.MediaLibraryConflictRename, action: remoteUploadFailIfExists, name: "电影/Movie (2024) (2).mkv"},
	} {
		t.Run(test.policy, func(t *testing.T) {
			fixture := newUploadTransferFixture(t, test.policy, true)
			worker := NewTransferWorker(fixture.service)
			planned, conflicts, err := worker.planRemoteNodeUpload(context.Background(), fixture.download, fixture.manifest, test.policy)
			if err != nil {
				t.Fatal(err)
			}
			if conflicts != 1 || len(planned) != 1 || planned[0].ConflictAction != test.action || planned[0].Relative != test.name {
				t.Fatalf("conflicts=%d plan=%+v", conflicts, planned)
			}
		})
	}
}

func TestRemoteNodeUploadPersistedPlanMustCoverExactFrozenSourceSet(t *testing.T) {
	fixture := newUploadTransferFixture(t, models.MediaLibraryConflictOverwrite, false)
	fixture.manifest.Files[0].RemoteFileToken = "file:movie"
	fixture.manifest.Files[0].SHA256 = strings.Repeat("a", 64)
	if err := fixture.service.Enqueue(fixture.download, fixture.manifest); err != nil {
		t.Fatal(err)
	}
	var task models.TransferTask
	if err := fixture.queue.db.Where("download_task_id = ?", fixture.download.ID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	planned, _, err := NewTransferWorker(fixture.service).planRemoteNodeUpload(context.Background(), fixture.download, fixture.manifest, models.MediaLibraryConflictOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewTransferWorker(fixture.service)
	if _, err := worker.createRemoteUploadFiles(context.Background(), task, planned); err != nil {
		t.Fatal(err)
	}
	if rows, found, err := worker.loadRemoteUploadFiles(context.Background(), task, fixture.download, fixture.manifest); err != nil || !found || len(rows) != 1 {
		t.Fatalf("rows=%+v found=%v err=%v", rows, found, err)
	}

	changed := fixture.manifest
	changed.Files = append(append([]downloadpkg.File(nil), changed.Files...), downloadpkg.File{
		RelativePath:    "Movie.2024.srt",
		Size:            123,
		RemoteFileToken: "file:subtitle",
		SHA256:          strings.Repeat("b", 64),
	})
	if _, found, err := worker.loadRemoteUploadFiles(context.Background(), task, fixture.download, changed); err == nil || !found {
		t.Fatalf("changed frozen source set accepted: found=%v err=%v", found, err)
	}
}

func TestRemoteNodeUploadPlanDoesNotMutateOrRenameWhenTargetDirectoryIsMissing(t *testing.T) {
	fixture := newUploadTransferFixture(t, models.MediaLibraryConflictRename, false)
	delete(fixture.driver.items, fixture.targetDirID)
	planned, conflicts, err := NewTransferWorker(fixture.service).planRemoteNodeUpload(context.Background(), fixture.download, fixture.manifest, models.MediaLibraryConflictRename)
	if err != nil {
		t.Fatal(err)
	}
	if conflicts != 0 || len(planned) != 1 || planned[0].Relative != "电影/Movie (2024).mkv" || planned[0].ConflictAction != remoteUploadFailIfExists {
		t.Fatalf("conflicts=%d plan=%+v", conflicts, planned)
	}
	if fixture.driver.uploadCalls != 0 {
		t.Fatalf("planning performed upload: %d", fixture.driver.uploadCalls)
	}
}
