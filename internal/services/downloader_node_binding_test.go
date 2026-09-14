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
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader/qbittorrent"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func remoteDownloaderFixture(t *testing.T) (*TransferNodeService, Actor, *DownloaderService, TransferNodeSummary, DownloaderSummary) {
	t.Helper()
	nodes, actor := transferNodeFixture(t)
	actor.Permissions[authz.PermissionDownloadersCreate] = struct{}{}
	actor.Permissions[authz.PermissionDownloadersRead] = struct{}{}
	actor.Permissions[authz.PermissionDownloadersUpdate] = struct{}{}
	node, _, err := nodes.Create(context.Background(), actor, CreateTransferNodeInput{Name: "Edge", APIURL: "https://edge.example.com:4433", Platform: "linux", Architecture: "amd64"}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, _ := json.Marshal(nodeprotocol.Capabilities{Codes: []string{nodeprotocol.CapabilityQBittorrentControl}})
	if err := nodes.db.Model(&models.TransferNode{}).Where("id = ?", node.ID).Updates(map[string]any{"status": models.NodeStatusOnline, "public_key_fingerprint": strings.Repeat("a", 64), "encryption_public_key": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey().Bytes()), "capabilities_json": string(capabilities)}).Error; err != nil {
		t.Fatal(err)
	}
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, qbittorrent.Capabilities, func(config downloadpkg.Config) (downloadpkg.Client, error) { return qbittorrent.New(config) }); err != nil {
		t.Fatal(err)
	}
	service := NewDownloaderService(nodes.db, nodes.audit, nodes.credentials, registry)
	service.SetTransferNodeService(nodes)
	created, err := service.Create(actor, DownloaderInput{Name: "Remote qB", Type: models.DownloaderTypeQBittorrent, ExecutionLocation: models.NodeLocationRemote, NodeID: node.ID, BaseURL: "http://127.0.0.1:8080", Username: "admin", Password: "secret", DownloaderSaveRoot: "/downloads", NodeMountRoot: "/var/lib/ohmycine-node/managed/downloads", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	return nodes, actor, service, node, created
}

func TestRemoteDownloaderStoresConnectionOnlyInNodeBinding(t *testing.T) {
	nodes, _, _, node, created := remoteDownloaderFixture(t)
	if created.ExecutionLocation != models.NodeLocationRemote || created.NodeID == nil || *created.NodeID != node.ID || created.BaseURL == "" || !created.PasswordConfigured {
		t.Fatalf("summary=%+v", created)
	}
	var record models.Downloader
	if err := nodes.db.First(&record, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if record.BaseURL != "" || record.UsernameCiphertext != "" || record.PasswordCiphertext != "" {
		t.Fatal("remote connection leaked into legacy local downloader fields")
	}
	var binding models.NodeDownloaderBinding
	if err := nodes.db.First(&binding, "downloader_id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if binding.BaseURL != "http://127.0.0.1:8080" || binding.PasswordCiphertext == "" || binding.PasswordCiphertext == "secret" {
		t.Fatalf("binding was not stored safely: %+v", binding)
	}
}

func TestRemoteDownloaderRouteCannotDriftAfterTaskCreation(t *testing.T) {
	nodes, actor, service, node, created := remoteDownloaderFixture(t)
	now := time.Now().UTC()
	jobID := uuid.NewString()
	ownerID := actor.User.ID
	if err := nodes.db.Create(&models.Job{ID: jobID, OwnerID: &ownerID, JobType: "download", Status: models.JobStatusCompleted, DisplayName: "existing remote download", PayloadJSON: "{}", CheckpointJSON: "{}", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := nodes.db.Create(&models.DownloadTask{ID: uuid.NewString(), OwnerID: ownerID, JobID: jobID, DownloaderID: &created.ID, ExecutionLocation: models.NodeLocationRemote, NodeID: &node.ID, NodeName: node.Name, ProtocolVersion: nodeprotocol.VersionV1, RoutePlanRevision: 1, RoutePlanDigest: strings.Repeat("b", 64), DownloaderName: created.Name, ProviderType: models.DownloaderTypeQBittorrent, SourceCiphertext: "sealed", DisplayName: "existing remote download", Phase: models.DownloadTaskStatusCompleted, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}

	changedRoot := "/other-downloads"
	if _, err := service.Update(actor, created.ID, UpdateDownloaderInput{DownloaderSaveRoot: &changedRoot}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed downloader root error=%v", err)
	}
	changedNode := uuid.NewString()
	if _, err := service.Update(actor, created.ID, UpdateDownloaderInput{NodeID: &changedNode}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed node error=%v", err)
	}

	var binding models.NodeDownloaderBinding
	if err := nodes.db.First(&binding, "downloader_id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if binding.NodeID != node.ID || binding.DownloaderSaveRoot != "/downloads" || binding.NodeMountRoot != "/var/lib/ohmycine-node/managed/downloads" {
		t.Fatalf("frozen route changed: %+v", binding)
	}
}

func TestClientForTaskRejectsFrozenNodeMismatch(t *testing.T) {
	nodes, actor, service, node, created := remoteDownloaderFixture(t)
	var record models.Downloader
	if err := nodes.db.First(&record, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	otherNodeID := uuid.NewString()
	localRoot := t.TempDir()
	task := models.DownloadTask{ID: uuid.NewString(), ExecutionLocation: models.NodeLocationRemote, NodeID: &otherNodeID}
	if _, err := service.clientForTask(context.Background(), record, task, localRoot); ErrorCode(err) != CodeDownloaderUnavailable {
		t.Fatalf("task node drift error=%v node=%s", err, node.ID)
	}
	task.NodeID = &node.ID
	if _, err := service.clientForTask(context.Background(), record, task, localRoot); ErrorCode(err) != CodeDownloaderUnavailable {
		t.Fatalf("missing frozen route plan error=%v", err)
	}
	_, revision, digest, err := nodeTaskRoutePlan(nodes.db, record, localRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	task.ProtocolVersion, task.RoutePlanRevision, task.RoutePlanDigest = nodeprotocol.VersionV1, revision, digest
	if _, err := service.clientForTask(context.Background(), record, task, localRoot); err != nil {
		t.Fatalf("valid frozen route rejected: %v", err)
	}
	newPassword := "rotated-secret"
	if _, err := service.Update(actor, created.ID, UpdateDownloaderInput{Password: &newPassword}, RequestContext{}); err != nil {
		t.Fatalf("credential rotation should not move a frozen route: %v", err)
	}
	if _, err := service.clientForTask(context.Background(), record, task, localRoot); err != nil {
		t.Fatalf("credential rotation invalidated frozen route: %v", err)
	}
	if err := nodes.db.Model(&models.NodeDownloaderBinding{}).Where("downloader_id = ?", created.ID).Update("downloader_save_root", "/drifted").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.clientForTask(context.Background(), record, task, localRoot); ErrorCode(err) != CodeDownloaderUnavailable {
		t.Fatalf("changed path mapping was not rejected: %v", err)
	}
}
