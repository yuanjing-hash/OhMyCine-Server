package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type recoveryQuarkDriver struct {
	*fakeCloudDriver
	inspector *quark.Client
	verify    func(context.Context, string, string) (quark.SpaceProof, error)
	root      string
}

func (d *recoveryQuarkDriver) ResolveDirectory(context.Context, string) (cloud.Item, error) {
	return cloud.Item{ID: d.root, IsDir: true}, nil
}
func (d *recoveryQuarkDriver) VerifyCommonSpace(ctx context.Context, id, path string) (quark.SpaceProof, error) {
	return d.verify(ctx, id, path)
}
func (d *recoveryQuarkDriver) CommonSpaceRecoveryState(ctx context.Context, id, path, root string) (bool, string, error) {
	return d.inspector.CommonSpaceRecoveryState(ctx, id, path, root)
}

func seedUnresolvedCommonSpace(t *testing.T, s *ConnectionService, id uint, c quark.Credentials, root, proofID string) (cloud.OperationReceipt, cloud.OperationReceipt) {
	t.Helper()
	data := bytes.Repeat([]byte{42}, 32)
	h := sha256.Sum256(data)
	name := ".omc-verify-" + hex.EncodeToString(h[:16])
	proofPayload, _ := json.Marshal(map[string]any{"path": "/媒体库", "tv_root": root, "agent_root": quark.QualifyID(quark.RouteAgent, c.Agent.AccountID, "agent-root"), "tv_account": c.TV.AccountID, "name": name, "bytes": data})
	uploadPayload, _ := json.Marshal(map[string]any{"parent": "agent-root", "name": name, "size": 32, "sha256": hex.EncodeToString(h[:]), "task_id": "private-original-task", "part_size": 64, "phase": "put_inflight", "inflight_part": 1})
	proof := cloud.OperationReceipt{ID: proofID, Provider: cloud.ProviderQuark, Route: "agent", AccountID: c.Agent.AccountID, Kind: "space_proof", Revision: 1, State: "prepared", Payload: proofPayload}
	upload := cloud.OperationReceipt{ID: proofID + "/upload", Provider: cloud.ProviderQuark, Route: "agent", AccountID: c.Agent.AccountID, Kind: "upload", Revision: 1, State: "prepared", Payload: uploadPayload}
	store := NewQuarkOperationStore(s.db, s.credentials, id)
	if err := store.Create(context.Background(), proof); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), upload); err != nil {
		t.Fatal(err)
	}
	upload.State = "unknown"
	upload.Revision++
	if err := store.Update(context.Background(), 1, upload); err != nil {
		t.Fatal(err)
	}
	return proof, upload
}

func quarkRootRecoveryFixture(t *testing.T) (*ConnectionService, Actor, ConnectionSummary, quark.Credentials, string, string) {
	t.Helper()
	s, actor, record := createQuarkConnection(t)
	c := saveQuarkTestBundle(t, s, record.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization(); c.Agent = testQuarkAuthorization() })
	root := quark.QualifyID(quark.RouteTV, c.TV.AccountID, "tv-root")
	base := quarkRootProofBase(record.ID, c, root, "/媒体库")
	seedUnresolvedCommonSpace(t, s, record.ID, c, root, base)
	return s, actor, record, c, root, base
}

func installRecoveryDriver(t *testing.T, s *ConnectionService, root string, verify func(context.Context, string, string) (quark.SpaceProof, error)) {
	t.Helper()
	if err := s.registry.Register(cloud.ProviderQuark, func(config cloud.Config) (cloud.Driver, error) {
		native, err := quark.NewClient(config)
		return &recoveryQuarkDriver{fakeCloudDriver: &fakeCloudDriver{}, inspector: native, root: root, verify: verify}, err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQuarkRootIndependentAttemptSurvivesLostResponseRestartAndConcurrentTabs(t *testing.T) {
	s, actor, record, _, root, base := quarkRootRecoveryFixture(t)
	var mu sync.Mutex
	ids := []string{}
	entered, release := make(chan struct{}), make(chan struct{})
	once := sync.Once{}
	installRecoveryDriver(t, s, root, func(_ context.Context, id, _ string) (quark.SpaceProof, error) {
		mu.Lock()
		ids = append(ids, id)
		mu.Unlock()
		once.Do(func() { close(entered); <-release })
		// Simulate a process/request ending after cursor commit but before
		// the new proof receipt or upload has been created.
		return quark.SpaceProof{}, cloud.Error(cloud.CodeMutationUnknown, true, nil)
	})
	ctx := context.Background()
	info, err := s.QuarkRootRecoveryInfo(ctx, actor, record.ID, root, "/媒体库")
	if err != nil || !info.Available || info.RecoveryToken == "" {
		t.Fatalf("no recovery: %v", err)
	}
	other, err := s.QuarkRootRecoveryInfo(ctx, actor, record.ID, root, "/媒体库")
	if err != nil || !other.Available || other.RecoveryToken == info.RecoveryToken {
		t.Fatal("independent tabs did not receive opaque capabilities")
	}
	store := NewQuarkOperationStore(s.db, s.credentials, record.ID)
	oldProof, _ := store.Load(ctx, base)
	oldUpload, _ := store.Load(ctx, base+"/upload")
	done := make(chan error, 3)
	go func() {
		_, err := s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{})
		done <- err
	}()
	<-entered
	go func() {
		_, err := s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", other.RecoveryToken, RequestContext{})
		done <- err
	}()
	go func() {
		_, err := s.VerifyQuarkRoot(ctx, actor, record.ID, root, "/媒体库", RequestContext{})
		done <- err
	}()
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; ErrorCode(err) != CodeConnectionUnavailable {
			t.Fatalf("unexpected result: %v", err)
		}
	}
	restarted := NewConnectionService(s.db, s.audit, s.credentials, s.registry, s.log)
	if _, err := restarted.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{}); ErrorCode(err) != CodeConnectionUnavailable {
		t.Fatalf("restart lost accepted intent: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 4 {
		t.Fatalf("calls=%d", len(ids))
	}
	for _, id := range ids {
		if id != quarkRootSuccessor(base) {
			t.Fatal("duplicate/ordinary request selected another attempt")
		}
	}
	if got, _ := store.Load(ctx, base); got.State != oldProof.State || got.Revision != oldProof.Revision || !bytes.Equal(got.Payload, oldProof.Payload) {
		t.Fatal("old proof was replaced")
	}
	if got, _ := store.Load(ctx, base+"/upload"); got.State != oldUpload.State || got.Revision != oldUpload.Revision || !bytes.Equal(got.Payload, oldUpload.Payload) {
		t.Fatal("old upload was replayed/reset")
	}
	var rows []models.QuarkOperationReceipt
	if err := s.db.Find(&rows).Error; err != nil || len(rows) != 3 {
		t.Fatalf("unexpected new receipt count=%d %v", len(rows), err)
	}
	for _, row := range rows {
		if row.Kind == "space_proof_cursor" && row.State != "confirmed" {
			t.Fatal("local cursor creates an eternal cloud write barrier")
		}
	}
}

func TestQuarkRootRecoveryRejectsStaleIntentBeforeCursorOrProvider(t *testing.T) {
	for _, change := range []string{"actor", "root", "path", "configuration", "credential_refresh", "account", "device", "expired", "checkpoint", "corrupt"} {
		t.Run(change, func(t *testing.T) {
			s, actor, record, _, root, base := quarkRootRecoveryFixture(t)
			calls := 0
			installRecoveryDriver(t, s, root, func(context.Context, string, string) (quark.SpaceProof, error) {
				calls++
				return quark.SpaceProof{}, errors.New("must not dispatch")
			})
			ctx := context.Background()
			info, err := s.QuarkRootRecoveryInfo(ctx, actor, record.ID, root, "/媒体库")
			if err != nil || !info.Available {
				t.Fatal(err)
			}
			path := "/媒体库"
			switch change {
			case "actor":
				actor.User.ID++
			case "root":
				root = quark.QualifyID(quark.RouteTV, "private-account", "another-root")
			case "path":
				path = "/其他目录"
			case "configuration":
				if err = s.db.Model(&models.Connection{}).Where("id=?", record.ID).Update("revision", record.Revision+1).Error; err != nil {
					t.Fatal(err)
				}
			case "credential_refresh", "account", "device":
				saveQuarkTestBundle(t, s, record.ID, func(c *quark.Credentials) {
					c.Agent.Generation++
					c.Agent.AccessToken = "new-private-access"
					if change == "account" {
						c.Agent.AccountID = "another-account"
					}
					if change == "device" {
						c.Agent.DeviceID = "another-device"
					}
				})
			case "expired":
				var claims quarkRootRecoveryClaims
				raw, _ := s.credentials.Decrypt(quarkRootRecoveryPurpose, info.RecoveryToken)
				_ = json.Unmarshal([]byte(raw), &claims)
				claims.Expires = time.Now().Add(-time.Minute).Unix()
				rawBytes, _ := json.Marshal(claims)
				info.RecoveryToken, _ = s.credentials.Encrypt(quarkRootRecoveryPurpose, string(rawBytes))
			case "checkpoint":
				store := NewQuarkOperationStore(s.db, s.credentials, record.ID)
				u, _ := store.Load(ctx, base+"/upload")
				old := u.Revision
				u.Revision++
				if err = store.Update(ctx, old, u); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				info.RecoveryToken = "invalid-private-token"
			}
			if _, err = s.StartQuarkRootAttempt(ctx, actor, record.ID, root, path, info.RecoveryToken, RequestContext{}); ErrorCode(err) != CodeConflict {
				t.Fatalf("stale %s accepted: %v", change, err)
			}
			var count int64
			if err = s.db.Model(&models.QuarkOperationReceipt{}).Where("kind=?", "space_proof_cursor").Count(&count).Error; err != nil || count != 0 || calls != 0 {
				t.Fatal("stale intent created cursor or dispatched provider")
			}
		})
	}
}

func TestQuarkRootRecoveryAcceptedIntentPermitsRefreshAndExpiryButFencesOldAncestors(t *testing.T) {
	s, actor, record, c, root, base := quarkRootRecoveryFixture(t)
	ids := []string{}
	installRecoveryDriver(t, s, root, func(_ context.Context, id, _ string) (quark.SpaceProof, error) {
		ids = append(ids, id)
		return quark.SpaceProof{}, cloud.Error(cloud.CodeMutationUnknown, true, nil)
	})
	ctx := context.Background()
	info, _ := s.QuarkRootRecoveryInfo(ctx, actor, record.ID, root, "/媒体库")
	_, _ = s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{})
	saveQuarkTestBundle(t, s, record.ID, func(c *quark.Credentials) { c.Agent.Generation++; c.Agent.AccessToken = "refreshed-private-access" })
	if _, err := s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{}); ErrorCode(err) != CodeConnectionUnavailable {
		t.Fatalf("accepted refresh continuity lost: %v", err)
	}
	// Expiry limits accepting a new intent. Once selected durably, retrying
	// that intent after a restart must continue the same successor.
	var claims quarkRootRecoveryClaims
	raw, err := s.credentials.Decrypt(quarkRootRecoveryPurpose, info.RecoveryToken)
	if err != nil || json.Unmarshal([]byte(raw), &claims) != nil {
		t.Fatal("cannot inspect fixture capability")
	}
	claims.Expires = time.Now().Add(-time.Minute).Unix()
	expiredClaims, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	expiredToken, err := s.credentials.Encrypt(quarkRootRecoveryPurpose, string(expiredClaims))
	if err != nil {
		t.Fatal(err)
	}
	cursorReceipt, _, err := s.loadQuarkRootCursor(ctx, record.ID, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewConnectionService(s.db, s.audit, s.credentials, s.registry, s.log)
	if _, err := restarted.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", expiredToken, RequestContext{}); ErrorCode(err) != CodeConnectionUnavailable {
		t.Fatalf("accepted expired intent lost after restart: %v", err)
	}
	currentReceipt, currentCursor, err := s.loadQuarkRootCursor(ctx, record.ID, base, nil)
	if err != nil || currentReceipt.Revision != cursorReceipt.Revision || currentCursor.Active != quarkRootSuccessor(base) || ids[len(ids)-1] != currentCursor.Active {
		t.Fatal("accepted expired intent changed the durable successor")
	}
	child := quarkRootSuccessor(base)
	seedUnresolvedCommonSpace(t, s, record.ID, c, root, child)
	newInfo, err := s.QuarkRootRecoveryInfo(ctx, actor, record.ID, root, "/媒体库")
	if err != nil || !newInfo.Available {
		t.Fatal("new explicit generation not offered")
	}
	_, _ = s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", newInfo.RecoveryToken, RequestContext{})
	before := len(ids)
	if _, err := s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{}); ErrorCode(err) != CodeConflict || len(ids) != before {
		t.Fatal("old ancestor reselected a stale proof")
	}
	_, cursor, err := s.loadQuarkRootCursor(ctx, record.ID, base, nil)
	if err != nil || cursor.Active != quarkRootSuccessor(child) {
		t.Fatal("late ancestor changed active attempt")
	}
}

func TestQuarkRootRecoveryPermissionsAndPublicProjection(t *testing.T) {
	s, actor, record, _, root, _ := quarkRootRecoveryFixture(t)
	installRecoveryDriver(t, s, root, func(context.Context, string, string) (quark.SpaceProof, error) {
		t.Fatal("inspection called provider")
		return quark.SpaceProof{}, nil
	})
	for _, permission := range []string{authz.PermissionConnectionsRead, authz.PermissionConnectionsUpdate, authz.PermissionStoragesBrowse, authz.PermissionStoragesCreate} {
		copyActor := actor
		copyActor.Permissions = map[string]struct{}{}
		for p := range actor.Permissions {
			if p != permission {
				copyActor.Permissions[p] = struct{}{}
			}
		}
		if permission == authz.PermissionStoragesCreate {
			delete(copyActor.Permissions, authz.PermissionStoragesUpdate)
		}
		if _, err := s.QuarkRootRecoveryInfo(context.Background(), copyActor, record.ID, root, "/媒体库"); ErrorCode(err) != CodePermissionDenied {
			t.Fatalf("missing %s accepted: %v", permission, err)
		}
	}
	info, err := s.QuarkRootRecoveryInfo(context.Background(), actor, record.ID, root, "/媒体库")
	if err != nil || !info.Available {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(info)
	for _, secret := range []string{"private-account", "private-original-task", "agent-root", "tv-root", ".omc-verify-", "access-secret", "refresh-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private checkpoint escaped public recovery DTO")
		}
	}
}

func TestQuarkRootOrdinaryContinuationCannotReplaceAcceptedAttemptScope(t *testing.T) {
	for _, change := range []string{"configuration", "device"} {
		t.Run(change, func(t *testing.T) {
			s, actor, record, _, root, _ := quarkRootRecoveryFixture(t)
			calls := 0
			installRecoveryDriver(t, s, root, func(context.Context, string, string) (quark.SpaceProof, error) {
				calls++
				return quark.SpaceProof{}, cloud.Error(cloud.CodeMutationUnknown, true, nil)
			})
			ctx := context.Background()
			info, _ := s.QuarkRootRecoveryInfo(ctx, actor, record.ID, root, "/媒体库")
			_, _ = s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{})
			if change == "configuration" {
				if err := s.db.Model(&models.Connection{}).Where("id=?", record.ID).Update("revision", record.Revision+1).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				saveQuarkTestBundle(t, s, record.ID, func(c *quark.Credentials) { c.Agent.DeviceID = "replacement-device" })
			}
			before := calls
			if _, err := s.VerifyQuarkRoot(ctx, actor, record.ID, root, "/媒体库", RequestContext{}); ErrorCode(err) != CodeConflict || calls != before {
				t.Fatal("ordinary path dispatched accepted intent under replaced scope")
			}
			if _, err := s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{}); ErrorCode(err) != CodeConflict || calls != before {
				t.Fatal("accepted token replaced its scope")
			}
		})
	}
}

func TestQuarkReservedProofHistoryDoesNotBlockMediaObservation(t *testing.T) {
	s, actor, record, _, root, base := quarkRootRecoveryFixture(t)
	installRecoveryDriver(t, s, root, func(context.Context, string, string) (quark.SpaceProof, error) {
		return quark.SpaceProof{}, cloud.Error(cloud.CodeMutationUnknown, true, nil)
	})
	ctx := context.Background()
	store := &quarkOperationStore{db: s.db, credentials: s.credentials, connectionID: record.ID}
	oldProof, _ := store.Load(ctx, base)
	oldUpload, _ := store.Load(ctx, base+"/upload")
	// Simulate legacy metadata from before reserved-pair classification.
	for id, kind := range map[string]string{base: "space_proof", base + "/upload": "upload"} {
		if err := s.db.Model(&models.QuarkOperationReceipt{}).Where("id=?", store.key(id)).Update("kind", kind).Error; err != nil {
			t.Fatal(err)
		}
	}
	var originalRows []models.QuarkOperationReceipt
	if err := s.db.Where("id IN ?", []string{store.key(base), store.key(base + "/upload")}).Find(&originalRows).Error; err != nil || len(originalRows) != 2 {
		t.Fatal("cannot snapshot original encrypted history")
	}
	scope := models.ProviderObservationScope{ID: "proof-library-scope", ConnectionID: record.ID}
	if err := s.db.Create(&scope).Error; err != nil {
		t.Fatal(err)
	}
	observer := &ProviderObservationService{db: s.db}
	if pending, err := observer.scopeHasPendingWritesTx(s.db, scope.ID); err != nil || !pending {
		t.Fatal("legacy unknown was silently exempt before validation")
	}
	info, err := s.QuarkRootRecoveryInfo(ctx, actor, record.ID, root, "/媒体库")
	if err != nil || !info.Available {
		t.Fatal(err)
	}
	_, _ = s.StartQuarkRootAttempt(ctx, actor, record.ID, root, "/媒体库", info.RecoveryToken, RequestContext{})
	if pending, err := observer.scopeHasPendingWritesTx(s.db, scope.ID); err != nil || pending {
		t.Fatal("owned excluded challenge caused permanent media-observation hold")
	}
	if current, _ := store.Load(ctx, base); current.State != oldProof.State || current.Revision != oldProof.Revision || !bytes.Equal(current.Payload, oldProof.Payload) {
		t.Fatal("classification rewrote old proof")
	}
	if current, _ := store.Load(ctx, base+"/upload"); current.State != oldUpload.State || current.Revision != oldUpload.Revision || !bytes.Equal(current.Payload, oldUpload.Payload) {
		t.Fatal("classification rewrote unknown cloud task")
	}
	for _, original := range originalRows {
		var current models.QuarkOperationReceipt
		if err := s.db.First(&current, "id=?", original.ID).Error; err != nil || current.State != original.State || current.Revision != original.Revision || !bytes.Equal(current.CheckpointCiphertext, original.CheckpointCiphertext) {
			t.Fatal("classification changed encrypted history instead of metadata only")
		}
		if current.Kind != "reserved_space_proof" && current.Kind != "reserved_space_proof_upload" {
			t.Fatal("validated proof pair did not receive reserved metadata")
		}
	}
	// A same-name, same-size ordinary upload lacks the paired private proof.
	ordinary := oldUpload
	ordinary.ID = "ordinary/upload"
	ordinary.Revision = 1
	ordinary.State = "prepared"
	if err := store.Create(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	ordinary.State = "unknown"
	ordinary.Revision++
	if err := store.Update(ctx, 1, ordinary); err != nil {
		t.Fatal(err)
	}
	if pending, err := observer.scopeHasPendingWritesTx(s.db, scope.ID); err != nil || !pending {
		t.Fatal("reserved filename bypassed ordinary unresolved-write hold")
	}
	for _, kind := range []string{"reserved_space_proof", "reserved_space_proof_upload"} {
		asserted := ordinary
		asserted.ID = "asserted-" + kind
		asserted.Kind = kind
		asserted.State = "prepared"
		asserted.Revision = 1
		if err := store.Create(ctx, asserted); !errors.Is(err, ErrObservationInvalid) {
			t.Fatal("caller-supplied reserved classification bypassed pair ownership")
		}
	}
}
