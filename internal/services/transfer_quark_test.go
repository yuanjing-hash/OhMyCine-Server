package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"gorm.io/gorm"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloader "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

// This fake keeps disjoint route namespaces and rejects every management call
// that accidentally receives a catalog/TV ID.
type quarkTransferFake struct {
	*fakeMutationCloudDriver
	contents               map[string][]byte
	readCalls, uploadCalls int
	corruptRead            bool
	denyProof              bool
	requireCreationContext bool
	creationContext        context.Context
}

func (f *quarkTransferFake) Provider() string { return cloud.ProviderQuark }
func (f *quarkTransferFake) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{DirectoryList: true, TemporaryDirectURL: true, SignedProxy: true, CreateDirectory: true, Rename: true, Move: true, FileUpload: true}
}
func (f *quarkTransferFake) ResolveManagementItem(ctx context.Context, id string) (cloud.Item, error) {
	if f.denyProof {
		return cloud.Item{}, errors.New("proof no longer valid")
	}
	item, err := f.Stat(ctx, id)
	item.ID = "agent:" + item.ID
	item.ParentID = "agent:" + item.ParentID
	return item, err
}
func (f *quarkTransferFake) ResolveReadItem(ctx context.Context, id string) (cloud.Item, error) {
	if f.requireCreationContext && (f.creationContext == nil || f.creationContext != ctx) {
		return cloud.Item{}, errors.New("original operation context required")
	}
	if !strings.HasPrefix(id, "agent:") || f.denyProof {
		return cloud.Item{}, errors.New("management identity required")
	}
	return f.Stat(ctx, strings.TrimPrefix(id, "agent:"))
}
func agentTestID(id string) (string, error) {
	if !strings.HasPrefix(id, "agent:") {
		return "", errors.New("TV ID reached Agent")
	}
	return strings.TrimPrefix(id, "agent:"), nil
}
func (f *quarkTransferFake) CreateDirectory(ctx context.Context, parent, name string) (cloud.Item, error) {
	f.creationContext = ctx
	p, err := agentTestID(parent)
	if err != nil {
		return cloud.Item{}, err
	}
	i, err := f.fakeMutationCloudDriver.CreateDirectory(ctx, p, name)
	i.ID = "agent:" + i.ID
	i.ParentID = "agent:" + i.ParentID
	return i, err
}
func (f *quarkTransferFake) Move(ctx context.Context, id, parent string) error {
	i, err := agentTestID(id)
	if err != nil {
		return err
	}
	p, err := agentTestID(parent)
	if err != nil {
		return err
	}
	return f.fakeMutationCloudDriver.Move(ctx, i, p)
}
func (f *quarkTransferFake) Rename(ctx context.Context, id, name string) error {
	i, err := agentTestID(id)
	if err != nil {
		return err
	}
	return f.fakeMutationCloudDriver.Rename(ctx, i, name)
}
func (f *quarkTransferFake) Upload(ctx context.Context, r cloud.UploadRequest) (cloud.Item, error) {
	f.creationContext = ctx
	p, err := agentTestID(r.ParentID)
	if err != nil {
		return cloud.Item{}, err
	}
	b, err := io.ReadAll(r.Reader)
	if err != nil {
		return cloud.Item{}, err
	}
	// Simulate a durable already-confirmed provider receipt on retries.
	for _, i := range f.items {
		if i.ParentID == p && i.Name == r.Name {
			i.ID = "agent:" + i.ID
			i.ParentID = "agent:" + p
			return i, nil
		}
	}
	f.uploadCalls++
	id := "uploaded-" + strconv.Itoa(f.uploadCalls)
	i := cloud.Item{ID: id, ParentID: p, Name: r.Name, Size: int64(len(b))}
	f.items[id] = i
	f.contents[id] = b
	i.ID = "agent:" + id
	i.ParentID = "agent:" + p
	return i, nil
}
func (f *quarkTransferFake) OpenRead(_ context.Context, r cloud.ReadRequest) (cloud.ReadResult, error) {
	f.readCalls++
	b := append([]byte(nil), f.contents[r.FileID]...)
	if f.corruptRead && len(b) > 0 {
		b[0] ^= 0xff
	}
	size := int64(len(b))
	return cloud.ReadResult{Body: io.NopCloser(bytes.NewReader(b)), TotalSize: &size}, nil
}

func makeQuarkTransferFake(base *fakeMutationCloudDriver) *quarkTransferFake {
	return &quarkTransferFake{fakeMutationCloudDriver: base, contents: map[string][]byte{}}
}

func TestQuarkTransferCreationMappingRetainsOriginalOperationContext(t *testing.T) {
	f := makeQuarkTransferFake(newFakeMutationCloudDriver())
	f.items["root"] = cloud.Item{ID: "root", Name: "library", IsDir: true}
	f.requireCreationContext = true
	d, err := transferCloudDriver(f, "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.(cloud.MutationDriver).CreateDirectory(context.Background(), "root", "created"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.(cloud.UploadDriver).Upload(context.Background(), cloud.UploadRequest{ParentID: "root", Name: "payload.bin", Size: 3, Reader: bytes.NewReader([]byte("abc"))}); err != nil {
		t.Fatal(err)
	}
	if f.readCalls != 1 || f.uploadCalls != 1 {
		t.Fatal("creation mapping skipped content verification")
	}
}

func setQuarkTransferRoute(t *testing.T, f cloudTransferFixture) cloudTransferFixture {
	t.Helper()
	if err := f.queue.db.Model(&models.Connection{}).Where("id = ?", f.connection).Update("provider", models.StorageTypeQuark).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Model(&models.Storage{}).Where("connection_id = ?", f.connection).Update("type", models.StorageTypeQuark).Error; err != nil {
		t.Fatal(err)
	}
	f.download.ProviderType = models.DownloaderTypeQuarkShare
	f.download.TargetStorageType = models.StorageTypeQuark
	f.download.TransferRouteVersion = models.TransferRouteVersionCurrent
	f.download.TransferRouteKind = models.TransferRouteSameSourceProvider
	f.download.SourceDataSourceJSON, _ = marshalDataSourceIdentity(models.DataSourceIdentity{Kind: models.DataSourceKindProvider, ProviderType: models.StorageTypeQuark, ConnectionIdentity: strconv.Itoa(int(f.connection)), StorageScope: strconv.Itoa(int(*f.download.StagingStorageID))})
	f.download.TargetDataSourceJSON, _ = marshalDataSourceIdentity(models.DataSourceIdentity{Kind: models.DataSourceKindProvider, ProviderType: models.StorageTypeQuark, ConnectionIdentity: strconv.Itoa(int(f.connection)), StorageScope: strconv.Itoa(int(*f.download.TargetStorageID))})
	if err := f.queue.db.Save(&f.download).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

type quarkTransferPublisher struct {
	calls     int
	dbFixture cloudTransferFixture
}

func (p *quarkTransferPublisher) ReconcileTransferBatch(_ context.Context, id string) error {
	p.calls++
	var task models.TransferTask
	if err := p.dbFixture.queue.db.First(&task, "id = ?", id).Error; err != nil {
		return err
	}
	if task.Phase != models.TransferTaskStatusCompleted {
		return errors.New("publication before output confirmation")
	}
	state, err := decodeCloudTransferState(task.CloudStateJSON)
	if err != nil {
		return err
	}
	for _, i := range state.Items {
		if strings.HasPrefix(i.CurrentID, "agent:") {
			return errors.New("Agent ID leaked into catalog")
		}
	}
	return nil
}

func TestQuarkTransferExistingPipelineMovesCanonicalIdentityAndRetainsPackage(t *testing.T) {
	f := setQuarkTransferRoute(t, newCloudTransferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictAsk, false))
	provider := makeQuarkTransferFake(f.driver)
	f.service.connections.drivers[f.connection] = provider
	publisher := &quarkTransferPublisher{dbFixture: f}
	f.service.SetMediaLibraryReconciler(publisher)
	result := f.run(t)
	if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
		t.Fatalf("result=%+v", result)
	}
	i := provider.items[f.sourceID]
	if i.ParentID == "source-root" || i.Name != "Movie (2024).mkv" || publisher.calls != 1 {
		t.Fatalf("item=%+v publish=%d", i, publisher.calls)
	}
	if _, ok := provider.items["source-root"]; !ok || len(provider.recycled) != 0 {
		t.Fatal("unverified Quark recycle was attempted")
	}
	var task models.TransferTask
	if err := f.queue.db.First(&task, "download_task_id = ?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.CleanupStatus != models.TransferCleanupSkipped || task.CleanupErrorCode != "quark_recycle_unavailable" {
		t.Fatalf("cleanup=%s %s", task.CleanupStatus, task.CleanupErrorCode)
	}
}

func TestQuarkTransferDoesNotOfferOverwriteAndRejectsCopyBeforeWriting(t *testing.T) {
	for _, mode := range []string{models.MediaLibraryTransferMove, models.MediaLibraryTransferCopy} {
		t.Run(mode, func(t *testing.T) {
			f := setQuarkTransferRoute(t, newCloudTransferFixture(t, mode, models.MediaLibraryConflictAsk, true))
			f.service.connections.drivers[f.connection] = makeQuarkTransferFake(f.driver)
			r := f.run(t)
			if mode == models.MediaLibraryTransferCopy {
				if r.ErrorCode != "cloud_transfer_capability_missing" {
					t.Fatalf("%+v", r)
				}
			} else {
				if r.Wait == nil || strings.Contains(strings.Join(r.Wait.Options, ","), "overwrite") {
					t.Fatalf("%+v", r)
				}
			}
			if f.driver.moveCalls != 0 || f.driver.copyCalls != 0 || len(f.driver.recycled) != 0 {
				t.Fatal("unsupported mutation submitted")
			}
		})
	}
}

func TestQuarkUploadReadbackRejectsSameSizeCorruptionAndRetryDoesNotUploadAgain(t *testing.T) {
	f := makeQuarkTransferFake(newFakeMutationCloudDriver())
	f.items["root"] = cloud.Item{ID: "root", IsDir: true}
	driver, err := transferCloudDriver(f, "task")
	if err != nil {
		t.Fatal(err)
	}
	uploader := driver.(cloud.UploadDriver)
	request := cloud.UploadRequest{ParentID: "root", Name: "video.mkv", Size: 7, Reader: bytes.NewReader([]byte("payload"))}
	f.corruptRead = true
	if _, err := uploader.Upload(context.Background(), request); err == nil {
		t.Fatal("same-size corruption accepted")
	}
	f.corruptRead = false
	output, err := uploader.Upload(context.Background(), request)
	if err != nil || output.ID == "" || strings.HasPrefix(output.ID, "agent:") || f.uploadCalls != 1 || f.readCalls != 2 {
		t.Fatalf("output=%+v uploads=%d reads=%d err=%v", output, f.uploadCalls, f.readCalls, err)
	}
	f.denyProof = true
	if _, err := uploader.Upload(context.Background(), request); err == nil {
		t.Fatal("revoked common-root proof accepted")
	}
}

func TestQuarkLocalUploadRunsFrozenRouteAndReadback(t *testing.T) {
	f := newUploadTransferFixture(t, models.MediaLibraryConflictAsk, false)
	connection := *f.download.TargetConnectionID
	if err := f.queue.db.Model(&models.Connection{}).Where("id = ?", connection).Update("provider", models.StorageTypeQuark).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Model(&models.Storage{}).Where("id = ?", *f.download.TargetStorageID).Update("type", models.StorageTypeQuark).Error; err != nil {
		t.Fatal(err)
	}
	f.download.TargetStorageType = models.StorageTypeQuark
	f.download.TransferRouteVersion = models.TransferRouteVersionCurrent
	f.download.TransferRouteKind = models.TransferRouteCrossSource
	f.download.SourceDataSourceJSON, _ = marshalDataSourceIdentity(localDataSourceIdentity())
	f.download.TargetDataSourceJSON, _ = marshalDataSourceIdentity(models.DataSourceIdentity{Kind: models.DataSourceKindProvider, ProviderType: models.StorageTypeQuark, ConnectionIdentity: strconv.Itoa(int(connection)), StorageScope: strconv.Itoa(int(*f.download.TargetStorageID))})
	// The full publication consumer requires the frozen recognition result
	// normally persisted by DownloadWorker before enqueueing the transfer.
	tmdbID := int64(550)
	f.download.ScrapeTMDBID = &tmdbID
	f.download.IdentitySource, f.download.IdentityStatus, f.download.IdentityRevision = mediaIdentitySourceAutomatic, mediaIdentityStatusVerified, 1
	identity, err := json.Marshal(MediaIdentitySnapshot{Version: 1, Revision: 1, Source: f.download.IdentitySource, Status: f.download.IdentityStatus, TMDBID: &tmdbID, MediaType: f.download.ScrapeMediaType, Title: f.download.ScrapeTitle, Year: f.download.ScrapeYear, Category: f.download.ScrapeCategory, Confidence: f.download.ScrapeConfidence})
	if err != nil {
		t.Fatal(err)
	}
	f.download.IdentitySnapshotJSON = string(identity)
	if err := f.queue.db.Save(&f.download).Error; err != nil {
		t.Fatal(err)
	}
	provider := makeQuarkTransferFake(f.driver.fakeMutationCloudDriver)
	f.service.connections.drivers[connection] = provider
	libraries := setupQuarkUploadCatalog(t, f.service, &f.library)
	r := f.enqueueAndRun(t)
	if r.ErrorCode != "" || r.RetryAt != nil || r.Wait != nil || provider.uploadCalls != 1 || provider.readCalls != 1 {
		var failedTask models.TransferTask
		f.queue.db.First(&failedTask, "download_task_id = ?", f.download.ID)
		t.Fatalf("result=%+v uploads=%d reads=%d publish=%v", r, provider.uploadCalls, provider.readCalls, libraries.ReconcileTransferBatch(context.Background(), failedTask.ID))
	}
	var task models.TransferTask
	f.queue.db.First(&task, "download_task_id = ?", f.download.ID)
	var state cloudTransferState
	if err := json.Unmarshal([]byte(task.CloudStateJSON), &state); err != nil {
		t.Fatal(err)
	}
	for _, i := range state.Items {
		if strings.HasPrefix(i.CurrentID, "agent:") {
			t.Fatal("management identity published")
		}
	}
	if task.CatalogPublishedAt == nil {
		t.Fatal("upload was not immediately published")
	}
	entries := catalogReadEntries(t, libraries.catalogStore, f.library.ID, "")
	if len(entries) != 1 || entries[0].ProviderID != "uploaded-1" || entries[0].Size != f.manifest.Files[0].Size {
		t.Fatalf("uploaded catalog=%+v", entries)
	}
	if err := libraries.RecoverCatalogScanFollowups(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	job, err := f.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || job == nil {
		t.Fatalf("upload artifact=%+v error=%v", job, err)
	}
	artifactResult := NewMediaArtifactWorker(libraries.artifacts).Run(context.Background(), workerRuntime{queue: f.queue, job: *job}, *job)
	if artifactResult.ErrorCode != "" || artifactResult.RetryAt != nil || artifactResult.Wait != nil {
		t.Fatalf("upload artifact result=%+v", artifactResult)
	}
	if err := f.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
	strmPath := filepath.Join(f.library.STRMLocalRoot, "电影", "Movie (2024).strm")
	body, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "https://media.example.test/") || !strings.Contains(string(body), "sig=") || strings.Contains(string(body), "uploaded-1") {
		t.Fatal("upload STRM is not an opaque signed Server URL")
	}
	assertQuarkTransferObservationNoop(t, libraries, provider, f.library.ID, strmPath)
	if provider.uploadCalls != 1 || provider.readCalls != 1 {
		t.Fatal("publication/observation repeated upload or full readback")
	}
}

func setupQuarkUploadCatalog(t *testing.T, transfers *TransferService, library *models.MediaLibrary) *MediaLibraryService {
	t.Helper()
	library.STRMEnabled, library.SignedProxyEnabled = true, true
	library.STRMLocalRoot = t.TempDir()
	if err := transfers.db.Save(library).Error; err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := transfers.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := transfers.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	storage.Capabilities = `{"temporary_direct_url":true,"signed_proxy":true}`
	if err := transfers.db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := transfers.db.Transaction(func(tx *gorm.DB) error { return initializeFreshQuarkCatalogTx(tx, *library, storage, profile) }); err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err := transfers.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	readDB, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := readDB.DB()
	t.Cleanup(func() { _ = pool.Close() })
	catalog := NewCatalogSnapshotStore(transfers.db, readDB)
	s := NewMediaLibraryService(transfers.db, transfers.audit, zerolog.Nop())
	s.connections, s.queue = transfers.connections, transfers.queue
	s.SetCatalogSnapshotStore(catalog)
	s.changes = NewMediaChangeService(transfers.db)
	installHydrationMetadata(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":550,"title":"Movie","name":"Movie"}`))
	})
	credentials, err := credential.Open(filepath.Join(t.TempDir(), "test.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewSignedProxyService(transfers.db, credentials, transfers.connections, "https://media.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	proxy.SetCatalogSnapshotStore(catalog)
	artifacts := NewMediaArtifactService(transfers.db, transfers.queue, proxy, zerolog.Nop())
	artifacts.SetCatalogSnapshotStore(catalog)
	artifacts.SetConnectionService(transfers.connections)
	artifacts.SetMediaChangeService(s.changes)
	s.SetArtifactService(artifacts)
	s.EnableCatalogScanFollowups()
	transfers.SetMediaLibraryReconciler(s)
	return s
}

func TestQuarkMaterializationRestartsUnverifiedPartialAndKeepsDigestReceipt(t *testing.T) {
	f := makeQuarkTransferFake(newFakeMutationCloudDriver())
	f.contents["file"] = []byte("payload")
	root := t.TempDir()
	destination := filepath.Join(root, "video.mkv")
	if err := os.WriteFile(destination+".partial", []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := materializeQuarkFile(context.Background(), nil, f, downloader.File{ProviderItemID: "file", Size: 7}, root, destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyMaterializedSHA256(destination, 7, digest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("payloae"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyMaterializedSHA256(destination, 7, digest); err == nil {
		t.Fatal("same-size staging corruption accepted")
	}
}

func TestQuarkCrossSourceResumeUsesPersistedSHA256AndPublishesLocalTarget(t *testing.T) {
	f := setQuarkTransferRoute(t, newCloudTransferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictAsk, false))
	provider := makeQuarkTransferFake(f.driver)
	provider.contents[f.sourceID] = bytes.Repeat([]byte{'v'}, int(minimumAutomaticTransferVideoBytes))
	item := provider.items[f.sourceID]
	item.SHA1 = ""
	provider.items[f.sourceID] = item
	f.manifest.Files[0].SHA1 = ""
	f.service.connections.drivers[f.connection] = provider
	targetRoot := t.TempDir()
	staging := t.TempDir()
	if err := f.queue.db.Model(&models.Storage{}).Where("id = ?", *f.download.TargetStorageID).Updates(map[string]any{"type": models.StorageTypeLocal, "root_path": targetRoot, "root_path_normalized": targetRoot, "connection_id": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Model(&models.MediaLibrary{}).Where("id = ?", f.library.ID).Updates(map[string]any{"relative_root": "/", "provider_root_id": ""}).Error; err != nil {
		t.Fatal(err)
	}
	f.download.TargetStorageType = models.StorageTypeLocal
	f.download.TargetStorageRoot = targetRoot
	f.download.TargetRelativeRoot = "/"
	f.download.TargetConnectionID = nil
	f.download.TargetProviderRootID = ""
	f.download.StagingAbsolutePath = staging
	f.download.TransferRouteKind = models.TransferRouteCrossSource
	f.download.TargetDataSourceJSON, _ = marshalDataSourceIdentity(localDataSourceIdentity())
	if err := f.queue.db.Save(&f.download).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.Enqueue(f.download, f.manifest); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.queue.Claim([]string{"transfer"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	var task models.TransferTask
	if err := f.queue.db.First(&task, "download_task_id = ?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	runtime := workerRuntime{queue: f.queue, job: *claimed}
	worker := NewTransferWorker(f.service)
	_, managed, err := worker.materializePan115Source(context.Background(), runtime, task, f.download, f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if provider.readCalls != 1 {
		t.Fatalf("reads=%d", provider.readCalls)
	}
	// A fresh worker reloads the confirmed checkpoint, checks its staged hash,
	// then uses the ordinary local transfer and catalog publication pipeline.
	publisher := &quarkTransferPublisher{dbFixture: f}
	f.service.SetMediaLibraryReconciler(publisher)
	r := NewTransferWorker(f.service).Run(context.Background(), runtime, *claimed)
	if r.ErrorCode != "" || r.RetryAt != nil || r.Wait != nil {
		t.Fatalf("result=%+v", r)
	}
	if provider.readCalls != 1 || publisher.calls != 1 {
		t.Fatalf("reads=%d publish=%d", provider.readCalls, publisher.calls)
	}
	destination := filepath.Join(targetRoot, "电影", "Movie (2024)", "Movie (2024).mkv")
	got, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(got, provider.contents[f.sourceID]) {
		t.Fatalf("content changed: %v", err)
	}
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("owned staging remains: %v", err)
	}
	if _, exists := provider.items[f.sourceID]; !exists {
		t.Fatal("Quark cloud source deleted without capability")
	}
	if err := f.queue.db.First(&task, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	state, err := decodeCloudTransferState(task.CloudStateJSON)
	if err != nil {
		t.Fatal(err)
	}
	saved := state.Materialized[normalizedManifestPath(f.manifest.Files[0].RelativePath)]
	if saved.DigestVersion != 2 || saved.DigestAlgorithm != "sha256" || len(saved.DigestValue) != 64 {
		t.Fatalf("digest receipt=%+v", saved)
	}
}

func TestQuarkTransferPublishesVersionedCatalogAndRealSignedSTRM(t *testing.T) {
	f := setQuarkTransferRoute(t, newCloudTransferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictAsk, false))
	provider := makeQuarkTransferFake(f.driver)
	f.service.connections.drivers[f.connection] = provider
	projection := t.TempDir()
	f.library.STRMEnabled = true
	f.library.SignedProxyEnabled = true
	f.library.STRMLocalRoot = projection
	if err := f.queue.db.Save(&f.library).Error; err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := f.queue.db.First(&storage, f.library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.First(&profile, f.library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	storage.Capabilities = `{"temporary_direct_url":true,"signed_proxy":true}`
	if err := f.queue.db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Transaction(func(tx *gorm.DB) error { return initializeFreshQuarkCatalogTx(tx, f.library, storage, profile) }); err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err := f.queue.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	readDB, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := readDB.DB()
	t.Cleanup(func() { _ = pool.Close() })
	catalog := NewCatalogSnapshotStore(f.queue.db, readDB)
	s := NewMediaLibraryService(f.queue.db, f.queue.audit, zerolog.Nop())
	s.connections = f.service.connections
	s.queue = f.queue
	s.SetCatalogSnapshotStore(catalog)
	s.changes = NewMediaChangeService(f.queue.db)
	installHydrationMetadata(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%d,"title":"Movie","name":"Movie"}`, *f.download.ScrapeTMDBID)
	})
	credentials, err := credential.Open(filepath.Join(t.TempDir(), "test.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewSignedProxyService(f.queue.db, credentials, f.service.connections, "https://media.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	proxy.SetCatalogSnapshotStore(catalog)
	artifacts := NewMediaArtifactService(f.queue.db, f.queue, proxy, zerolog.Nop())
	artifacts.SetCatalogSnapshotStore(catalog)
	artifacts.SetConnectionService(f.service.connections)
	artifacts.SetMediaChangeService(s.changes)
	s.SetArtifactService(artifacts)
	s.EnableCatalogScanFollowups()
	f.service.SetMediaLibraryReconciler(s)
	result := f.run(t)
	if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
		var task models.TransferTask
		f.queue.db.First(&task, "download_task_id = ?", f.download.ID)
		t.Fatalf("transfer=%+v publish=%v", result, s.ReconcileTransferBatch(context.Background(), task.ID))
	}
	entries := catalogReadEntries(t, catalog, f.library.ID, "")
	if len(entries) != 1 || entries[0].ProviderID != f.sourceID || entries[0].TMDBID == nil || *entries[0].TMDBID != *f.download.ScrapeTMDBID {
		t.Fatalf("catalog entries=%+v", entries)
	}
	if err := s.RecoverCatalogScanFollowups(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	claim, err := f.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claim == nil {
		t.Fatalf("artifact claim=%+v err=%v", claim, err)
	}
	result = NewMediaArtifactWorker(artifacts).Run(context.Background(), workerRuntime{queue: f.queue, job: *claim}, *claim)
	if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
		t.Fatalf("artifact=%+v", result)
	}
	body, err := os.ReadFile(filepath.Join(projection, "电影", "Movie (2024)", "Movie (2024).strm"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), "https://media.example.test/") || strings.Contains(string(body), f.sourceID) || strings.Contains(string(body), "example.invalid") {
		t.Fatalf("not an opaque Server playback URL: %s", body)
	}
	var task models.TransferTask
	if err := f.queue.db.First(&task, "download_task_id = ?", f.download.ID).Error; err != nil || task.CatalogPublishedAt == nil {
		t.Fatalf("publication not durable: %v", err)
	}
	if err := f.queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	assertQuarkTransferObservationNoop(t, s, provider, f.library.ID, filepath.Join(projection, "电影", "Movie (2024)", "Movie (2024).strm"))
}

// A task's publication and a later independent provider observation must not
// create a second import, rewrite its STRM, or announce the same media again.
func assertQuarkTransferObservationNoop(t *testing.T, s *MediaLibraryService, provider cloud.Driver, libraryID uint, strmPath string) {
	t.Helper()
	ctx := context.Background()
	ready := 0
	s.changes.SetReadyHandler(func(uint, uint64) { ready++ })
	dispatch := func() {
		t.Helper()
		for i := 0; i < 20; i++ {
			more, err := s.changes.DispatchBatch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !more {
				return
			}
		}
		t.Fatal("ready dispatch did not finish")
	}
	dispatch()
	beforeReady := ready
	var beforeHead models.CatalogHead
	var beforeLibrary models.MediaLibrary
	if err := s.db.First(&beforeHead, "library_id = ?", libraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&beforeLibrary, libraryID).Error; err != nil {
		t.Fatal(err)
	}
	// Both callers begin with the empty revision-1 catalog. The transfer must
	// have published precisely one import before observation starts.
	var transferPublications int64
	if err := s.db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ? AND kind = ? AND status = ?", libraryID, "transfer_batch", "success").Count(&transferPublications).Error; err != nil {
		t.Fatal(err)
	}
	if beforeHead.Revision != 2 || transferPublications != 1 {
		t.Fatalf("transfer publication count: head=%d successful batches=%d", beforeHead.Revision, transferPublications)
	}
	beforeEntries := catalogReadEntries(t, s.catalogStore, libraryID, "")
	beforeBody, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeStat, err := os.Stat(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	observer := NewProviderObservationService(s.db)
	clock := time.Now().UTC()
	observer.now = func() time.Time { return clock }
	s.SetProviderObservationService(observer) // Real scope/fence and reader wiring.
	scope, err := s.ensureQuarkObservation(ctx, libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if err = observer.RequestFullSync(ctx, scope.ID); err != nil {
		t.Fatal(err)
	}
	complete := false
	for i := 0; i < 40; i++ {
		clock = clock.Add(3 * time.Second) // Offline request spacing only.
		if _, err = observer.ObserveNext(ctx, scope.ID, quarkObservationReader{driver: provider}); err != nil {
			t.Fatal(err)
		}
		if _, err = observer.PublishPending(ctx, scope.ID, 100); err != nil {
			t.Fatal(err)
		}
		status, e := observer.Status(ctx, scope.ID)
		if e != nil {
			t.Fatal(e)
		}
		if status.LastCompleteAt != nil && status.PendingDirectories == 0 && status.PendingPublications == 0 && status.PathRepairs == 0 && status.PendingConfirmations == 0 {
			complete = true
			break
		}
	}
	if !complete {
		t.Fatal("post-transfer observation did not finish")
	}
	var observedPages int64
	if err := s.db.Model(&models.ProviderObservationBatch{}).Where("scope_id = ? AND completed_at IS NOT NULL", scope.ID).Select("COALESCE(SUM(pages), 0)").Scan(&observedPages).Error; err != nil {
		t.Fatal(err)
	}
	if observedPages == 0 {
		t.Fatal("post-transfer observation completed without reading provider directory pages")
	}
	if err := s.RecoverCatalogScanFollowups(ctx, 20); err != nil {
		t.Fatal(err)
	}
	job, err := s.queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || job != nil {
		t.Fatalf("duplicate artifact job=%+v error=%v", job, err)
	}
	dispatch()
	var afterHead models.CatalogHead
	var afterLibrary models.MediaLibrary
	if err := s.db.First(&afterHead, "library_id = ?", libraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&afterLibrary, libraryID).Error; err != nil {
		t.Fatal(err)
	}
	afterEntries := catalogReadEntries(t, s.catalogStore, libraryID, "")
	afterBody, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	afterStat, err := os.Stat(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterHead.Revision != beforeHead.Revision || afterLibrary.ContentRevision != beforeLibrary.ContentRevision || afterLibrary.DirtyGeneration != beforeLibrary.DirtyGeneration || ready != beforeReady || !bytes.Equal(beforeBody, afterBody) || !afterStat.ModTime().Equal(beforeStat.ModTime()) {
		t.Fatalf("post-transfer observation changed publication: head %d/%d content %d/%d dirty %d/%d ready %d/%d", beforeHead.Revision, afterHead.Revision, beforeLibrary.ContentRevision, afterLibrary.ContentRevision, beforeLibrary.DirtyGeneration, afterLibrary.DirtyGeneration, beforeReady, ready)
	}
	if len(beforeEntries) != 1 || len(afterEntries) != 1 || afterEntries[0].ID != beforeEntries[0].ID || !afterEntries[0].CreatedAt.Equal(beforeEntries[0].CreatedAt) {
		t.Fatal("post-transfer observation changed stable identity")
	}
}
