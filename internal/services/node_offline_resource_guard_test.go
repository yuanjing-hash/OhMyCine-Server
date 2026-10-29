package services

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

func guardResourceKey(hash string) string {
	raw, _ := json.Marshal([]string{"native-bt-v1", hash})
	return providerReceiptHash(string(raw))
}
func TestNodeOfflineGuardBlocksSelectedBeforeAnyGrantAcrossConnectionAliases(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "same", true: "alias"}[alias], func(t *testing.T) {
			driver := &fakeCloudDriver{}
			db, secrets, connections, actor := newConnectionTestService(t, driver)
			left, err := connections.Create(actor, ConnectionInput{Name: "owner", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			id := left.ID
			if alias {
				right, e := connections.Create(actor, ConnectionInput{Name: "alias", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
				if e != nil {
					t.Fatal(e)
				}
				id = right.ID
			}
			hash := strings.Repeat("a", 40)
			store := NewProviderOperationStore(db, secrets, left.ID, cloud.ProviderPan115, "100").(cloud.ResourceOperationStore)
			r := cloud.OperationReceipt{ID: "selected-owner", Provider: cloud.ProviderPan115, AccountID: "100", Route: "cookie", Kind: "selective_offline_owner", State: "prepared", Revision: 1, Payload: json.RawMessage(`{"plan":"selected"}`)}
			if err = store.Claim(context.Background(), guardResourceKey(hash), r); err != nil {
				t.Fatal(err)
			}
			nodes := NewTransferNodeService(db, NewAuditService(db), secrets)
			storage := models.Storage{Name: "scope", NameNormalized: uuid.NewString(), RootPath: "root", RootPathNormalized: uuid.NewString(), Type: models.StorageTypePan115, ConnectionID: &id, Enabled: true, Capabilities: "{}"}
			if err = db.Create(&storage).Error; err != nil {
				t.Fatal(err)
			}
			key, e := ecdh.X25519().GenerateKey(rand.Reader)
			if e != nil {
				t.Fatal(e)
			}
			node := models.TransferNode{ID: uuid.NewString(), OwnerID: actor.User.ID, Name: "node", NameNormalized: uuid.NewString(), APIURL: "https://node.invalid", EncryptionPublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())}
			if err = db.Create(&node).Error; err != nil {
				t.Fatal(err)
			}
			_, err = nodes.sealStorageSourceGrant(context.Background(), node, storage, "task", "storage:source:task", []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineSubmit, nodeprotocol.StorageSourceGrantOfflineStatus}, "magnet:?xt=urn:btih:"+hash)
			var grants int64
			if countErr := db.Model(&models.NodeCredentialGrant{}).Count(&grants).Error; countErr != nil {
				t.Fatal(countErr)
			}
			if code, _ := cloud.ErrorInfo(err); code != cloud.CodeOfflineTaskExists {
				t.Fatalf("selected resource did not fail at ownership boundary: error=%v grants=%d", err, grants)
			}
			if err = db.Model(&models.NodeCredentialGrant{}).Count(&grants).Error; err != nil || grants != 0 || driver.probeCalls != 0 || driver.statCalls != 0 {
				t.Fatal("guard leaked a grant or SDK request", grants, err)
			}
		})
	}
}

func TestNodeOfflineGuardCredentialNamespaceRefreshSwitchAndInvalidIdentity(t *testing.T) {
	driver := &fakeCloudDriver{}
	db, secrets, connections, actor := newConnectionTestService(t, driver)
	summary, err := connections.Create(actor, ConnectionInput{Name: "source", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := models.TransferNode{ID: uuid.NewString(), OwnerID: actor.User.ID, Name: "node", NameNormalized: uuid.NewString(), APIURL: "https://node.invalid", EncryptionPublicKey: base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes())}
	if err = db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "scope", NameNormalized: uuid.NewString(), RootPath: "root", RootPathNormalized: uuid.NewString(), Type: models.StorageTypePan115, ConnectionID: &summary.ID, Enabled: true, Capabilities: "{}"}
	if err = db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	nodes := NewTransferNodeService(db, NewAuditService(db), secrets)
	uri := "magnet:?xt=urn:btih:" + strings.Repeat("b", 40)
	grant := func() error {
		_, err := nodes.sealStorageSourceGrant(context.Background(), node, storage, "stable-task", "stable-operation", []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineSubmit}, uri)
		return err
	}
	if err = grant(); err != nil {
		t.Fatal("empty confirmed ID with valid credential was rejected", err)
	}
	if err = grant(); err != nil {
		t.Fatal("same UID refresh changed operation", err)
	}
	refreshed, _ := secrets.Encrypt(connectionPurpose(summary.ID, cloud.ProviderPan115), "UID=100_A2; CID=rotated; SEID=rotated")
	if err = db.Model(&models.Connection{}).Where("id=?", summary.ID).Updates(map[string]any{"credential_ciphertext": refreshed, "revision": 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err = grant(); err != nil {
		t.Fatal("continuous UID refresh lost resource scope", err)
	}
	var before int64
	if err = db.Model(&models.NodeCredentialGrant{}).Count(&before).Error; err != nil || before != 3 {
		t.Fatal(before, err)
	}
	for _, cookie := range []string{"UID=200_A1; CID=next; SEID=next", "UID=bad; CID=next; SEID=next", "UID=100; UID=100; CID=next; SEID=next"} {
		cipher, e := secrets.Encrypt(connectionPurpose(summary.ID, cloud.ProviderPan115), cookie)
		if e != nil {
			t.Fatal(e)
		}
		if e = db.Model(&models.Connection{}).Where("id=?", summary.ID).Update("credential_ciphertext", cipher).Error; e != nil {
			t.Fatal(e)
		}
		if e = grant(); e == nil {
			t.Fatal("changed or ambiguous credential reused an old operation")
		}
	}
	cipher, _ := secrets.Encrypt(connectionPurpose(summary.ID, cloud.ProviderPan115), testPan115Cookie)
	if err = db.Model(&models.Connection{}).Where("id=?", summary.ID).Updates(map[string]any{"credential_ciphertext": cipher, "account_id": "999"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = grant(); err == nil {
		t.Fatal("confirmed account mismatch got a grant")
	}
	var after int64
	_ = db.Model(&models.NodeCredentialGrant{}).Count(&after).Error
	if after != before || driver.probeCalls != 0 || driver.statCalls != 0 {
		t.Fatal("failed guard made SDK calls or grants", before, after)
	}
}

type offlineFactsTestDriver struct {
	*fakeCloudDriver
	reads int
}

func (d *offlineFactsTestDriver) OfflineFacts(context.Context, string) (cloud.OfflineTask, error) {
	d.reads++
	return cloud.OfflineTask{}, cloud.Error(cloud.CodeNotFound, false, nil)
}

func TestNodeOfflineGuardCompleteRequiresDurableNodeTerminalAndUsesOriginalNamespace(t *testing.T) {
	base := &fakeCloudDriver{}
	db, secrets, connections, actor := newConnectionTestService(t, base)
	facts := &offlineFactsTestDriver{fakeCloudDriver: base}
	registry := cloud.NewRegistry()
	if err := registry.Register(cloud.ProviderPan115, func(cloud.Config) (cloud.Driver, error) { return facts, nil }); err != nil {
		t.Fatal(err)
	}
	connections.registry = registry
	summary, err := connections.Create(actor, ConnectionInput{Name: "completion", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "scope", NameNormalized: uuid.NewString(), RootPath: "root", RootPathNormalized: uuid.NewString(), Type: models.StorageTypePan115, ConnectionID: &summary.ID, Enabled: true, Capabilities: "{}"}
	if err = db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var connection models.Connection
	if err = db.First(&connection, summary.ID).Error; err != nil {
		t.Fatal(err)
	}
	guard, err := credentialOfflineGuard(db, secrets, connection, testPan115Cookie, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, operation := uuid.NewString(), "source:operation"
	hash := strings.Repeat("c", 40)
	uri := "magnet:?xt=urn:btih:" + hash
	access := nodeOfflineAccess(task, operation, uri)
	if err = guard.BeginOfflineAccess(context.Background(), access); err != nil {
		t.Fatal(err)
	}
	row := models.RemoteOperation{OperationKey: operation, TaskID: task, NodeID: "node", PlanDigest: strings.Repeat("d", 64), PlanRevision: 1, LeaseEpoch: 1, Status: nodeprotocol.OperationRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = db.Create(&models.TransferNode{ID: "node", OwnerID: actor.User.ID, Name: "node", NameNormalized: uuid.NewString(), APIURL: "https://node.invalid"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	s := NewDownloaderService(db, NewAuditService(db), secrets, nil)
	s.SetConnectionService(connections)
	if err = s.completeNodeOfflineAccess(context.Background(), storage, task, operation, uri); err == nil || facts.reads != 0 {
		t.Fatal("nonterminal Node released guard", err)
	}
	if err = db.Model(&row).Update("status", nodeprotocol.OperationCompleted).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.completeNodeOfflineAccess(context.Background(), storage, task, operation, uri); err != nil || facts.reads != 1 {
		t.Fatal("confirmed Node could not complete empty-record-ID guard", err)
	}
	store := NewProviderOperationStore(db, secrets, summary.ID, cloud.ProviderPan115, "100").(cloud.ResourceOperationStore)
	if _, err = store.ResourceClaim(context.Background(), guardResourceKey(hash)); !errors.Is(err, cloud.ErrOperationNotFound) {
		t.Fatal("terminal Node guard retained", err)
	}
	selected := cloud.OperationReceipt{ID: "successor", Provider: cloud.ProviderPan115, AccountID: "100", Route: "cookie", Kind: "selective_offline_owner", State: "prepared", Revision: 1, Payload: json.RawMessage(`{"plan":"successor"}`)}
	if err = store.Claim(context.Background(), guardResourceKey(hash), selected); err != nil {
		t.Fatal(err)
	}
	if err = s.completeNodeOfflineAccess(context.Background(), storage, task, operation, uri); err == nil || facts.reads != 1 {
		t.Fatal("late Node completion touched successor", err)
	}
}

func TestNodeOfflineGuardRevisionDriftCannotSealAStaleGrant(t *testing.T) {
	db, secrets, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := connections.Create(actor, ConnectionInput{Name: "revision", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "scope", NameNormalized: uuid.NewString(), RootPath: "root", RootPathNormalized: uuid.NewString(), Type: models.StorageTypePan115, ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err = db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := models.TransferNode{ID: uuid.NewString(), OwnerID: actor.User.ID, Name: "node", NameNormalized: uuid.NewString(), APIURL: "https://node.invalid", EncryptionPublicKey: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())}
	if err = db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	fired := false
	const callback = "review-revision-drift"
	if err = db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Model.(*models.ProviderOperationReceipt); !ok || fired {
			return
		}
		fired = true
		tx.AddError(tx.Model(&models.Connection{}).Where("id=?", connection.ID).Update("revision", 2).Error)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	nodes := NewTransferNodeService(db, NewAuditService(db), secrets)
	uri := "magnet:?xt=urn:btih:" + strings.Repeat("e", 40)
	grant := func() error {
		_, err := nodes.sealStorageSourceGrant(context.Background(), node, storage, "task", "operation", []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineSubmit}, uri)
		return err
	}
	if err = grant(); err == nil || !fired {
		t.Fatal("stale revision was sealed", err)
	}
	var count int64
	if err = db.Model(&models.NodeCredentialGrant{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("stale grant persisted", count, err)
	}
	if err = grant(); err != nil {
		t.Fatal("same-UID current revision could not recover original guard", err)
	}
}
