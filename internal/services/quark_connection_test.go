package services

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func createQuarkConnection(t *testing.T) (*ConnectionService, Actor, ConnectionSummary) {
	t.Helper()
	_, _, service, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := service.Create(actor, ConnectionInput{Name: "我的夸克", Provider: cloud.ProviderQuark, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	return service, actor, connection
}
func testQuarkAuthorization() *quark.Authorization {
	return &quark.Authorization{AccessToken: "access-secret", RefreshToken: "refresh-secret", DeviceID: "stable-device", AccountID: "private-account", Generation: 1, IdentityVerified: true}
}
func saveQuarkTestBundle(t *testing.T, s *ConnectionService, id uint, mutate func(*quark.Credentials)) quark.Credentials {
	t.Helper()
	record, bundle, err := s.quarkCredentials(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	previous := bundle.Revision
	mutate(&bundle)
	bundle.Revision++
	raw, _ := json.Marshal(bundle)
	if err = s.quarkCredentialPersister(id, record.Revision)(context.Background(), previous, raw); err != nil {
		t.Fatal(err)
	}
	return bundle
}
func TestQuarkConnectionEncryptedBundleSeparateFromLegacyPurpose(t *testing.T) {
	s, actor, created := createQuarkConnection(t)
	bundle := saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization(); c.Agent = testQuarkAuthorization() })
	var record models.Connection
	if err := s.db.First(&record, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(record.CredentialCiphertext, "secret") {
		t.Fatal("plaintext credential stored")
	}
	if _, err := s.credentials.Decrypt(connectionPurpose(created.ID), record.CredentialCiphertext); err == nil {
		t.Fatal("quark credential accepted legacy115 purpose")
	}
	status, err := s.QuarkStatus(context.Background(), actor, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Playback.Ready || !status.Management.Ready {
		t.Fatal("verified routes not projected")
	}
	listed, err := s.List(actor, cloud.ProviderQuark)
	if err != nil {
		t.Fatal(err)
	}
	for _, dto := range []any{status, listed} {
		raw, _ := json.Marshal(dto)
		for _, secret := range []string{bundle.TV.AccessToken, bundle.TV.RefreshToken, bundle.TV.AccountID, bundle.TV.DeviceID} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("private value leaked from DTO")
			}
		}
	}
	actor.Permissions[authz.PermissionConnectionsSecretsExport] = struct{}{}
	reveal := NewCredentialRevealService(s.db, s.audit, s.credentials)
	if _, err := reveal.reveal(CredentialRevealInput{ResourceType: CredentialResourceConnection, ResourceID: uintID(created.ID), Field: "credential"}); err == nil {
		t.Fatal("quark refresh bundle reveal allowed")
	}
}
func TestQuarkRotationCASAndCosmeticEditPreserveNewestCredential(t *testing.T) {
	s, actor, created := createQuarkConnection(t)
	first := saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization() })
	second := saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) { c.TV.AccessToken = "new-access"; c.TV.Generation++ })
	stale := first
	stale.Revision++
	raw, _ := json.Marshal(stale)
	if err := s.quarkCredentialPersister(created.ID, created.Revision)(context.Background(), first.Revision, raw); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale credential CAS accepted: %v", err)
	}
	name := "改名"
	if _, err := s.Update(actor, created.ID, UpdateConnectionInput{Name: &name, Revision: created.Revision}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	_, saved, err := s.quarkCredentials(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TV.AccessToken != second.TV.AccessToken || saved.Revision != second.Revision {
		t.Fatal("cosmetic edit rolled back rotated credential")
	}
}
func TestQuarkConnectionRejectsManualCredentialAndDisabledRotation(t *testing.T) {
	s, actor, created := createQuarkConnection(t)
	secret := "manual-cookie"
	if _, err := s.Update(actor, created.ID, UpdateConnectionInput{Cookie: &secret, Revision: created.Revision}, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("manual cookie accepted")
	}
	_, bundle, err := s.quarkCredentials(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	enabled := false
	if _, err = s.Update(actor, created.ID, UpdateConnectionInput{Enabled: &enabled, Revision: created.Revision}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	bundle.TV = testQuarkAuthorization()
	expected := bundle.Revision
	bundle.Revision++
	raw, _ := json.Marshal(bundle)
	if err = s.quarkCredentialPersister(created.ID, created.Revision)(context.Background(), expected, raw); ErrorCode(err) != CodeConflict {
		t.Fatal("disabled/config-changed credential writer accepted")
	}
}

type fakeQuarkAuthenticator struct{ starts, polls, confirms int }

func (f *fakeQuarkAuthenticator) Start(_ context.Context, route quark.Route, c quark.Credentials) (quark.Challenge, error) {
	f.starts++
	return quark.Challenge{Route: route, DeviceID: "stable-device", QueryToken: "private-query", QR: []byte("test-png"), Revision: c.Revision, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (f *fakeQuarkAuthenticator) Poll(ctx context.Context, ch quark.Challenge, c quark.Credentials, persist quark.PersistFunc) (quark.Credentials, bool, error) {
	f.polls++
	expected := c.Revision
	c.TV = testQuarkAuthorization()
	c.TV.IdentityVerified = false
	c.Revision++
	raw, _ := json.Marshal(c)
	if err := persist(ctx, expected, raw); err != nil {
		return c, false, err
	}
	return c, false, cloud.Error(quark.CodeUnavailable, true, nil)
}
func (f *fakeQuarkAuthenticator) Confirm(ctx context.Context, _ quark.Route, c quark.Credentials, persist quark.PersistFunc) (quark.Credentials, error) {
	f.confirms++
	expected := c.Revision
	c.TV.IdentityVerified = true
	c.Revision++
	raw, _ := json.Marshal(c)
	return c, persist(ctx, expected, raw)
}
func TestQuarkAuthorizationResumesCheckpointWithoutNewScanOrExchange(t *testing.T) {
	s, actor, created := createQuarkConnection(t)
	fake := &fakeQuarkAuthenticator{}
	s.quarkAuthenticator = fake
	session, err := s.StartQuarkAuth(context.Background(), actor, created.ID, "playback", RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartQuarkAuth(context.Background(), actor, created.ID, "management", RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatal("parallel authorization allowed")
	}
	other := actor
	other.User.ID++
	if _, err = s.PollQuarkAuth(context.Background(), other, created.ID, session.SessionID, RequestContext{}); ErrorCode(err) != CodeNotFound {
		t.Fatal("foreign actor polled session")
	}
	result, err := s.PollQuarkAuth(context.Background(), actor, created.ID, session.SessionID, RequestContext{})
	if err != nil || result.State != "retryable" {
		t.Fatalf("checkpoint response: %v %+v", err, result)
	}
	// Simulate process/session loss. The already saved token remains durable.
	s.removeQuarkSession(session.SessionID)
	resumed, err := s.StartQuarkAuth(context.Background(), actor, created.ID, "playback", RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.QRDataURL != "" {
		t.Fatal("checkpoint recovery unnecessarily requested another scan")
	}
	result, err = s.PollQuarkAuth(context.Background(), actor, created.ID, resumed.SessionID, RequestContext{})
	if err != nil || result.State != "complete" {
		t.Fatalf("resume: %v %+v", err, result)
	}
	if fake.starts != 1 || fake.polls != 1 || fake.confirms != 1 {
		t.Fatalf("unexpected replay: %+v", fake)
	}
	result, err = s.PollQuarkAuth(context.Background(), actor, created.ID, resumed.SessionID, RequestContext{})
	if err != nil || result.State != "complete" {
		t.Fatal("completed poll is not idempotent")
	}
}

func TestQuarkRootProofBoundToBothIdentitiesAndRoot(t *testing.T) {
	c := quark.NewCredentials()
	c.TV = testQuarkAuthorization()
	c.Agent = testQuarkAuthorization()
	c.Proofs = []quark.SpaceProof{{Version: 1, Generation: 1, TVAccount: c.TV.AccountID, AgentAccount: c.Agent.AccountID, TVRoot: "tv-root", AgentRoot: "agent-root", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now()}}
	if !c.HasProof("tv-root", "agent-root") {
		t.Fatal("expected scoped proof")
	}
	if c.HasProof("other-root", "agent-root") {
		t.Fatal("proof escaped root")
	}
	c.Agent.AccountID = "different-account"
	if c.HasProof("tv-root", "agent-root") {
		t.Fatal("proof survived account change")
	}
}

func TestQuarkDirectoryTokenSurvivesRefreshButNotIdentityOrConfigurationDrift(t *testing.T) {
	s, actor, created := createQuarkConnection(t)
	saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization() })
	directories := NewProviderDirectoryService(s, s.credentials)
	binding, err := directories.quarkDirectoryBinding(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := directories.sign(providerDirectoryClaims{ActorID: actor.User.ID, ConnectionID: created.ID, ItemID: "qualified-root", DisplayPath: "/媒体", Purpose: tokenPurposeSelect, Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) { c.TV.AccessToken = "rotated"; c.TV.Generation++ })
	if _, err = directories.resolve(actor, token, tokenPurposeSelect); err != nil {
		t.Fatalf("refresh invalidated same-account selection: %v", err)
	}
	saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) { c.TV.AccountID = "new-account" })
	if _, err = directories.resolve(actor, token, tokenPurposeSelect); ErrorCode(err) != CodeDirectoryTokenInvalid {
		t.Fatal("old account picker token accepted")
	}
	binding, err = directories.quarkDirectoryBinding(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err = directories.sign(providerDirectoryClaims{ActorID: actor.User.ID, ConnectionID: created.ID, ItemID: "qualified-root", DisplayPath: "/媒体", Purpose: tokenPurposeSelect, Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	name := "新配置"
	if _, err = s.Update(actor, created.ID, UpdateConnectionInput{Name: &name, Revision: created.Revision}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if _, err = directories.resolve(actor, token, tokenPurposeSelect); ErrorCode(err) != CodeDirectoryTokenInvalid {
		t.Fatal("old configuration picker token accepted")
	}
}

func TestQuarkConnectionBoundStorageRejectsIdentityReplacement(t *testing.T) {
	s, _, created := createQuarkConnection(t)
	previous := saveQuarkTestBundle(t, s, created.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization() })
	storage := models.Storage{Name: "绑定库", NameNormalized: "绑定库", Type: models.StorageTypeQuark, ConnectionID: &created.ID, RootPath: "quark-root", RootPathNormalized: "quark:test", Enabled: true}
	if err := s.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	next := previous
	replacement := *previous.TV
	replacement.AccountID = "wrong-account"
	next.TV = &replacement
	next.Revision++
	raw, _ := json.Marshal(next)
	if err := s.quarkCredentialPersister(created.ID, created.Revision)(context.Background(), previous.Revision, raw); ErrorCode(err) != CodeConflict {
		t.Fatalf("bound account changed: %v", err)
	}
	_, saved, err := s.quarkCredentials(context.Background(), created.ID)
	if err != nil || saved.TV.AccountID != previous.TV.AccountID {
		t.Fatal("failed identity replacement altered original credentials")
	}
}

func TestQuarkAuthorizationPresentationAllowsOnlyImageQRAndOfficialURL(t *testing.T) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	s := quarkAuthSession{state: "waiting", challenge: quark.Challenge{Route: quark.RouteTV, QR: encoded.Bytes(), AuthorizationURL: "https://evil.invalid/private-query"}}
	dto := s.summary("session")
	if !strings.HasPrefix(dto.QRDataURL, "data:image/jpeg;base64,") || dto.AuthorizationURL != "" {
		t.Fatal("unsafe or incorrect QR projection")
	}
	s.challenge.QR = []byte("<svg onload='alert(1)' />")
	if s.summary("session").QRDataURL != "" {
		t.Fatal("active image content accepted")
	}
}
