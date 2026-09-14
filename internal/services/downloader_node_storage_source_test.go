package services

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader/pan115offline"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestNodeStorageSourcePlansFreezeLocalAndCrossAccountRoutes(t *testing.T) {
	sourceConnectionID := uint(10)
	source := models.Storage{ID: 1, Type: models.StorageTypePan115, ConnectionID: &sourceConnectionID}
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"

	local, err := buildNodeStorageSourcePlan(source, models.DownloadTask{TargetStorageType: models.StorageTypeLocal}, nodeprotocol.StorageSourceKindPan115OfflineMagnet, magnet, "source-output")
	if err != nil {
		t.Fatal(err)
	}
	if local.TargetKind != nodeprotocol.StorageSourceTargetLocal || local.TargetIdentityDigest != "" || local.SourceIdentityDigest == "" {
		t.Fatalf("local route=%+v", local)
	}

	targetConnectionID := uint(20)
	cross, err := buildNodeStorageSourcePlan(source, models.DownloadTask{TargetStorageType: models.StorageTypePan115, TargetConnectionID: &targetConnectionID}, nodeprotocol.StorageSourceKindPan115OfflineMagnet, magnet, "source-output")
	if err != nil {
		t.Fatal(err)
	}
	if cross.TargetKind != nodeprotocol.StorageSourceTargetPan115 || cross.TargetIdentityDigest == "" || cross.TargetIdentityDigest == cross.SourceIdentityDigest {
		t.Fatalf("cross-account route=%+v", cross)
	}

	if _, err := buildNodeStorageSourcePlan(source, models.DownloadTask{TargetStorageType: models.StorageTypePan115, TargetConnectionID: &sourceConnectionID}, nodeprotocol.StorageSourceKindPan115OfflineMagnet, magnet, "source-output"); ErrorCode(err) != CodeTransferRouteUnsupported {
		t.Fatalf("same-account route err=%v", err)
	}
	if _, _, err := nodeStorageSourceInput(downloadSourceEnvelope{Kind: downloadpkg.SourceURL, URL: "https://example.test/movie.iso"}); ErrorCode(err) != CodeTransferRouteUnsupported {
		t.Fatalf("ordinary HTTP source err=%v", err)
	}

	raw, err := json.Marshal(cross)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), magnet) || strings.Contains(string(raw), "UID=") {
		t.Fatalf("frozen plan leaked source credentials: %s", raw)
	}
	publicTask, err := json.Marshal(models.DownloadTask{SourceCiphertext: magnet + testPan115Cookie, CompletedManifestJSON: magnet})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicTask), magnet) || strings.Contains(string(publicTask), testPan115Cookie) {
		t.Fatalf("download response leaked private source material: %s", publicTask)
	}
}

func TestNodeStorageSourceUsesRecoveredProviderOutputWithoutCreation(t *testing.T) {
	record := models.Downloader{ProviderDirectoryID: "current-directory"}
	recovered, needsCreate, err := nodeStorageSourceDirectorySnapshot(record, models.DownloadTask{StagingProviderDirectoryID: "frozen-directory", ProviderOutputID: "existing-output", ProviderTag: "omc-task-1"})
	if err != nil {
		t.Fatal(err)
	}
	if needsCreate || recovered.ID != "existing-output" || recovered.ParentID != "frozen-directory" {
		t.Fatalf("recovered directory=%+v needsCreate=%v", recovered, needsCreate)
	}
	missing, needsCreate, err := nodeStorageSourceDirectorySnapshot(record, models.DownloadTask{StagingProviderDirectoryID: "frozen-directory", ProviderTag: "omc-task-1"})
	if err != nil || !needsCreate || missing.ID != "" {
		t.Fatalf("new directory=%+v needsCreate=%v err=%v", missing, needsCreate, err)
	}
}

func TestNodeStorageDownloaderRouteCannotChangeAfterAnyTask(t *testing.T) {
	driver := &fakeCloudDriver{
		nativeOffline: true, createDirectory: true,
		items: map[string]cloudpkg.Item{
			"source-root": {ID: "source-root", ParentID: "0", Name: "source", IsDir: true},
			"other-root":  {ID: "other-root", ParentID: "0", Name: "other", IsDir: true},
			"directory-a": {ID: "directory-a", ParentID: "source-root", Name: "A", IsDir: true},
			"directory-b": {ID: "directory-b", ParentID: "source-root", Name: "B", IsDir: true},
			"directory-c": {ID: "directory-c", ParentID: "other-root", Name: "C", IsDir: true},
		},
		children: map[string][]cloudpkg.Item{"source-root": {
			{ID: "directory-a", ParentID: "source-root", Name: "A", IsDir: true},
			{ID: "directory-b", ParentID: "source-root", Name: "B", IsDir: true},
		}, "other-root": {{ID: "directory-c", ParentID: "other-root", Name: "C", IsDir: true}}},
	}
	db, _, connections, actor := newConnectionTestService(t, driver)
	actor.Permissions[authz.PermissionDownloadersCreate] = struct{}{}
	actor.Permissions[authz.PermissionDownloadersRead] = struct{}{}
	actor.Permissions[authz.PermissionDownloadersUpdate] = struct{}{}
	actor.Permissions[authz.PermissionTransferNodesCreate] = struct{}{}
	actor.Permissions[authz.PermissionTransferNodesRead] = struct{}{}
	actor.Permissions[authz.PermissionTransferNodesUpdate] = struct{}{}
	actor.Permissions[authz.PermissionTransferNodesEnroll] = struct{}{}

	connection, err := connections.Create(actor, ConnectionInput{Name: "Node source", Provider: cloudpkg.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	storage := models.Storage{Name: "Node source", NameNormalized: "node-source", Type: models.StorageTypePan115, RootPath: "source-root", RootDisplayPath: "/source", RootPathNormalized: "pan115:node-source", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{"native_offline_download":true}`, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	otherStorage := models.Storage{Name: "Other source", NameNormalized: "other-source", Type: models.StorageTypePan115, RootPath: "other-root", RootDisplayPath: "/other", RootPathNormalized: "pan115:other-source", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{"native_offline_download":true}`, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&otherStorage).Error; err != nil {
		t.Fatal(err)
	}
	directories := NewProviderDirectoryService(connections, connections.credentials)
	listing, err := directories.BrowseStorage(context.Background(), actor, storage.ID, "", "")
	if err != nil || len(listing.Items) != 2 {
		t.Fatalf("directory listing=%+v err=%v", listing, err)
	}
	otherListing, err := directories.BrowseStorage(context.Background(), actor, otherStorage.ID, "", "")
	if err != nil || len(otherListing.Items) != 1 {
		t.Fatalf("other directory listing=%+v err=%v", otherListing, err)
	}

	nodes := NewTransferNodeService(db, NewAuditService(db), connections.credentials)
	node, _, err := nodes.Create(nil, actor, CreateTransferNodeInput{Name: "Storage node", APIURL: "https://node.example.test", Platform: "linux", Architecture: "amd64"}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, _ := json.Marshal(nodeprotocol.Capabilities{Codes: []string{nodeprotocol.CapabilityPan115Offline, nodeprotocol.CapabilityPan115ShareReceive, nodeprotocol.CapabilityPan115Read, nodeprotocol.CapabilityRangeExport}})
	if err := db.Model(&models.TransferNode{}).Where("id = ?", node.ID).Updates(map[string]any{"status": models.NodeStatusOnline, "public_key_fingerprint": strings.Repeat("a", 64), "encryption_public_key": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes()), "capabilities_json": string(capabilities)}).Error; err != nil {
		t.Fatal(err)
	}

	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypePan115Offline, pan115offline.Capabilities, pan115offline.New); err != nil {
		t.Fatal(err)
	}
	service := NewDownloaderService(db, NewAuditService(db), connections.credentials, registry)
	service.SetConnectionService(connections)
	service.SetTransferNodeService(nodes)
	created, err := service.CreateContext(context.Background(), actor, DownloaderInput{Name: "Remote 115", Type: models.DownloaderTypePan115Offline, ExecutionLocation: models.NodeLocationRemote, NodeID: node.ID, StorageID: &storage.ID, ProviderDirectoryToken: listing.Items[0].SelectionToken, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	ownerID, jobID := actor.User.ID, uuid.NewString()
	if err := db.Create(&models.Job{ID: jobID, OwnerID: &ownerID, JobType: "download", Status: models.JobStatusCompleted, DisplayName: "frozen source", PayloadJSON: "{}", CheckpointJSON: "{}", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DownloadTask{ID: uuid.NewString(), OwnerID: ownerID, JobID: jobID, DownloaderID: &created.ID, DownloaderName: created.Name, ProviderType: created.Type, SourceCiphertext: "sealed", DisplayName: "frozen source", Phase: models.DownloadTaskStatusCompleted, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}

	newName := "Remote 115 renamed"
	if _, err := service.UpdateContext(context.Background(), actor, created.ID, UpdateDownloaderInput{Name: &newName}, RequestContext{}); err != nil {
		t.Fatalf("safe name update: %v", err)
	}
	sameDirectoryToken := listing.Items[0].SelectionToken
	if _, err := service.UpdateContext(context.Background(), actor, created.ID, UpdateDownloaderInput{StorageID: &storage.ID, ProviderDirectoryToken: &sameDirectoryToken}, RequestContext{}); err != nil {
		t.Fatalf("same frozen source update: %v", err)
	}
	changedDirectoryToken := listing.Items[1].SelectionToken
	if _, err := service.UpdateContext(context.Background(), actor, created.ID, UpdateDownloaderInput{StorageID: &storage.ID, ProviderDirectoryToken: &changedDirectoryToken}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed source directory err=%v", err)
	}
	changedStorageToken := otherListing.Items[0].SelectionToken
	if _, err := service.UpdateContext(context.Background(), actor, created.ID, UpdateDownloaderInput{StorageID: &otherStorage.ID, ProviderDirectoryToken: &changedStorageToken}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed source Storage err=%v", err)
	}
	changedNodeID := uuid.NewString()
	if _, err := service.UpdateContext(context.Background(), actor, created.ID, UpdateDownloaderInput{NodeID: &changedNodeID}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed node err=%v", err)
	}

	var persisted models.Downloader
	if err := db.First(&persisted, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.NodeID == nil || *persisted.NodeID != node.ID || persisted.ProviderDirectoryID != "directory-a" || persisted.Name != newName {
		t.Fatalf("frozen downloader drifted: %+v", persisted)
	}
}

func TestStorageSourceGrantPersistsOnlyCredentialMetadata(t *testing.T) {
	db, _, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := connections.Create(actor, ConnectionInput{Name: "Grant source", Provider: cloudpkg.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "Grant source", NameNormalized: "grant-source", Type: models.StorageTypePan115, RootPath: "root", RootPathNormalized: "pan115:grant-source", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := models.TransferNode{ID: uuid.NewString(), OwnerID: actor.User.ID, Name: "Grant node", NameNormalized: "grant-node", APIURL: "https://node.example.test", EncryptionPublicKey: base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes()), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	nodes := NewTransferNodeService(db, NewAuditService(db), connections.credentials)
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	operationKey := "storage:source:grant-test"
	envelope, err := nodes.sealStorageSourceGrant(context.Background(), node, storage, "task-grant", operationKey, []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineSubmit, nodeprotocol.StorageSourceGrantOfflineStatus}, magnet)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Ciphertext == "" {
		t.Fatal("sealed task grant is missing")
	}
	var persisted models.NodeCredentialGrant
	if err := db.First(&persisted, "id = ?", envelope.GrantID).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testPan115Cookie) || strings.Contains(string(raw), magnet) || strings.Contains(string(raw), envelope.Ciphertext) {
		t.Fatalf("credential metadata leaked secret material: %s", raw)
	}
}
