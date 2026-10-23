package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const libraryFixtureAccount = "library-private-account"

type libraryQuarkDriver struct {
	routerCloudDriver
	lists atomic.Int32
}

func (*libraryQuarkDriver) Provider() string { return cloud.ProviderQuark }
func libraryQuarkID(raw string) string {
	return quark.QualifyID(quark.RouteTV, libraryFixtureAccount, raw)
}
func (*libraryQuarkDriver) Stat(_ context.Context, id string) (cloud.Item, error) {
	for _, item := range []cloud.Item{
		{ID: libraryQuarkID("0"), Name: "夸克网盘", IsDir: true},
		{ID: libraryQuarkID("media"), ParentID: libraryQuarkID("0"), Name: "媒体", IsDir: true},
		{ID: libraryQuarkID("movies"), ParentID: libraryQuarkID("media"), Name: "电影", IsDir: true},
	} {
		if item.ID == id {
			return item, nil
		}
	}
	return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
}
func (d *libraryQuarkDriver) ResolveDirectory(ctx context.Context, path string) (cloud.Item, error) {
	ids := map[string]string{"/": "0", "/媒体": "media", "/媒体/电影": "movies"}
	if raw, ok := ids[path]; ok {
		return d.Stat(ctx, libraryQuarkID(raw))
	}
	return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
}
func (d *libraryQuarkDriver) List(ctx context.Context, parent string, _ cloud.PageRequest) (cloud.Page, error) {
	d.lists.Add(1)
	items := []cloud.Item{}
	if parent == libraryQuarkID("media") {
		item, _ := d.Stat(ctx, libraryQuarkID("movies"))
		items = append(items, item)
	} else if parent != libraryQuarkID("movies") {
		return cloud.Page{}, cloud.Error(cloud.CodeNotFound, false, nil)
	}
	total := int64(len(items))
	return cloud.Page{Items: items, Total: &total}, nil
}

type librarySelectionFixture struct {
	client  *testClient
	driver  *libraryQuarkDriver
	storage models.Storage
	profile uint
	bundle  quark.Credentials
}

// This represents an existing, verified Storage. No proof upload or provider
// network is needed to exercise the real picker, token decoder and CRUD routes.
func newLibrarySelectionFixture(t *testing.T) *librarySelectionFixture {
	t.Helper()
	driver := &libraryQuarkDriver{}
	client := newTestClient(t, driver)
	client.setup(t)
	status, envelope := client.request(t, http.MethodPost, "/api/v1/connections", map[string]any{"name": "夸克账号", "provider": "quark", "enabled": true}, true)
	var connection struct {
		ID uint `json:"id"`
	}
	if status != http.StatusCreated || json.Unmarshal(envelope.Data, &connection) != nil {
		t.Fatalf("connection status=%d message=%s", status, envelope.Message)
	}
	bundle := quark.NewCredentials()
	bundle.Revision = 1
	bundle.TV = &quark.Authorization{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", DeviceID: "fixture-device", AccountID: libraryFixtureAccount, IdentityVerified: true, Generation: 1}
	f := &librarySelectionFixture{client: client, driver: driver, bundle: bundle}
	f.saveBundle(t, connection.ID)
	if err := client.db.Model(&models.Connection{}).Where("id = ?", connection.ID).Update("account_id", libraryFixtureAccount).Error; err != nil {
		t.Fatal(err)
	}
	f.storage = models.Storage{Name: "夸克媒体", NameNormalized: "夸克媒体", Type: models.StorageTypeQuark, RootPath: libraryQuarkID("media"), RootDisplayPath: "/媒体", RootPathNormalized: "quark-fixture-media", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{}`}
	if err := client.db.Create(&f.storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := client.db.First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	f.profile = profile.ID
	return f
}

func (f *librarySelectionFixture) saveBundle(t *testing.T, id uint) {
	t.Helper()
	raw, err := json.Marshal(f.bundle)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := f.client.credentials.Encrypt(fmt.Sprintf("connection:%d:quark:authorizations:v1", id), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.db.Model(&models.Connection{}).Where("id = ?", id).Update("credential_ciphertext", sealed).Error; err != nil {
		t.Fatal(err)
	}
}

func (f *librarySelectionFixture) browse(t *testing.T) services.DirectoryListing {
	t.Helper()
	status, envelope := f.client.request(t, http.MethodGet, fmt.Sprintf("/api/v1/storages/%d/directory", f.storage.ID), nil, false)
	var listing services.DirectoryListing
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &listing) != nil || listing.CurrentSelectionToken == "" {
		t.Fatalf("Quark picker status=%d message=%s", status, envelope.Message)
	}
	return listing
}

func (f *librarySelectionFixture) payload(token string) map[string]any {
	return map[string]any{"name": "夸克媒体库", "storage_id": f.storage.ID, "profile_id": f.profile, "relative_root_token": token, "enabled": false, "recursive": true}
}

func TestQuarkMediaLibrarySelectionCreate(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprintf("child=%t", child), func(t *testing.T) {
			f := newLibrarySelectionFixture(t)
			listing := f.browse(t)
			token, relative, id := listing.CurrentSelectionToken, "/", libraryQuarkID("media")
			if child {
				token, relative, id = listing.Items[0].SelectionToken, "/电影", libraryQuarkID("movies")
			}
			status, envelope := f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(token), true)
			if status != http.StatusCreated {
				t.Fatalf("valid Quark picker token: status=%d message=%s data=%s", status, envelope.Message, envelope.Data)
			}
			var library models.MediaLibrary
			if err := f.client.db.First(&library).Error; err != nil {
				t.Fatal(err)
			}
			if library.RelativeRoot != relative || library.ProviderRootID != id {
				t.Fatalf("lost authoritative root: relative=%q id=%q", library.RelativeRoot, library.ProviderRootID)
			}
			if strings.Contains(string(envelope.Data), id) || strings.Contains(string(envelope.Data), libraryFixtureAccount) {
				t.Fatal("private provider identity leaked")
			}
		})
	}
}

func TestQuarkMediaLibrarySelectionUnchangedEdit(t *testing.T) {
	f := newLibrarySelectionFixture(t)
	library := models.MediaLibrary{Name: "原媒体库", NameNormalized: "原媒体库", StorageID: f.storage.ID, ProfileID: f.profile, RelativeRoot: "/电影", ProviderRootID: libraryQuarkID("movies"), Enabled: false}
	if err := f.client.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	payload := f.payload("")
	payload["name"], payload["relative_root"] = "重命名媒体库", "/电影"
	status, envelope := f.client.request(t, http.MethodPut, fmt.Sprintf("/api/v1/media-libraries/%d", library.ID), payload, true)
	if status != http.StatusOK {
		t.Fatalf("unchanged Quark source: status=%d message=%s data=%s", status, envelope.Message, envelope.Data)
	}
	var saved models.MediaLibrary
	if err := f.client.db.First(&saved, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ProviderRootID != library.ProviderRootID || saved.RelativeRoot != library.RelativeRoot {
		t.Fatal("unchanged source identity was discarded")
	}
}

func TestQuarkMediaLibrarySelectionFences(t *testing.T) {
	f := newLibrarySelectionFixture(t)
	listing := f.browse(t)
	const purpose = "provider-directory-token:v1"
	raw, err := f.client.credentials.Decrypt(purpose, listing.CurrentSelectionToken)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		field string
		value any
	}{
		{"expired", "expires_at", time.Now().Add(-time.Minute).Unix()},
		{"actor", "actor_id", uint(999)},
		{"connection", "connection_id", *f.storage.ConnectionID + 1},
		{"storage", "storage_id", f.storage.ID + 1},
		{"storage-root", "storage_root_id", libraryQuarkID("movies")},
		{"purpose", "purpose", "browse"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Re-seal one claim from an authentic GET token. This tests the
			// route's full provider validation, not a failed local decoder.
			var claims map[string]any
			if err := json.Unmarshal([]byte(raw), &claims); err != nil {
				t.Fatal(err)
			}
			claims[test.field] = test.value
			changed, _ := json.Marshal(claims)
			token, err := f.client.credentials.Encrypt(purpose, string(changed))
			if err != nil {
				t.Fatal(err)
			}
			status, envelope := f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(token), true)
			code := services.CodeDirectoryTokenInvalid
			if test.name == "expired" {
				code = services.CodeDirectoryTokenExpired
			}
			if status != http.StatusBadRequest || !strings.Contains(string(envelope.Data), code) {
				t.Fatalf("fence status=%d data=%s", status, envelope.Data)
			}
		})
	}
	for _, token := range []string{"", "invalid", listing.CurrentToken} {
		status, _ := f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(token), true)
		if status != http.StatusBadRequest {
			t.Fatalf("manual/navigation token accepted: %d", status)
		}
	}
	var count int64
	if err := f.client.db.Model(&models.MediaLibrary{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rejected requests wrote libraries: %d %v", count, err)
	}
	// A valid old token must also fail after the saved Storage root changes.
	if err := f.client.db.Model(&models.Storage{}).Where("id = ?", f.storage.ID).Update("root_path", libraryQuarkID("movies")).Error; err != nil {
		t.Fatal(err)
	}
	status, envelope := f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(listing.CurrentSelectionToken), true)
	if status != http.StatusBadRequest || !strings.Contains(string(envelope.Data), services.CodeDirectoryTokenInvalid) {
		t.Fatalf("changed saved root accepted old token: %d %s", status, envelope.Data)
	}
	if err := f.client.db.Model(&models.Storage{}).Where("id = ?", f.storage.ID).Update("root_path", f.storage.RootPath).Error; err != nil {
		t.Fatal(err)
	}
	// Same-account access/refresh rotation must preserve a valid selection.
	f.bundle.Revision++
	f.bundle.TV.Generation++
	f.bundle.TV.AccessToken = "rotated-fixture-access"
	f.saveBundle(t, *f.storage.ConnectionID)
	status, envelope = f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(listing.CurrentSelectionToken), true)
	if status != http.StatusCreated {
		t.Fatalf("same identity refresh invalidated selection: %d %s", status, envelope.Message)
	}
	// A connection configuration change invalidates the former selection.
	if err := f.client.db.Model(&models.Connection{}).Where("id = ?", *f.storage.ConnectionID).Update("revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	status, envelope = f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(listing.CurrentSelectionToken), true)
	if status != http.StatusBadRequest || !strings.Contains(string(envelope.Data), services.CodeDirectoryTokenInvalid) {
		t.Fatalf("configuration fence status=%d data=%s", status, envelope.Data)
	}
	fresh := f.browse(t)
	f.bundle.TV.AccountID = "replacement-private-account"
	f.saveBundle(t, *f.storage.ConnectionID)
	status, envelope = f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(fresh.CurrentSelectionToken), true)
	if status != http.StatusBadRequest || !strings.Contains(string(envelope.Data), services.CodeDirectoryTokenInvalid) {
		t.Fatalf("credential identity fence status=%d data=%s", status, envelope.Data)
	}
}

func TestQuarkMediaLibrarySelectionSourceEdit(t *testing.T) {
	f := newLibrarySelectionFixture(t)
	listing := f.browse(t)
	status, envelope := f.client.request(t, http.MethodPost, "/api/v1/media-libraries", f.payload(listing.CurrentSelectionToken), true)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, envelope.Message)
	}
	var library models.MediaLibrary
	if err := f.client.db.First(&library).Error; err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/media-libraries/%d", library.ID)
	var initialHead models.CatalogHead
	if err := f.client.db.First(&initialHead, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"", "/"} {
		payload := f.payload("")
		payload["relative_root"] = relative
		status, envelope = f.client.request(t, http.MethodPut, path, payload, true)
		if status != http.StatusOK {
			t.Fatalf("unchanged edit: %d %s", status, envelope.Message)
		}
	}
	var unchangedHead models.CatalogHead
	if err := f.client.db.First(&unchangedHead, "library_id = ?", library.ID).Error; err != nil || unchangedHead.SourceEpoch != initialHead.SourceEpoch {
		t.Fatalf("ordinary edit changed source epoch: %v", err)
	}
	var otherStorageID uint
	for _, change := range []map[string]any{{"relative_root": "/电影"}, {"storage_id": f.storage.ID + 1}} {
		if _, ok := change["storage_id"]; ok {
			other := f.storage
			other.ID = 0
			other.Name = "另一个来源"
			other.NameNormalized = other.Name
			other.RootPathNormalized = "quark-other"
			if err := f.client.db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			change["storage_id"] = other.ID
			otherStorageID = other.ID
		}
		payload := f.payload("")
		for key, value := range change {
			payload[key] = value
		}
		status, envelope = f.client.request(t, http.MethodPut, path, payload, true)
		if status != http.StatusBadRequest {
			t.Fatalf("tokenless source change accepted: %d %s", status, envelope.Message)
		}
	}
	payload := f.payload(listing.Items[0].SelectionToken)
	status, envelope = f.client.request(t, http.MethodPut, path, payload, true)
	if status != http.StatusOK {
		t.Fatalf("fresh child selection: %d %s", status, envelope.Message)
	}
	var saved models.MediaLibrary
	if err := f.client.db.First(&saved, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ProviderRootID != libraryQuarkID("movies") || saved.RelativeRoot != "/电影" {
		t.Fatal("source edit did not save the selected identity/path pair")
	}
	f.storage.ID = otherStorageID
	newSource := f.browse(t)
	status, envelope = f.client.request(t, http.MethodPut, path, f.payload(newSource.CurrentSelectionToken), true)
	if status != http.StatusOK {
		t.Fatalf("fresh Storage selection: %d %s", status, envelope.Message)
	}
	if err := f.client.db.First(&saved, library.ID).Error; err != nil || saved.StorageID != otherStorageID || saved.RelativeRoot != "/" || saved.ProviderRootID != libraryQuarkID("media") {
		t.Fatalf("new Storage source identity was not saved: %v", err)
	}
	var changedHead models.CatalogHead
	if err := f.client.db.First(&changedHead, "library_id = ?", library.ID).Error; err != nil || changedHead.SourceEpoch <= initialHead.SourceEpoch {
		t.Fatalf("source replacement did not advance its epoch: %v", err)
	}
}

func TestQuarkMediaLibrarySelectionRegistersInitialObservation(t *testing.T) {
	f := newLibrarySelectionFixture(t)
	observer := services.NewProviderObservationService(f.client.db)
	f.client.libraries.SetProviderObservationService(observer)
	listing := f.browse(t)
	payload := f.payload(listing.CurrentSelectionToken)
	payload["enabled"] = true
	status, envelope := f.client.request(t, http.MethodPost, "/api/v1/media-libraries", payload, true)
	if status != http.StatusCreated {
		t.Fatalf("enabled create: %d %s", status, envelope.Message)
	}
	waitLibraryCondition(t, func() bool {
		var scope models.ProviderObservationScope
		if err := f.client.db.Where("kind = ? AND enabled = ?", "library", true).Find(&scope).Error; err != nil || scope.RootID != libraryQuarkID("media") || scope.RequestedScanRunID == 0 {
			return false
		}
		var run models.MediaLibraryScanRun
		return f.client.db.First(&run, scope.RequestedScanRunID).Error == nil && run.LibraryID == scope.LibraryID && run.Kind == "provider_observation" && run.Status == "queued"
	})
}

func TestPan115MediaLibrarySelectionRegression(t *testing.T) {
	client := newTestClient(t)
	client.setup(t)
	status, envelope := client.request(t, http.MethodPost, "/api/v1/connections", map[string]any{"name": "115", "provider": "pan115", "cookie": "UID=fixture_A1; CID=fixture-cid; SEID=fixture-secret", "enabled": true}, true)
	var connection struct {
		ID uint `json:"id"`
	}
	if status != http.StatusCreated || json.Unmarshal(envelope.Data, &connection) != nil {
		t.Fatalf("connection: %d %s", status, envelope.Message)
	}
	storage := models.Storage{Name: "115媒体", NameNormalized: "115媒体", Type: models.StorageTypePan115, RootPath: "root-1", RootDisplayPath: "/媒体", RootPathNormalized: "115-fixture", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{}`}
	if err := client.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := client.db.First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	status, envelope = client.request(t, http.MethodGet, fmt.Sprintf("/api/v1/storages/%d/directory", storage.ID), nil, false)
	var listing services.DirectoryListing
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &listing) != nil {
		t.Fatalf("115 picker: %d %s", status, envelope.Message)
	}
	payload := map[string]any{"name": "115媒体库", "storage_id": storage.ID, "profile_id": profile.ID, "relative_root_token": listing.CurrentSelectionToken, "enabled": false}
	status, envelope = client.request(t, http.MethodPost, "/api/v1/media-libraries", payload, true)
	if status != http.StatusCreated {
		t.Fatalf("115 create: %d %s", status, envelope.Message)
	}
	var library models.MediaLibrary
	if err := client.db.First(&library).Error; err != nil {
		t.Fatal(err)
	}
	delete(payload, "relative_root_token")
	payload["relative_root"] = "/"
	payload["name"] = "115重命名"
	status, envelope = client.request(t, http.MethodPut, fmt.Sprintf("/api/v1/media-libraries/%d", library.ID), payload, true)
	if status != http.StatusOK {
		t.Fatalf("115 unchanged edit: %d %s", status, envelope.Message)
	}
}

// Used below to bound asynchronous supervisor registration without sleeping for
// the production observer's scheduling interval.
func waitLibraryCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("library supervisor did not register initial synchronization")
}
