package services

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestCompletedManifestLegacyEscapeIsReadOnlyAndRequiresPersistentSelectionAbsence(t *testing.T) {
	_, service, _, actor, _ := downloadFixture(t)
	provider := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}
	if err := service.registry.Register(models.DownloaderTypeFake, downloadpkg.Capabilities{}, func(downloadpkg.Config) (downloadpkg.Client, error) { return provider, nil }); err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(actor, DownloaderInput{Name: "legacy-recovery", Type: models.DownloaderTypeFake, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var config models.Downloader
	if err = service.db.First(&config, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	libID := uint(1)
	task := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, DownloaderID: &config.ID, ExecutionLocation: models.NodeLocationServer, ProviderTaskID: "already-completed", Phase: models.DownloadTaskStatusFailed, ScrapeStatus: "completed_unrecognized", TargetLibraryID: &libID, SourceCiphertext: "legacy-source-no-longer-readable"}
	ctx := withCompletedManifestRecovery(context.Background())
	client, err := service.clientForTask(ctx, config, task, "")
	if err != nil {
		t.Fatal("legacy completed manifest backfill blocked", err)
	}
	manifestClient, ok := client.(downloadpkg.ManifestClient)
	if !ok {
		t.Fatal("no legacy manifest capability")
	}
	if _, err = manifestClient.Manifest(ctx, task.ProviderTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Get(ctx, task.ProviderTaskID); err == nil {
		t.Fatal("legacy escape exposed Get")
	}
	if _, err = client.Submit(ctx, downloadpkg.SubmitRequest{}); err == nil {
		t.Fatal("legacy escape exposed Submit")
	}
	if err = client.Cancel(ctx, task.ProviderTaskID, true); err == nil {
		t.Fatal("legacy escape exposed Cancel")
	}
	if err = client.(downloadpkg.Pauser).Pause(ctx, task.ProviderTaskID); err == nil {
		t.Fatal("legacy escape exposed Pause")
	}
	if err = client.(downloadpkg.Resumer).Resume(ctx, task.ProviderTaskID); err == nil {
		t.Fatal("legacy escape exposed Resume")
	}
	if _, err = manifestClient.Manifest(ctx, "different-provider-task"); err == nil {
		t.Fatal("legacy escape changed native identity")
	}
	if provider.manifestCalls != 1 || provider.gets != 0 || provider.submits != 0 || provider.cancelled {
		t.Fatal("legacy escape called a forbidden provider method")
	}
	if _, err = service.clientForTask(context.Background(), config, task, ""); err == nil {
		t.Fatal("ordinary execution borrowed the read-only escape")
	}
	task.ContentSelectionRequired = true
	if _, err = service.clientForTask(ctx, config, task, ""); err == nil {
		t.Fatal("unreadable new selected source became legacy whole manifest")
	}
	task.SourceCiphertext = ""
	if _, err = service.clientForTask(ctx, config, task, ""); err == nil {
		t.Fatal("missing new selected source became legacy whole manifest")
	}
	legacyRaw, _ := json.Marshal(downloadSourceEnvelope{Kind: downloadpkg.SourceURL, URL: "magnet:legacy"})
	task.SourceCiphertext, err = service.credentials.Encrypt(downloadSourcePurpose(task.ID), string(legacyRaw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.clientForTask(ctx, config, task, ""); err == nil {
		t.Fatal("required marker with missing selection became whole manifest")
	}
	raw := []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	sel := &resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
	digest, _ := sel.Digest()
	selectedRaw, _ := json.Marshal(downloadSourceEnvelope{Kind: downloadpkg.SourceTorrent, Torrent: raw, ContentSelection: sel, ContentSelectionDigest: digest, SelectionMode: "files"})
	task.SourceCiphertext, err = service.credentials.Encrypt(downloadSourcePurpose(task.ID), string(selectedRaw))
	if err != nil {
		t.Fatal(err)
	}
	task.ContentSelectionRequired = false
	if _, err = service.clientForTask(ctx, config, task, ""); err == nil {
		t.Fatal("readable selected source with lost persistent marker was accepted")
	}
	task.BTSelectionGroupID = "managed-group"
	task.SourceCiphertext = "legacy-source-no-longer-readable"
	if _, err = service.clientForTask(ctx, config, task, ""); err == nil {
		t.Fatal("group did not independently force required selection")
	}
	if provider.manifestCalls != 1 || provider.gets != 0 || provider.submits != 0 {
		t.Fatal("rejected selected recovery touched provider")
	}
}
