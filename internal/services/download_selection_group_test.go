package services

import (
	"context"
	"encoding/json"
	"github.com/rs/zerolog"
	"os"
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

type groupTestProvider struct {
	*metadataDownloadClient
	last                        downloadpkg.SelectionGroup
	adds, applies               int
	failAfterApply              bool
	cleanupRequests             int
	cleanupGone, cleanupAckLost bool
}

func (p *groupTestProvider) ApplySelectionGroup(_ context.Context, r downloadpkg.SubmitRequest, g downloadpkg.SelectionGroup) (downloadpkg.Task, error) {
	p.applies++
	if p.adds == 0 {
		p.adds++
	}
	p.last = g
	if p.failAfterApply {
		p.failAfterApply = false
		return downloadpkg.Task{}, downloadpkg.Error("fixture_ack_lost", true, nil)
	}
	return downloadpkg.Task{ID: "shared-native", Status: "running"}, nil
}
func (p *groupTestProvider) GetSelected(_ context.Context, _ string, b downloadpkg.SelectionContext) (downloadpkg.Task, error) {
	if b.Group == nil {
		panic("lost group authority")
	}
	total := int64(0)
	for _, f := range b.Selection.Files {
		total += f.Size
	}
	return downloadpkg.Task{ID: "shared-native", BytesTotal: &total}, nil
}
func (p *groupTestProvider) ManifestSelected(_ context.Context, _ string, b downloadpkg.SelectionContext) (downloadpkg.Manifest, error) {
	m := downloadpkg.Manifest{Complete: true, Name: b.RootName}
	for _, f := range b.Selection.Files {
		m.Files = append(m.Files, downloadpkg.File{RelativePath: b.RootName + "/" + f.Path, Size: f.Size})
	}
	return m, nil
}
func (p *groupTestProvider) PauseSelected(context.Context, string, downloadpkg.SelectionContext) error {
	panic("per-ref native pause")
}
func (p *groupTestProvider) ResumeSelected(context.Context, string, downloadpkg.SelectionContext) error {
	panic("per-ref native resume")
}
func (p *groupTestProvider) CancelSelected(context.Context, string, downloadpkg.SelectionContext, bool) error {
	panic("per-ref native cancel")
}

func (p *groupTestProvider) CleanupSelectionGroup(_ context.Context, _ downloadpkg.Source, _ downloadpkg.SelectionContext, cleanup downloadpkg.GroupCleanup) (bool, error) {
	if cleanup.ProbeOnly {
		return p.cleanupGone, nil
	}
	p.cleanupRequests++
	p.cleanupGone = true
	if p.cleanupAckLost {
		p.cleanupAckLost = false
		return false, downloadpkg.Error("fixture_cleanup_ack_lost", true, nil)
	}
	return true, nil
}

func TestDownloadSelectionGroupDurableUnionLateRestoreCancelAndAckRecovery(t *testing.T) {
	downloads, service, queue, actor, _ := downloadFixture(t)
	provider := &groupTestProvider{metadataDownloadClient: &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}}
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, downloadpkg.Capabilities{SelectiveFiles: true, SelectiveFileGroups: true, Seeding: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return provider, nil }); err != nil {
		t.Fatal(err)
	}
	service.registry = registry
	config := models.Downloader{ID: uuid.NewString(), OwnerID: actor.User.ID, Name: "qB", NameNormalized: "qb", Type: models.DownloaderTypeQBittorrent, CapabilitiesJSON: `{"seeding":true}`, Enabled: true}
	config.UsernameCiphertext, _ = service.credentials.Encrypt(downloaderPurpose(config.ID, "username"), "")
	config.PasswordCiphertext, _ = service.credentials.Encrypt(downloaderPurpose(config.ID, "password"), "")
	if err := queue.db.Create(&config).Error; err != nil {
		t.Fatal(err)
	}
	info := []byte("d5:filesld6:lengthi10e4:pathl5:a.mkveed6:lengthi20e4:pathl5:b.mkveee4:name4:Pack12:piece lengthi16384e6:pieces20:" + strings.Repeat("a", 20) + "e")
	raw := append(append([]byte("d4:info"), info...), 'e')
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "downloads")
	makeTask := func(index int, size int64, name string) (models.DownloadTask, downloadpkg.Source, *models.BTSelectionGroup) {
		sel := &resourcecontent.Selection{Version: 1, Identity: manifest.Identity, Files: []resourcecontent.SelectedFile{{Index: index, Path: name, Size: size}}}
		digest, _ := sel.Digest()
		envelope := downloadSourceEnvelope{Kind: downloadpkg.SourceTorrent, Torrent: raw, ContentSelection: sel, ContentSelectionDigest: digest, SelectionMode: "files"}
		group, e := downloads.prepareBTSelectionGroup(actor.User.ID, config, envelope, root, SubmitDownloadInput{SourceSiteID: 7})
		if e != nil {
			t.Fatal(e)
		}
		id := uuid.NewString()
		plain, _ := json.Marshal(envelope)
		cipher, e := service.credentials.Encrypt(downloadSourcePurpose(id), string(plain))
		if e != nil {
			t.Fatal(e)
		}
		task := models.DownloadTask{ID: id, JobID: uuid.NewString(), OwnerID: actor.User.ID, DownloaderID: &config.ID, DownloaderName: config.Name, ProviderType: config.Type, SourceCiphertext: cipher, ProviderTag: "omc-" + id, BTSelectionGroupID: group.ID, StagingAbsolutePath: group.Workspace, Phase: models.DownloadTaskStatusDownloading, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if e = queue.db.Transaction(func(tx *gorm.DB) error {
			job := models.Job{ID: task.JobID, OwnerID: &task.OwnerID, JobType: "download", Status: models.JobStatusQueued, PayloadJSON: "{}"}
			if err := tx.Create(&job).Error; err != nil {
				return err
			}
			var profile models.MediaClassificationProfile
			if err := tx.First(&profile).Error; err != nil {
				return err
			}
			task.ProfileID = profile.ID
			if err := tx.Create(&task).Error; err != nil {
				return err
			}
			return attachBTSelectionRef(tx, group, task, envelope)
		}); e != nil {
			t.Fatal(e)
		}
		return task, downloadpkg.Source{Kind: envelope.Kind, Torrent: raw, ContentSelection: sel}, group
	}
	a, sourceA, groupA := makeTask(0, 10, "a.mkv")
	boundA, err := service.bindBTSelectionGroup(provider, config, a, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = boundA.Submit(context.Background(), downloadpkg.SubmitRequest{Source: sourceA, Tag: a.ProviderTag, SavePath: a.StagingAbsolutePath}); err != nil {
		t.Fatal(err)
	}
	if err = boundA.(downloadpkg.Resumer).Resume(context.Background(), "shared-native"); err != nil {
		t.Fatal(err)
	}
	b, sourceB, groupB := makeTask(1, 20, "b.mkv")
	if groupB.ID != groupA.ID || b.StagingAbsolutePath != a.StagingAbsolutePath {
		t.Fatal("same trusted source got different workspace")
	}
	boundB, err := service.bindBTSelectionGroup(provider, config, b, sourceB)
	if err != nil {
		t.Fatal(err)
	}
	provider.failAfterApply = true
	if _, err = boundB.Submit(context.Background(), downloadpkg.SubmitRequest{Source: sourceB, Tag: b.ProviderTag, SavePath: b.StagingAbsolutePath}); err == nil {
		t.Fatal("missing ACK not exposed")
	}
	// A newly constructed service/client represents a restart. Its registry
	// comes from persisted refs, not the pre-E15 instance of A's own source.
	restarted := NewDownloaderService(queue.db, queue.audit, service.credentials, registry)
	restored, err := restarted.bindBTSelectionGroup(provider, config, a, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restored.Get(context.Background(), "shared-native")
	if err != nil || *got.BytesTotal != 10 || len(provider.last.Union.Files) != 2 || !provider.last.Resume || provider.adds != 1 {
		t.Fatal(got, err, provider.last)
	}
	if err = boundB.(downloadpkg.Resumer).Resume(context.Background(), "shared-native"); err != nil {
		t.Fatal(err)
	}
	// One ref is delivered while another still consumes the package. A shared
	// frozen move uses copy first, and must not remove either source yet.
	storage := models.Storage{Name: "cleanup-target", NameNormalized: "cleanup-target", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: "cleanup-target", Capabilities: "{}", Enabled: true}
	if err = queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "cleanup-library", NameNormalized: "cleanup-library", StorageID: storage.ID, ProfileID: b.ProfileID, ProfileRevision: 1, RelativeRoot: "/", TransferMode: models.MediaLibraryTransferMove, ConflictPolicy: models.MediaLibraryConflictRename, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`}
	if err = queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	if err = queue.db.Model(&models.DownloadTask{}).Where("id IN ?", []string{a.ID, b.ID}).Updates(map[string]any{"transfer_mode": models.MediaLibraryTransferMove, "target_library_id": library.ID}).Error; err != nil {
		t.Fatal(err)
	}
	job := models.Job{ID: uuid.NewString(), OwnerID: &b.OwnerID, JobType: "transfer", Status: models.JobStatusCompleted, PayloadJSON: "{}"}
	if err = queue.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	transfer := models.TransferTask{ID: uuid.NewString(), JobID: job.ID, OwnerID: b.OwnerID, DownloadTaskID: b.ID, LibraryID: library.ID, LibraryName: library.Name, ManifestJSON: "{}", Phase: models.TransferTaskStatusCompleted, CatalogPublishedAt: &now}
	if err = queue.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(b.StagingAbsolutePath, "Pack")
	if err = os.MkdirAll(pack, 0700); err != nil {
		t.Fatal(err)
	}
	fileA, fileB, extra := filepath.Join(pack, "a.mkv"), filepath.Join(pack, "b.mkv"), filepath.Join(pack, "unrelated.txt")
	for file, payload := range map[string][]byte{fileA: make([]byte, 10), fileB: make([]byte, 20), extra: []byte("retain")} {
		if err = os.WriteFile(file, payload, 0600); err != nil {
			t.Fatal(err)
		}
	}
	b.TransferMode = models.MediaLibraryTransferMove
	b.ProviderTaskID = "shared-native"
	b.TargetLibraryID = &library.ID
	if err = queue.db.Model(&models.DownloadTask{}).Where("id = ?", b.ID).Update("provider_task_id", b.ProviderTaskID).Error; err != nil {
		t.Fatal(err)
	}
	seeding := NewSeedingService(queue.db, queue.audit, queue, service, zerolog.Nop())
	if err = seeding.Enqueue(b); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"seeding"})
	if err != nil || claimed == nil {
		t.Fatal("group did not use existing cleanup job", err)
	}
	result := NewSeedingWorker(seeding).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.RetryAt == nil || provider.cleanupRequests != 0 {
		t.Fatal("cleanup job did not defer active consumer", result)
	}
	cleanupB := boundB.(interface {
		retireAndCleanup(context.Context) (bool, error)
	})
	if done, e := cleanupB.retireAndCleanup(context.Background()); e != nil || done || provider.cleanupRequests != 0 {
		t.Fatal("cleaned while A consumed", done, e)
	}
	if _, err = os.Stat(fileB); err != nil {
		t.Fatal("delivered ref source removed early", err)
	}
	if err = boundA.Cancel(context.Background(), "shared-native", false); err != nil {
		t.Fatal(err)
	}
	if len(provider.last.Union.Files) != 1 || provider.last.Union.Files[0].Path != "b.mkv" || provider.last.Resume {
		t.Fatal("cancel harmed B", provider.last)
	}
	if _, err = restored.Get(context.Background(), "shared-native"); err == nil {
		t.Fatal("late A context resurrected released ref")
	}
	got, err = boundB.Get(context.Background(), "shared-native")
	if err != nil || *got.BytesTotal != 20 {
		t.Fatal(got, err)
	}
	var own models.BTSelectionRef
	if err = queue.db.First(&own, "task_id = ?", a.ID).Error; err != nil || own.State != "released" {
		t.Fatal(own, err)
	}
	plain, _ := service.credentials.Decrypt(downloadSourcePurpose(a.ID), a.SourceCiphertext)
	var original downloadSourceEnvelope
	_ = json.Unmarshal([]byte(plain), &original)
	if len(original.ContentSelection.Files) != 1 || original.ContentSelection.Files[0].Path != "a.mkv" {
		t.Fatal("union overwrote own source")
	}
	// A copy/retain reference in the same group keeps the move ref explicitly
	// deferred. Nothing claims source cleanup completion while it still holds.
	c, sourceC, _ := makeTask(0, 10, "a.mkv")
	if err = queue.db.Model(&models.DownloadTask{}).Where("id = ?", c.ID).Update("transfer_mode", models.MediaLibraryTransferCopy).Error; err != nil {
		t.Fatal(err)
	}
	boundC, e := service.bindBTSelectionGroup(provider, config, c, sourceC)
	if e != nil {
		t.Fatal(e)
	}
	if done, e := cleanupB.retireAndCleanup(context.Background()); e != nil || done || provider.cleanupRequests != 0 {
		t.Fatal("mixed retain policy cleaned source", done, e)
	}
	var deferred models.TransferTask
	if e = queue.db.First(&deferred, "id = ?", transfer.ID).Error; e != nil || deferred.CleanupStatus == models.TransferCleanupCompleted {
		t.Fatal("move cleanup falsely completed", e)
	}
	if _, e = os.Stat(fileB); e != nil {
		t.Fatal("move source vanished during retained ref", e)
	}
	if e = boundC.Cancel(context.Background(), "shared-native", false); e != nil {
		t.Fatal(e)
	}
	provider.cleanupAckLost = true
	if done, e := cleanupB.retireAndCleanup(context.Background()); e == nil || done || provider.cleanupRequests != 1 {
		t.Fatal("unknown native cleanup not retained", done, e)
	}
	if _, e := downloads.prepareBTSelectionGroup(actor.User.ID, config, downloadSourceEnvelope{Kind: sourceB.Kind, Torrent: sourceB.Torrent, ContentSelection: sourceB.ContentSelection, SelectionMode: "files"}, root, SubmitDownloadInput{SourceSiteID: 7}); e == nil {
		t.Fatal("new admission overlapped uncertain cleanup")
	}
	// Reopen the authority and reconcile the original deletion. There is no
	// second native mutation, and only the delivered move ref's file is removed.
	recoveredSeeding := NewSeedingService(queue.db, queue.audit, queue, restarted, zerolog.Nop())
	result = NewSeedingWorker(recoveredSeeding).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if result.ErrorCode != "" || result.RetryAt != nil || provider.cleanupRequests != 1 || provider.adds != 1 {
		t.Fatal("cleanup job did not recover original receipt", result)
	}
	var seed models.SeedingTask
	if e = queue.db.Where("download_task_id = ?", b.ID).First(&seed).Error; e != nil || seed.Phase != models.SeedingTaskStatusCompleted {
		t.Fatal("cleanup job did not finish", seed, e)
	}
	if _, e = os.Stat(fileB); !os.IsNotExist(e) {
		t.Fatal("frozen move source not eventually cleaned", e)
	}
	for _, file := range []string{fileA, extra} {
		if _, e = os.Stat(file); e != nil {
			t.Fatal("cleanup removed protected source", file, e)
		}
	}
	next, e := downloads.prepareBTSelectionGroup(actor.User.ID, config, downloadSourceEnvelope{Kind: sourceB.Kind, Torrent: sourceB.Torrent, ContentSelection: sourceB.ContentSelection, SelectionMode: "files"}, root, SubmitDownloadInput{SourceSiteID: 7})
	if e != nil || next.Generation != groupB.Generation+1 || next.Workspace == groupB.Workspace {
		t.Fatal("cleanup lifecycle reused root", next, e)
	}
	newFile := filepath.Join(next.Workspace, "Pack", "b.mkv")
	if e = os.MkdirAll(filepath.Dir(newFile), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(newFile, make([]byte, 20), 0600); e != nil {
		t.Fatal(e)
	}
	if done, e := cleanupB.retireAndCleanup(context.Background()); e != nil || !done {
		t.Fatal(done, e)
	}
	if _, e = os.Stat(newFile); e != nil || provider.cleanupRequests != 1 {
		t.Fatal("late cleanup touched new generation", e)
	}
}

func TestDownloadSelectionGroupScopeExcludesProfileAndTarget(t *testing.T) {
	downloads, service, _, actor, _ := downloadFixture(t)
	registry := downloadpkg.NewRegistry()
	_ = registry.Register("group", downloadpkg.Capabilities{SelectiveFileGroups: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return nil, nil })
	service.registry = registry
	source := downloadSourceEnvelope{Kind: downloadpkg.SourceTorrent, SelectionMode: "files", ContentSelection: &resourcecontent.Selection{Version: 1, Identity: resourcecontent.Identity{V1: strings.Repeat("a", 40)}}}
	r := models.Downloader{ID: "executor", Type: "group"}
	input := SubmitDownloadInput{SourceSiteID: 8, ProfileID: 1}
	root := filepath.Join(t.TempDir(), "root")
	a, err := downloads.prepareBTSelectionGroup(actor.User.ID, r, source, root, input)
	if err != nil {
		t.Fatal(err)
	}
	input.ProfileID = 9
	target := uint(19)
	input.MediaLibraryID = &target
	b, err := downloads.prepareBTSelectionGroup(actor.User.ID, r, source, root, input)
	if err != nil || a.ID != b.ID {
		t.Fatal("profile target drift split source workspace", err)
	}
	input.SourceSiteID++
	c, err := downloads.prepareBTSelectionGroup(actor.User.ID, r, source, root, input)
	if err != nil || a.ID == c.ID {
		t.Fatal("trusted provenance was omitted", err)
	}
}
