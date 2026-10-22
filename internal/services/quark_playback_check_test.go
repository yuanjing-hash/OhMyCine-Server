package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkPlaybackCheckSignedStatusOnlyMapping(t *testing.T) {
	for code, status := range map[string]int{quark.CodeAuth: http.StatusServiceUnavailable, quark.CodeLimited: http.StatusTooManyRequests, quark.CodeInvalid: http.StatusBadGateway, quark.CodeUnavailable: http.StatusServiceUnavailable, quark.CodeUnsupported: http.StatusServiceUnavailable, quark.CodeIdentity: http.StatusConflict, quark.CodeProof: http.StatusConflict, quark.CodeIncomplete: http.StatusBadGateway} {
		err := quarkPlaybackError(models.StorageTypeQuark, &quark.Failure{Code: code})
		if ProxyHTTPStatus(err) != status {
			t.Fatalf("%s status=%d", code, ProxyHTTPStatus(err))
		}
	}
}

type quarkPlaybackCheckDriver struct {
	*fakeCloudDriver
	readErr, linkErr error
	reads            int
}

func (d *quarkPlaybackCheckDriver) ResolveScopedItem(_ context.Context, _, _ string, id string) (cloud.Item, error) {
	d.reads++
	if d.readErr != nil {
		return cloud.Item{}, d.readErr
	}
	return d.items[id], nil
}
func (d *quarkPlaybackCheckDriver) DirectURL(ctx context.Context, r cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	if d.linkErr != nil {
		d.directURLCalls.Add(1)
		return cloud.TemporaryURL{}, d.linkErr
	}
	return d.fakeCloudDriver.DirectURL(ctx, r)
}

func quarkPlaybackCheckFixture(t *testing.T) (*SignedProxyService, *quarkPlaybackCheckDriver, Actor, uint) {
	t.Helper()
	_, store, library, _, entries, actor := playerSnapshotFixture(t)
	template, admin, _ := createQuarkConnection(t)
	connections := NewConnectionService(store.writeDB, NewAuditService(store.writeDB), template.credentials, template.registry, zerolog.Nop())
	driver := &quarkPlaybackCheckDriver{fakeCloudDriver: &fakeCloudDriver{items: map[string]cloud.Item{entries[0].ProviderID: {ID: entries[0].ProviderID, ParentID: "root", Name: "sample.mkv", Size: 128}}}}
	if err := connections.registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return driver, nil }); err != nil {
		t.Fatal(err)
	}
	connection, err := connections.Create(admin, ConnectionInput{Name: "playback-check", Provider: cloud.ProviderQuark, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	saveQuarkTestBundle(t, connections, connection.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization() })
	if err := store.writeDB.Model(&models.Storage{}).Where("id=?", library.StorageID).Updates(map[string]any{"type": models.StorageTypeQuark, "connection_id": connection.ID, "root_path": "root"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&library).Update("provider_root_id", "root").Error; err != nil {
		t.Fatal(err)
	}
	proxy, err := NewSignedProxyService(store.writeDB, template.credentials, connections, "https://server.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	proxy.SetCatalogSnapshotStore(store)
	return proxy, driver, actor, entries[0].ID
}

func TestQuarkPlaybackCheckReusesValidLinkAndRechecksAuthorization(t *testing.T) {
	proxy, driver, actor, id := quarkPlaybackCheckFixture(t)
	ctx := context.Background()
	// JSON reqwest and the existing Player bridge both omit User-Agent. Their
	// source TCP ports differ; the established cache fingerprint uses only host.
	check, err := proxy.CheckPlayerEntryDetails(ctx, actor, id, "", "127.0.0.1:10001")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(check)
	if string(encoded) != `{"ready":true,"version":1,"http_read_profile":"bounded_ranges_v1"}` {
		t.Fatalf("unexpected Quark playback check: %s", encoded)
	}
	stream, err := proxy.ResolvePlayerEntry(ctx, actor, id, "", "127.0.0.1:10002")
	if err != nil || stream.RedirectURL == "" || stream.File != nil || driver.directURLCalls.Load() != 1 {
		t.Fatalf("check/stream cache mismatch: calls=%d err=%v", driver.directURLCalls.Load(), err)
	}
	if denied, err := proxy.CheckPlayerEntryDetails(ctx, Actor{}, id, "", ""); ErrorCode(err) != CodePermissionDenied || denied != (PlayerPlaybackCheck{}) {
		t.Fatalf("missing permission: %v", err)
	}
	blocked := actor
	blocked.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeAllowlist}}
	reads := driver.reads
	if err := proxy.CheckPlayerEntry(ctx, blocked, id, "", "127.0.0.1:10002"); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("resource policy check bypass: %v", err)
	}
	if _, err := proxy.ResolvePlayerEntry(ctx, blocked, id, "", "127.0.0.1:10002"); ErrorCode(err) != CodePermissionDenied || driver.reads != reads {
		t.Fatalf("resource policy stream bypass/provider request: %v", err)
	}
	driver.readErr = &quark.Failure{Code: quark.CodeAuth}
	if err := proxy.CheckPlayerEntry(ctx, actor, id, "", "127.0.0.1:10003"); ErrorCode(err) != quark.CodeAuth {
		t.Fatalf("warm cache hid auth failure: %v", err)
	}
	if driver.directURLCalls.Load() != 1 {
		t.Fatal("denied check made another download-link request")
	}
}

func TestQuarkPlaybackCheckSafeFailures(t *testing.T) {
	proxy, driver, actor, id := quarkPlaybackCheckFixture(t)
	for _, code := range []string{quark.CodeAuth, quark.CodeLimited, quark.CodeInvalid, quark.CodeUnavailable, quark.CodeUnsupported, quark.CodeIdentity, quark.CodeProof, quark.CodeIncomplete} {
		t.Run(code, func(t *testing.T) {
			driver.linkErr = cloud.Error(code, true, errors.New("https://private.example/?token=secret Cookie=secret"))
			check, err := proxy.CheckPlayerEntryDetails(context.Background(), actor, id, "", "")
			if check != (PlayerPlaybackCheck{}) || ErrorCode(err) != code || ErrorMessage(err) == "" || strings.Contains(ErrorMessage(err), "secret") || strings.Contains(ErrorMessage(err), "private.example") {
				t.Fatalf("unsafe/masked error: code=%s message=%s", ErrorCode(err), ErrorMessage(err))
			}
		})
	}
	driver.linkErr = errors.New("unknown upstream body token=secret")
	if err := proxy.CheckPlayerEntry(context.Background(), actor, id, "", ""); ErrorCode(err) != CodeProxyUpstreamUnavailable || strings.Contains(ErrorMessage(err), "secret") {
		t.Fatalf("unknown leaked: %v", err)
	}
	for _, tc := range []struct {
		wait       time.Duration
		hasSeconds bool
	}{{1500 * time.Millisecond, true}, {-time.Second, false}, {25 * time.Hour, false}} {
		err := quarkPlaybackError(models.StorageTypeQuark, &quark.Failure{Code: quark.CodeLimited, RetryAfter: tc.wait})
		if strings.Contains(ErrorMessage(err), "2 秒") != tc.hasSeconds {
			t.Fatalf("cooldown: %s", ErrorMessage(err))
		}
	}
	if quarkPlaybackError(models.StorageTypePan115, &quark.Failure{Code: quark.CodeAuth}) != nil {
		t.Fatal("changed 115 error contract")
	}
}

func TestPlayerPlaybackCheckDoesNotOpenLocalOrResolve115(t *testing.T) {
	_, store, library, _, entries, actor := playerSnapshotFixture(t)
	proxy := &SignedProxyService{db: store.writeDB, catalogStore: store}
	// No physical file exists, no ConnectionService was provided. Checks must
	// inspect access/catalog only, without attempting file or cloud operations.
	for _, provider := range []string{models.StorageTypeLocal, models.StorageTypePan115} {
		if err := store.writeDB.Model(&models.Storage{}).Where("id=?", library.StorageID).Update("type", provider).Error; err != nil {
			t.Fatal(err)
		}
		check, err := proxy.CheckPlayerEntryDetails(context.Background(), actor, entries[0].ID, "", "")
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		encoded, _ := json.Marshal(check)
		if string(encoded) != `{"ready":true,"version":1}` {
			t.Fatalf("non-Quark profile leaked: %s", encoded)
		}
	}
	if err := store.writeDB.Model(&library).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := proxy.CheckPlayerEntry(context.Background(), actor, entries[0].ID, "", ""); err == nil {
		t.Fatal("disabled library accepted")
	}
}
