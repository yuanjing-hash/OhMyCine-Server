package httpserver

import (
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"net/http"
	"testing"
)

func TestSubscriptionCalendarRoutesUseSessionNoStoreAndCSRF(t *testing.T) {
	client := newTestClient(t)
	const path = "/api/v1/follows/calendar?start=2026-10-01&end=2026-11-01&timezone=Asia%2FShanghai"
	status, _ := client.request(t, http.MethodGet, path, nil, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous=%d", status)
	}
	client.setup(t)
	status, envelope := client.request(t, http.MethodGet, path, nil, true)
	if status != http.StatusOK || client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET status=%d body=%s", status, envelope.Data)
	}
	var page services.CalendarPage
	if err := json.Unmarshal(envelope.Data, &page); err != nil || len(page.Events) != 0 || page.Scope != "all" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	status, _ = client.request(t, http.MethodPost, "/api/v1/follows/calendar/refresh", map[string]any{}, false)
	if status != http.StatusForbidden {
		t.Fatalf("missing CSRF=%d", status)
	}
	status, _ = client.request(t, http.MethodPost, "/api/v1/follows/calendar/refresh", map[string]any{}, true)
	if status != http.StatusAccepted {
		t.Fatalf("refresh=%d", status)
	}
	status, _ = client.request(t, http.MethodGet, path+"&page_size=invalid", nil, true)
	if status != http.StatusBadRequest {
		t.Fatalf("bad page=%d", status)
	}
	status, _ = client.request(t, http.MethodGet, "/api/v1/follows/calendar?start=2026-10-01&end=2027-01-01", nil, true)
	if status != http.StatusBadRequest {
		t.Fatalf("bad range=%d", status)
	}
}
