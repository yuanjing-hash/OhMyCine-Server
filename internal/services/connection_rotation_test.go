package services

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

const rotatedPan115Cookie = "UID=100_B2; CID=rotated-cid; SEID=rotated-seid"

type rotationProbeDriver struct {
	*fakeCloudDriver
	probe func() (cloud.Account, error)
}

func (d *rotationProbeDriver) Probe(context.Context) (cloud.Account, error) { return d.probe() }

func setRotationRegistry(t *testing.T, service *ConnectionService, build func(cloud.Config) cloud.Driver) {
	t.Helper()
	service.registry = cloud.NewRegistry()
	if err := service.registry.Register(cloud.ProviderPan115, func(c cloud.Config) (cloud.Driver, error) { return build(c), nil }); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionRotationPreservesPendingPhysicalEvidenceAndSource(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		for _, state := range []string{"admitted", "entered", "quiescent"} {
			t.Run(state+map[bool]string{true: "_versioned", false: "_legacy"}[versioned], func(t *testing.T) {
				store, service, actor, library, connection := catalogConnectionFixture(t, versioned)
				oldDriver := &fakeCloudDriver{err: cloud.Error(cloud.CodeAuthExpired, false, nil)}
				newDriver := &fakeCloudDriver{account: cloud.Account{ID: "100", Name: "renewed"}}
				setRotationRegistry(t, service, func(c cloud.Config) cloud.Driver {
					if c.Cookie == rotatedPan115Cookie {
						return newDriver
					}
					return oldDriver
				})
				if _, _, err := service.driver(connection.ID); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: "pending-renewal", Revision: 3, State: state, OwnerDigest: "unchanged", EnteredAt: now, UpdatedAt: now}
				if err := store.writeDB.Create(&proof).Error; err != nil {
					t.Fatal(err)
				}
				if err := store.writeDB.First(&proof, proof.ID).Error; err != nil {
					t.Fatal(err)
				}
				beforeStorage, profile := catalogSourceContext(t, store, library)
				beforeSource := mediaLibraryScanSourceFingerprint(library, beforeStorage, profile)
				var headsBefore []models.CatalogHead
				if err := store.writeDB.Where("library_id=?", library.ID).Find(&headsBefore).Error; err != nil {
					t.Fatal(err)
				}
				cookie := rotatedPan115Cookie
				result, err := service.Update(actor, connection.ID, UpdateConnectionInput{Cookie: &cookie, Revision: connection.Revision}, RequestContext{})
				if err != nil {
					t.Fatal(err)
				}
				if result.Account.ID != "100" || result.Health.Status != "online" || newDriver.probeCalls != 1 || oldDriver.probeCalls != 0 {
					t.Fatal("renewal did not validate only the new credential")
				}
				var after models.CatalogPhysicalWrite
				if err := store.writeDB.First(&after, proof.ID).Error; err != nil || !reflect.DeepEqual(proof, after) {
					t.Fatal("physical recovery evidence changed")
				}
				afterStorage, _ := catalogSourceContext(t, store, library)
				if mediaLibraryScanSourceFingerprint(library, afterStorage, profile) != beforeSource || afterStorage.CatalogConnectionEpoch != beforeStorage.CatalogConnectionEpoch || afterStorage.CatalogConnectionRevision != beforeStorage.CatalogConnectionRevision {
					t.Fatal("renewal fenced the recoverable source")
				}
				var headsAfter []models.CatalogHead
				if err := store.writeDB.Where("library_id=?", library.ID).Find(&headsAfter).Error; err != nil || !reflect.DeepEqual(headsBefore, headsAfter) {
					t.Fatal("renewal changed catalog heads")
				}
				_, recoveryDriver, err := service.driver(connection.ID)
				if err != nil || recoveryDriver != newDriver {
					t.Fatal("recovery still receives the expired cached driver")
				}
				name := "cosmetic update"
				updated, err := service.Update(actor, connection.ID, UpdateConnectionInput{Name: &name, Revision: result.Revision}, RequestContext{})
				if err != nil || updated.Account.ID != "100" {
					t.Fatal("cosmetic update lost account binding")
				}
				var audits []models.AuditLog
				if err := store.writeDB.Find(&audits).Error; err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal([]any{result, audits})
				if strings.Contains(string(encoded), "rotated-seid") || strings.Contains(string(encoded), "rotated-cid") {
					t.Fatal("renewal leaked secrets")
				}
			})
		}
	}
}

func TestConnectionRotationRejectedWithoutCredentialMutation(t *testing.T) {
	for _, tc := range []struct {
		name, binding, oldCookie, nextCookie, probedID string
		probeErr                                       error
		wantProbe                                      bool
	}{
		{name: "expired", probedID: "100", probeErr: cloud.Error(cloud.CodeAuthExpired, false, errors.New("secret-upstream-body")), wantProbe: true},
		{name: "rate_limited", probeErr: cloud.Error(cloud.CodeRateLimited, true, nil), wantProbe: true},
		{name: "different_uid", nextCookie: "UID=200_B2; CID=new; SEID=new", probedID: "200"},
		{name: "probe_identity_mismatch", probedID: "200", wantProbe: true},
		{name: "probe_identity_empty", wantProbe: true},
		{name: "trusted_binding_wins", binding: "200", probedID: "100"},
		{name: "unknown_legacy_identity", oldCookie: "UID=unknown; CID=old; SEID=old", probedID: "100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &fakeCloudDriver{account: cloud.Account{ID: tc.probedID}, err: tc.probeErr}
			db, _, service, actor := newConnectionTestService(t, driver)
			old := tc.oldCookie
			if old == "" {
				old = testPan115Cookie
			}
			created, err := service.Create(actor, ConnectionInput{Name: "rotation", Provider: cloud.ProviderPan115, Cookie: old, Enabled: true}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.Connection{}).Where("id=?", created.ID).Update("account_id", tc.binding).Error; err != nil {
				t.Fatal(err)
			}
			var before, after models.Connection
			if err := db.First(&before, created.ID).Error; err != nil {
				t.Fatal(err)
			}
			next := tc.nextCookie
			if next == "" {
				next = rotatedPan115Cookie
			}
			_, err = service.Update(actor, created.ID, UpdateConnectionInput{Cookie: &next, Revision: created.Revision}, RequestContext{})
			if err == nil || strings.Contains(err.Error(), "secret-upstream-body") {
				t.Fatal("unsafe/missing rejection")
			}
			if err := db.First(&after, created.ID).Error; err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rejection changed saved connection")
			}
			if (driver.probeCalls > 0) != tc.wantProbe {
				t.Fatalf("probe calls=%d", driver.probeCalls)
			}
		})
	}
}

func TestConnectionRotationRejectsProbeBindingRace(t *testing.T) {
	db, _, service, actor := newConnectionTestService(t, &fakeCloudDriver{})
	created, err := service.Create(actor, ConnectionInput{Name: "rotation", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var before models.Connection
	if err := db.First(&before, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	setRotationRegistry(t, service, func(cloud.Config) cloud.Driver {
		return &rotationProbeDriver{fakeCloudDriver: &fakeCloudDriver{}, probe: func() (cloud.Account, error) {
			// This write also proves Probe runs outside the credential transaction.
			if err := db.Model(&models.Connection{}).Where("id=?", created.ID).Update("account_id", "200").Error; err != nil {
				t.Fatal(err)
			}
			return cloud.Account{ID: "100"}, nil
		}}
	})
	cookie := rotatedPan115Cookie
	if _, err := service.Update(actor, created.ID, UpdateConnectionInput{Cookie: &cookie, Revision: created.Revision}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("race accepted: %v", err)
	}
	var after models.Connection
	if err := db.First(&after, created.ID).Error; err != nil || after.AccountID != "200" || after.CredentialCiphertext != before.CredentialCiphertext || after.Revision != before.Revision {
		t.Fatal("race replaced credential or binding")
	}
}

func TestConnectionRotationAllowsOriginalRepairRecoveryWithoutClearingCheckpoints(t *testing.T) {
	store, service, actor, library, connection := catalogConnectionFixture(t, false)
	newDriver := &fakeCloudDriver{account: cloud.Account{ID: "100"}}
	setRotationRegistry(t, service, func(cloud.Config) cloud.Driver { return newDriver })
	now := time.Now().UTC()
	repair := models.MediaLibraryStructureRepair{ID: "renewal-repair", OwnerID: actor.User.ID, LibraryID: library.ID, Scope: models.MediaLibraryStructureScopeWork, PlanJSON: "{}", StateJSON: `{"completed":["already-moved"]}`, Phase: "executing", CreatedAt: now, UpdatedAt: now}
	input := CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, ActorID: actor.User.ID}
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		return RegisterCatalogPhysicalOwnerTx(tx, input, func(tx *gorm.DB) error { return tx.Create(&repair).Error })
	}); err != nil {
		t.Fatal(err)
	}
	var original CatalogPhysicalWritePermit
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		var err error
		original, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, original) }); err != nil {
		t.Fatal(err)
	}
	cookie := rotatedPan115Cookie
	if _, err := service.Update(actor, connection.ID, UpdateConnectionInput{Cookie: &cookie, Revision: connection.Revision}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var resumed CatalogPhysicalWritePermit
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		var err error
		resumed, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatalf("original repair cannot resume after renewal: %v", err)
	}
	if resumed.evidence.Revision != original.evidence.Revision+1 {
		t.Fatal("recovery did not fence prior callbacks")
	}
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error { return QuiesceCatalogPhysicalWriteTx(tx, original) }); err == nil {
		t.Fatal("old callback changed resumed proof")
	}
	var recovered models.MediaLibraryStructureRepair
	if err := store.writeDB.First(&recovered, "id=?", repair.ID).Error; err != nil || recovered.StateJSON != repair.StateJSON {
		t.Fatal("renewal erased completed checkpoints")
	}
	_, driver, err := service.driver(connection.ID)
	if err != nil || driver != newDriver {
		t.Fatal("resumed repair cannot obtain renewed driver")
	}
}

func TestConnectionProbeCannotReplaceBoundAccount(t *testing.T) {
	driver := &fakeCloudDriver{account: cloud.Account{ID: "200"}}
	db, _, service, actor := newConnectionTestService(t, driver)
	created, err := service.Create(actor, ConnectionInput{Name: "bound", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Connection{}).Where("id=?", created.ID).Update("account_id", "100").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Test(context.Background(), actor, created.ID, RequestContext{}); err == nil {
		t.Fatal("mismatched probe succeeded")
	}
	var after models.Connection
	if err := db.First(&after, created.ID).Error; err != nil || after.AccountID != "100" || after.LastHealthStatus != "offline" {
		t.Fatal("probe rebound the account")
	}
}

func TestConnectionOldProbeCannotMarkRenewedCookieExpired(t *testing.T) {
	db, _, service, actor := newConnectionTestService(t, &fakeCloudDriver{})
	created, err := service.Create(actor, ConnectionInput{Name: "probe-race", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	newDriver := &fakeCloudDriver{account: cloud.Account{ID: "100"}}
	setRotationRegistry(t, service, func(c cloud.Config) cloud.Driver {
		if c.Cookie == rotatedPan115Cookie {
			return newDriver
		}
		return &rotationProbeDriver{fakeCloudDriver: &fakeCloudDriver{}, probe: func() (cloud.Account, error) {
			cookie := rotatedPan115Cookie
			if _, err := service.Update(actor, created.ID, UpdateConnectionInput{Cookie: &cookie, Revision: created.Revision}, RequestContext{}); err != nil {
				t.Fatal(err)
			}
			return cloud.Account{}, cloud.Error(cloud.CodeAuthExpired, false, nil)
		}}
	})
	if _, err := service.Test(context.Background(), actor, created.ID, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale probe committed: %v", err)
	}
	var after models.Connection
	if err := db.First(&after, created.ID).Error; err != nil || after.LastHealthStatus != "online" || after.LastHealthErrorCode != "" || after.AccountID != "100" {
		t.Fatal("old probe invalidated renewed credential")
	}
}

func TestConnectionCosmeticUpdateRetainsExpiredHealth(t *testing.T) {
	db, _, service, actor := newConnectionTestService(t, &fakeCloudDriver{})
	created, err := service.Create(actor, ConnectionInput{Name: "expired", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Connection{}).Where("id=?", created.ID).Updates(map[string]any{"account_id": "100", "last_health_status": "offline", "last_health_error_code": cloud.CodeAuthExpired}).Error; err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	updated, err := service.Update(actor, created.ID, UpdateConnectionInput{Name: &name, Revision: created.Revision}, RequestContext{})
	if err != nil || updated.Account.ID != "100" || updated.Health.Status != "offline" || updated.Health.ErrorCode != cloud.CodeAuthExpired {
		t.Fatal("cosmetic update erased credential failure")
	}
}

func TestConnectionCredentialGuardPersistsExpiryAndFencesLateFailure(t *testing.T) {
	db, _, service, actor := newConnectionTestService(t, &fakeCloudDriver{account: cloud.Account{ID: "100"}})
	created, err := service.Create(actor, ConnectionInput{Name: "guarded", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.Create(actor, ConnectionInput{Name: "independent", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	service.recordConnectionCredentialFailure(created.ID, created.Revision, cloud.Error(cloud.CodeRateLimited, true, nil))
	if err := service.guardConnectionCredential(context.Background(), created.ID, created.Revision); err != nil {
		t.Fatal("rate limiting invalidated connection")
	}
	service.recordConnectionCredentialFailure(created.ID, created.Revision, cloud.Error(cloud.CodeAuthExpired, false, nil))
	if code, _ := cloud.ErrorInfo(service.guardConnectionCredential(context.Background(), created.ID, created.Revision)); code != cloud.CodeAuthExpired {
		t.Fatal("confirmed expired credential was not guarded")
	}
	if err := service.guardConnectionCredential(context.Background(), other.ID, other.Revision); err != nil {
		t.Fatal("unrelated connection was paused")
	}
	cookie := rotatedPan115Cookie
	renewed, err := service.Update(actor, created.ID, UpdateConnectionInput{Cookie: &cookie, Revision: created.Revision}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	service.recordConnectionCredentialFailure(created.ID, created.Revision, cloud.Error(cloud.CodeAuthExpired, false, nil))
	if err := service.guardConnectionCredential(context.Background(), created.ID, renewed.Revision); err != nil {
		t.Fatal("old failure invalidated renewed connection")
	}
	if err := service.guardConnectionCredential(context.Background(), created.ID, created.Revision); err == nil {
		t.Fatal("old driver allowed new calls")
	}
	var after models.Connection
	if err := db.First(&after, created.ID).Error; err != nil || after.LastHealthStatus != "online" {
		t.Fatal("renewed health was lost")
	}
}

func TestConnectionCosmeticCommitRetainsConcurrentAuthFailure(t *testing.T) {
	db, _, service, actor := newConnectionTestService(t, &fakeCloudDriver{})
	created, err := service.Create(actor, ConnectionInput{Name: "race", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	injected := false
	const hook = "test:connection-auth-failure-after-read"
	if err := db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		if !injected && tx.Statement.Table == "connections" {
			injected = true
			service.recordConnectionCredentialFailure(created.ID, created.Revision, cloud.Error(cloud.CodeAuthExpired, false, nil))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(hook) })
	name := "renamed"
	updated, err := service.Update(actor, created.ID, UpdateConnectionInput{Name: &name, Revision: created.Revision}, RequestContext{})
	if err != nil || !injected || updated.Health.Status != "offline" || updated.Health.ErrorCode != cloud.CodeAuthExpired {
		t.Fatal("concurrent credential failure erased by cosmetic save")
	}
}

func TestConnectionLateSuccessfulProbeCannotClearAuthFailure(t *testing.T) {
	db, _, service, actor := newConnectionTestService(t, &fakeCloudDriver{})
	created, err := service.Create(actor, ConnectionInput{Name: "late-success", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	setRotationRegistry(t, service, func(cloud.Config) cloud.Driver {
		return &rotationProbeDriver{fakeCloudDriver: &fakeCloudDriver{}, probe: func() (cloud.Account, error) {
			service.recordConnectionCredentialFailure(created.ID, created.Revision, cloud.Error(cloud.CodeAuthExpired, false, nil))
			return cloud.Account{ID: "100"}, nil
		}}
	})
	if _, err := service.Test(context.Background(), actor, created.ID, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatal("late success cleared authentication failure")
	}
	var current models.Connection
	if err := db.First(&current, created.ID).Error; err != nil || current.LastHealthErrorCode != cloud.CodeAuthExpired {
		t.Fatal("durable auth failure lost")
	}
}
