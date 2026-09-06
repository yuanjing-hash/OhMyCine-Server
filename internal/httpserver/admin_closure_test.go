package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestAdminCollectionsAndNotificationsSessionCSRFAndOwnership(t *testing.T) {
	c := newTestClient(t)
	c.setup(t)
	var owner models.User
	var viewerRole models.Role
	if err := c.db.First(&owner, "username_normalized = ?", "owner").Error; err != nil {
		t.Fatal(err)
	}
	if err := c.db.First(&viewerRole, "code = ?", "viewer").Error; err != nil {
		t.Fatal(err)
	}
	status, envelope := c.request(t, http.MethodPost, "/api/v1/users", map[string]any{"username": "other", "display_name": "Other", "password": "other-strong-password", "role_ids": []uint{viewerRole.ID}}, true)
	if status != http.StatusCreated {
		t.Fatal(status, envelope.Message)
	}
	other := newTestClientWithRouter(c.router)
	var otherUser models.User
	if err := c.db.First(&otherUser, "username_normalized = ?", "other").Error; err != nil {
		t.Fatal(err)
	}
	for _, permission := range []string{authz.PermissionMediaLibrariesRead, authz.PermissionJobsReadOwn} {
		if err := c.db.Create(&models.UserAuthorizationRule{UserID: otherUser.ID, PermissionCode: permission, Effect: models.AuthorizationEffectAllow, CreatedBy: owner.ID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	other.login(t, "other", "other-strong-password")
	base := "/api/v1/media-libraries/collections"
	input := map[string]any{"name": "Private collection", "kind": "collection"}
	if status, _ := c.request(t, http.MethodPost, base, input, false); status != http.StatusForbidden {
		t.Fatal("missing CSRF", status)
	}
	status, envelope = c.request(t, http.MethodPost, base, input, true)
	if status != http.StatusCreated {
		t.Fatal(status, envelope.Message)
	}
	var collection models.PlayerMediaCollection
	if err := json.Unmarshal(envelope.Data, &collection); err != nil || collection.ID == "" {
		t.Fatal(string(envelope.Data), err)
	}
	path := base + "/" + collection.ID
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		if status, _ := c.request(t, method, path, map[string]any{"name": "New", "revision": 1}, false); status != http.StatusForbidden {
			t.Fatal("mutation missing CSRF", method, status)
		}
		if status, _ := other.request(t, method, path, map[string]any{"name": "Foreign", "revision": 1}, true); status != http.StatusForbidden {
			t.Fatal("foreign mutation", method, status)
		}
	}
	if status, _ := other.request(t, http.MethodGet, path+"/items?page=1&page_size=24", nil, false); status != http.StatusNotFound {
		t.Fatal("foreign read", status)
	}
	if status, _ := c.request(t, http.MethodPatch, path, map[string]any{"name": "Renamed", "revision": 1}, true); status != http.StatusOK {
		t.Fatal("rename", status)
	}
	if status, _ := c.request(t, http.MethodPatch, path, map[string]any{"name": "Stale", "revision": 1}, true); status != http.StatusConflict {
		t.Fatal("stale revision", status)
	}
	job, err := c.queue.Enqueue(services.EnqueueJobInput{OwnerID: owner.ID, JobType: "fake", Priority: 10, DisplayName: "Private failure", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.db.Model(&models.Job{}).Where("id = ?", job.ID).Update("status", "failed").Error; err != nil {
		t.Fatal(err)
	}
	event := models.JobStatusEvent{JobID: job.ID, EventType: "failed", ToStatus: "failed", CreatedAt: time.Now().UTC()}
	if err := c.db.Create(&event).Error; err != nil {
		t.Fatal(err)
	}
	status, envelope = c.request(t, http.MethodGet, "/api/v1/notifications", nil, false)
	var page services.BrowserMediaPage[services.UserNotification]
	if status != http.StatusOK || json.Unmarshal(envelope.Data, &page) != nil || len(page.List) != 1 || page.List[0].Read {
		t.Fatal(status, string(envelope.Data))
	}
	read := "/api/v1/notifications/" + job.ID + "/read"
	ack := map[string]any{"occurrence": event.ID}
	if status, _ := c.request(t, http.MethodPost, read, ack, false); status != http.StatusForbidden {
		t.Fatal("ack CSRF", status)
	}
	if status, _ := other.request(t, http.MethodPost, read, ack, true); status != http.StatusNotFound && status != http.StatusForbidden {
		t.Fatal("foreign ack", status)
	}
	if status, _ := c.request(t, http.MethodPost, read, ack, true); status != http.StatusOK {
		t.Fatal("ack", status)
	}
	var persisted models.Job
	if err := c.db.First(&persisted, "id = ?", job.ID).Error; err != nil || persisted.Status != "failed" {
		t.Fatal("ack changed job", err)
	}
	for _, permission := range []string{authz.PermissionJobsReadAll, authz.PermissionJobsReadOwn} {
		if err := c.db.Create(&models.UserAuthorizationRule{UserID: owner.ID, PermissionCode: permission, Effect: models.AuthorizationEffectDeny, CreatedBy: owner.ID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if status, _ := c.request(t, http.MethodGet, "/api/v1/notifications", nil, false); status != http.StatusForbidden {
		t.Fatal("revoked notifications", status)
	}
	if status, _ := c.request(t, http.MethodPost, read, ack, true); status != http.StatusForbidden {
		t.Fatal("revoked ack", status)
	}
}
