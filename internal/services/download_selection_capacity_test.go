package services

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"gorm.io/gorm"
)

func TestDownloadSelectionGroupCapacityRejectsBeforeCommitWithoutPoisoningExistingRefs(t *testing.T) {
	_, service, queue, actor, _ := downloadFixture(t)
	config := models.Downloader{ID: uuid.NewString(), OwnerID: actor.User.ID, Name: "capacity", NameNormalized: "capacity", Type: models.DownloaderTypeQBittorrent, CapabilitiesJSON: "{}", Enabled: true}
	if err := queue.db.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := queue.db.First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	raw := []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	sel := &resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
	digest, _ := sel.Digest()
	source := downloadSourceEnvelope{Kind: downloadpkg.SourceTorrent, Torrent: raw, ContentSelection: sel, ContentSelectionDigest: digest, SelectionMode: "files"}
	plain, _ := json.Marshal(source)
	group := models.BTSelectionGroup{ID: strings.Repeat("a", 64), ScopeDigest: "capacity-scope", Generation: 1, OwnerID: actor.User.ID, DownloaderID: config.ID, Workspace: filepath.Join(t.TempDir(), "managed"), KnownDigestsJSON: "[]", Revision: 1}
	if err = queue.db.Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	jobs, tasks, refs := make([]models.Job, 1024), make([]models.DownloadTask, 1024), make([]models.BTSelectionRef, 1024)
	now := time.Now().UTC()
	for i := 0; i < 1024; i++ {
		id := uuid.NewString()
		jobID := uuid.NewString()
		cipher, e := service.credentials.Encrypt(downloadSourcePurpose(id), string(plain))
		if e != nil {
			t.Fatal(e)
		}
		jobs[i] = models.Job{ID: jobID, OwnerID: &actor.User.ID, JobType: "download", Status: models.JobStatusQueued, PayloadJSON: "{}"}
		tasks[i] = models.DownloadTask{ID: id, JobID: jobID, OwnerID: actor.User.ID, DownloaderID: &config.ID, ProviderType: config.Type, ProviderTag: "omc-" + id, ProfileID: profile.ID, SourceCiphertext: cipher, BTSelectionGroupID: group.ID, StagingAbsolutePath: group.Workspace, Phase: models.DownloadTaskStatusDownloading}
		refs[i] = models.BTSelectionRef{TaskID: id, GroupID: group.ID, OwnDigest: digest, State: "running", Revision: 1, CreatedAt: now, UpdatedAt: now}
	}
	if err = queue.db.Transaction(func(tx *gorm.DB) error {
		if e := tx.CreateInBatches(&jobs, 50).Error; e != nil {
			return e
		}
		if e := tx.CreateInBatches(&tasks, 50).Error; e != nil {
			return e
		}
		return tx.CreateInBatches(&refs, 50).Error
	}); err != nil {
		t.Fatal(err)
	}
	overflow := tasks[0]
	overflow.ID = uuid.NewString()
	overflow.JobID = uuid.NewString()
	overflow.ProviderTag = "omc-" + overflow.ID
	overflow.SourceCiphertext, err = service.credentials.Encrypt(downloadSourcePurpose(overflow.ID), string(plain))
	if err != nil {
		t.Fatal(err)
	}
	err = queue.db.Transaction(func(tx *gorm.DB) error {
		job := models.Job{ID: overflow.JobID, OwnerID: &actor.User.ID, JobType: "download", Status: models.JobStatusQueued, PayloadJSON: "{}"}
		if e := tx.Create(&job).Error; e != nil {
			return e
		}
		if e := tx.Create(&overflow).Error; e != nil {
			return e
		}
		return attachBTSelectionRef(tx, &group, overflow, source, service.credentials)
	})
	if code, _ := downloadpkg.ErrorInfo(err); code != "downloader_selection_group_limit" {
		t.Fatalf("1025th reference was not rejected before commit: %s %v", code, err)
	}
	for _, table := range []any{&models.DownloadTask{}, &models.Job{}, &models.BTSelectionRef{}} {
		var count int64
		if e := queue.db.Model(table).Count(&count).Error; e != nil || count != 1024 {
			t.Fatal("overflow left committed state", fmt.Sprintf("%T", table), count, e)
		}
	}
	provider := &groupTestProvider{metadataDownloadClient: &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}}
	bound, e := service.bindBTSelectionGroup(provider, config, tasks[0], downloadpkg.Source{Kind: source.Kind, Torrent: raw, ContentSelection: sel})
	if e != nil {
		t.Fatal(e)
	}
	if got, e := bound.Get(context.Background(), ""); e != nil || got.BytesTotal == nil || *got.BytesTotal != 10 || len(provider.last.Union.Files) != 1 {
		t.Fatal("rejected ref poisoned existing control/recovery", got, e)
	}
	var before models.BTSelectionGroup
	if e = queue.db.First(&before, "id = ?", group.ID).Error; e != nil {
		t.Fatal(e)
	}
	if e = queue.db.Transaction(func(tx *gorm.DB) error {
		return attachBTSelectionRef(tx, &group, tasks[0], source, service.credentials)
	}); e != nil {
		t.Fatal("full group blocked original ref idempotent recovery", e)
	}
	var after models.BTSelectionGroup
	if e = queue.db.First(&after, "id = ?", group.ID).Error; e != nil || after.Revision != before.Revision {
		t.Fatal("idempotent recovery changed group revision", e)
	}
	// Released refs remain lifetime authority/history, so releasing one does
	// not make room for an unbounded sequence that later blocks cleanup.
	if e = queue.db.Model(&models.BTSelectionRef{}).Where("task_id = ?", tasks[1].ID).Update("state", "released").Error; e != nil {
		t.Fatal(e)
	}
	if e = queue.db.Transaction(func(tx *gorm.DB) error {
		return attachBTSelectionRef(tx, &group, overflow, source, service.credentials)
	}); e == nil {
		t.Fatal("released history bypassed lifecycle cap")
	}
}
