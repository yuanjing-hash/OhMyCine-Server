package services

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestDownloadSettingsOwnUnifiedStagingBoundary(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	service := NewDownloadSettingsService(queue.db, queue.audit)
	if _, err := service.Get(actor); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("read error=%v", err)
	}
	actor.Permissions[authz.PermissionSettingsRead] = struct{}{}
	settings, err := service.Get(actor)
	if err != nil || settings.Configured {
		t.Fatalf("settings=%+v err=%v", settings, err)
	}
	if _, err := service.Snapshot(context.Background(), models.DownloaderTypeQBittorrent); ErrorCode(err) != CodeDownloadStagingRequired {
		t.Fatalf("snapshot error=%v", err)
	}
	actor.Permissions[authz.PermissionSettingsUpdate] = struct{}{}
	root := t.TempDir()
	staging := filepath.Join(root, "incoming")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	updated, err := service.Update(context.Background(), actor, UpdateDownloadSettingsInput{AbsolutePath: staging, Revision: 1}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Configured || updated.AbsolutePath != staging || updated.Revision != 2 {
		t.Fatalf("updated=%+v", updated)
	}
	current, err := service.Get(actor)
	if err != nil || current.AbsolutePath != staging {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	snapshot, err := service.Snapshot(context.Background(), models.DownloaderTypeQBittorrent)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AbsolutePath != staging {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	var record models.DownloadSettings
	if err := queue.db.First(&record, 1).Error; err != nil || record.AbsolutePath != staging || record.StorageID != nil {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	if _, err := service.Update(context.Background(), actor, UpdateDownloadSettingsInput{AbsolutePath: staging, Revision: math.MaxInt64}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("overflow error=%v", err)
	}
}
