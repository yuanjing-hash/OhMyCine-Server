package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
)

func TestProviderEventReviewRouteRequiresLibraryRead(t *testing.T) {
	c := newTestClient(t)
	status, _ := c.request(t, http.MethodGet, "/api/v1/media-libraries/1/provider-events", nil, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", status)
	}
	c.setup(t)
	status, envelope := c.request(t, http.MethodPost, "/api/v1/roles", map[string]any{"code": "event_discovery_only", "name": "Discovery only", "permissions": []string{authz.PermissionDiscoveryRead}}, true)
	if status != http.StatusCreated {
		t.Fatal(status)
	}
	var role struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(envelope.Data, &role); err != nil {
		t.Fatal(err)
	}
	status, _ = c.request(t, http.MethodPost, "/api/v1/users", map[string]any{"username": "event-reader", "display_name": "Event Reader", "password": "event-reader-strong-password", "role_ids": []uint{role.ID}}, true)
	if status != http.StatusCreated {
		t.Fatal(status)
	}
	reader := newTestClientWithRouter(c.router)
	reader.login(t, "event-reader", "event-reader-strong-password")
	status, _ = reader.request(t, http.MethodGet, "/api/v1/media-libraries/1/provider-events", nil, false)
	if status != http.StatusForbidden {
		t.Fatalf("read permission not enforced: %d", status)
	}
	status, _ = c.request(t, http.MethodGet, "/api/v1/media-libraries/1/provider-events?page=0", nil, false)
	if status != http.StatusBadRequest || c.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("bounded no-store route: %d %v", status, c.lastHeader)
	}
}
