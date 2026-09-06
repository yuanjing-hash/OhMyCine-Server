package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestMediaLibraryRetirementHTTPAcceptedAndPolling(t *testing.T) {
	c := newTestClient(t)
	c.setup(t)
	c.libraries.SetQueueService(c.queue)
	c.libraries.SetRetirementPhysicalGuard(services.AssertCatalogPhysicalDrainedTx)
	var profile models.MediaClassificationProfile
	if err := c.db.Where("code=?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "Retirement", NameNormalized: "retirement", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: "retirement", Enabled: true, Capabilities: `{}`}
	if err := c.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Retirement", NameNormalized: "retirement", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`, Status: models.MediaLibraryStatusListening}
	if err := c.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	// A converter has claimed this head; API acceptance does not require reading its incomplete facts.
	head := models.CatalogHead{LibraryID: library.ID, Mode: "converting", Revision: 1, SourceEpoch: 1, SourceFingerprint: "private-source", ConfigFingerprint: "private-config", UpdatedAt: time.Now().UTC()}
	if err := c.db.Create(&head).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/media-libraries/" + uintString(library.ID)
	var first services.MediaLibraryDeletionResult
	for i := 0; i < 2; i++ {
		status, envelope := c.request(t, http.MethodDelete, path, map[string]any{}, true)
		var result services.MediaLibraryDeletionResult
		if err := json.Unmarshal(envelope.Data, &result); err != nil || status != http.StatusAccepted || result.Deleted || result.Status != "deleting" || result.JobID == "" {
			t.Fatalf("accepted %d %s %v", status, envelope.Data, err)
		}
		if c.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatal("DELETE retirement cacheable")
		}
		if i == 0 {
			first = result
		} else if result != first {
			t.Fatal("duplicate request created another job")
		}
	}
	status, envelope := c.request(t, http.MethodGet, path, nil, false)
	var detail services.MediaLibraryDetail
	if err := json.Unmarshal(envelope.Data, &detail); err != nil || status != http.StatusOK || detail.Retirement == nil || detail.Enabled || detail.Retirement.JobID != first.JobID {
		t.Fatalf("polling %d %s %v", status, envelope.Data, err)
	}
	if c.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatal("retirement polling cacheable")
	}
	status, _ = c.request(t, http.MethodGet, path+"/catalog", nil, false)
	if status == http.StatusOK {
		t.Fatal("retiring library still exposes catalog")
	}
}
