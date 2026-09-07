package httpserver

import (
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestJobDetailsNoStoreAndHistoryPaging(t *testing.T) {
	owner := newTestClient(t)
	setup := owner.setup(t)
	id := uint(setup["user"].(map[string]any)["id"].(float64))
	job, err := owner.queue.Enqueue(services.EnqueueJobInput{OwnerID: id, JobType: "fake", DisplayName: "History", Payload: map[string]any{"step": 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"timeline?page=1&page_size=1", "attempts?page=1&page_size=1"} {
		status, _ := owner.request(t, http.MethodGet, "/api/v1/jobs/"+job.ID+"/"+suffix, nil, false)
		if status != 200 || owner.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatalf("status=%d headers=%v", status, owner.lastHeader)
		}
	}
	status, _ := owner.request(t, http.MethodGet, "/api/v1/jobs/"+job.ID+"/repair-details", nil, false)
	if status != 404 || owner.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v", status, owner.lastHeader)
	}
	status, _ = owner.request(t, http.MethodGet, "/api/v1/jobs/"+job.ID+"/timeline?page_size=201", nil, false)
	if status != 400 {
		t.Fatal(status)
	}
	anonymous := newTestClientWithRouter(owner.router)
	status, _ = anonymous.request(t, http.MethodGet, "/api/v1/jobs/"+job.ID+"/repair-details", nil, false)
	if status != http.StatusUnauthorized || anonymous.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", status, anonymous.lastHeader.Get("Cache-Control"))
	}
}
