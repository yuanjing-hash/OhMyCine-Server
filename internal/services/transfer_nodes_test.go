package services

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func transferNodeFixture(t *testing.T) (*TransferNodeService, Actor) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "transfer-nodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := credential.Open(filepath.Join(t.TempDir(), "credentials.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "node-owner", UsernameNormalized: "node-owner", DisplayName: "Node owner", PasswordHash: "x", Status: models.UserStatusActive}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	permissions := map[string]struct{}{}
	for _, code := range []string{
		authz.PermissionTransferNodesRead,
		authz.PermissionTransferNodesCreate,
		authz.PermissionTransferNodesUpdate,
		authz.PermissionTransferNodesDelete,
		authz.PermissionTransferNodesTest,
		authz.PermissionTransferNodesEnroll,
		authz.PermissionTransferNodesRevoke,
	} {
		permissions[code] = struct{}{}
	}
	return NewTransferNodeService(db, NewAuditService(db), store), Actor{User: user, Permissions: permissions}
}

func TestNormalizeStorageGrantActions(t *testing.T) {
	actions, err := normalizeStorageGrantActions([]string{
		nodeprotocol.StorageActionUpload,
		nodeprotocol.StorageActionReplace,
		nodeprotocol.StorageActionUpload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0] != nodeprotocol.StorageActionUpload || actions[1] != nodeprotocol.StorageActionReplace {
		t.Fatalf("actions=%v", actions)
	}
	if _, err := normalizeStorageGrantActions([]string{nodeprotocol.StorageActionReplace}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("missing upload action err=%v", err)
	}
	if _, err := normalizeStorageGrantActions([]string{nodeprotocol.StorageActionUpload, "delete"}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("unsupported action err=%v", err)
	}
}

func TestTransferNodeEnrollmentTokenIsOneTimeEncryptedAndRedacted(t *testing.T) {
	service, actor := transferNodeFixture(t)
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	node, token, err := service.Create(nil, actor, CreateTransferNodeInput{Name: "  公网节点 A  ", APIURL: "https://node.example.com:4433/", Platform: "linux", Architecture: "amd64"}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || node.APIURL != "https://node.example.com:4433" || node.Status != models.NodeStatusPending {
		t.Fatalf("node=%+v tokenConfigured=%v", node, token != "")
	}
	var enrollment models.NodeEnrollment
	if err := service.db.Where("node_id = ?", node.ID).First(&enrollment).Error; err != nil {
		t.Fatal(err)
	}
	if enrollment.TokenCiphertext == "" || enrollment.TokenCiphertext == token || strings.Contains(enrollment.TokenCiphertext, token) || enrollment.TokenHash == token {
		t.Fatal("enrollment token was not stored as an encrypted/hash-only secret")
	}
	raw, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) || strings.Contains(string(raw), enrollment.TokenCiphertext) || strings.Contains(string(raw), enrollment.TokenHash) {
		t.Fatalf("node summary leaked enrollment material: %s", raw)
	}

	newToken, expiresAt, err := service.RegenerateEnrollment(actor, node.ID, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if newToken == token || !expiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("regenerated token was not unique or correctly bounded: %v", expiresAt)
	}
	var activeCount int64
	if err := service.db.Model(&models.NodeEnrollment{}).Where("node_id = ? AND consumed_at IS NULL", node.ID).Count(&activeCount).Error; err != nil || activeCount != 1 {
		t.Fatalf("active enrollments=%d err=%v", activeCount, err)
	}
}

func TestTransferNodeSettingsRequireOnlineNodeAndUseRevisionCAS(t *testing.T) {
	service, actor := transferNodeFixture(t)
	node, _, err := service.Create(nil, actor, CreateTransferNodeInput{Name: "Node A", APIURL: "https://node.example.com", Platform: "windows", Architecture: "amd64"}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := service.Settings(actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateSettings(actor, &node.ID, settings.Revision, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("offline node accepted as default: %v", err)
	}
	if err := service.db.Model(&models.TransferNode{}).Where("id = ?", node.ID).Update("status", models.NodeStatusOnline).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := service.UpdateSettings(actor, &node.ID, settings.Revision, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.DefaultNodeID == nil || *updated.DefaultNodeID != node.ID || updated.Revision != settings.Revision+1 {
		t.Fatalf("settings=%+v", updated)
	}
	if _, err := service.UpdateSettings(actor, nil, settings.Revision, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale settings revision accepted: %v", err)
	}
}

func TestTransferNodeMustBeRevokedAndUnreferencedBeforeDeletion(t *testing.T) {
	service, actor := transferNodeFixture(t)
	node, _, err := service.Create(nil, actor, CreateTransferNodeInput{Name: "Node A", APIURL: "https://node.example.com", Platform: "linux", Architecture: "arm64"}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(actor, node.ID, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("pending node deleted without revocation: %v", err)
	}
	if err := service.Revoke(actor, node.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(actor, node.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := service.db.Model(&models.TransferNode{}).Where("id = ?", node.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("node count=%d err=%v", count, err)
	}
}

func TestTransferNodePermissionsAreEnforcedInService(t *testing.T) {
	service, actor := transferNodeFixture(t)
	actor.Permissions = map[string]struct{}{}
	if _, _, err := service.Create(nil, actor, CreateTransferNodeInput{Name: "Node A", APIURL: "https://node.example.com", Platform: "linux", Architecture: "amd64"}, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("create permission err=%v", err)
	}
	if _, err := service.List(actor); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("list permission err=%v", err)
	}
}
