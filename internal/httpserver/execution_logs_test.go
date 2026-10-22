package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestExecutionLogRoutesNoStoreAndScopedEnvelope(t *testing.T) {
	client := newTestClient(t)
	for _, path := range []string{"/api/v1/jobs/unknown/execution-logs", "/api/v1/follows/unknown/runs/unknown/execution-logs"} {
		status, _ := client.request(t, http.MethodGet, path, nil, false)
		if status != http.StatusUnauthorized || client.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatalf("anonymous detail status=%d headers=%v", status, client.lastHeader)
		}
	}
	client.setup(t)
	var owner models.User
	if err := client.db.Where("is_owner = ?", true).First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	job, err := client.queue.Enqueue(services.EnqueueJobInput{OwnerID: owner.ID, JobType: "fake", DisplayName: "真实任务摘要", ResourceKey: "test", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	status, response := client.request(t, http.MethodGet, "/api/v1/jobs/"+job.ID+"/execution-logs", nil, false)
	var page services.ExecutionLogPage
	if err := json.Unmarshal(response.Data, &page); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || page.Summary == nil || page.Summary.Title != "真实任务摘要" || page.Availability != "unavailable" || page.SnapshotTo.IsZero() || client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("scoped response status=%d page=%+v headers=%v", status, page, client.lastHeader)
	}
	for _, query := range []string{"?attempt=0", "?attempt=other", "?limit=201", "?snapshot_to=invalid", "?level=fatal"} {
		status, _ := client.request(t, http.MethodGet, "/api/v1/jobs/"+job.ID+"/execution-logs"+query, nil, false)
		if status != http.StatusBadRequest || client.lastHeader.Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid filter %s status=%d headers=%v", query, status, client.lastHeader)
		}
	}
}

func TestRuntimeLogSnapshotPaginationWithoutExplicitRange(t *testing.T) {
	client := newTestClient(t)
	client.setup(t)
	logger := client.logManager.Logger("snapshot", "http")
	for _, message := range []string{"first snapshot event", "second snapshot event", "third snapshot event"} {
		logger.Info().Msg(message)
	}
	status, response := client.request(t, http.MethodGet, "/api/v1/runtime-logs?module=snapshot&limit=1", nil, false)
	var first logging.QueryResult
	if err := json.Unmarshal(response.Data, &first); err != nil || status != http.StatusOK || len(first.List) != 1 || first.NextCursor == "" {
		t.Fatalf("initial snapshot: status=%d page=%+v err=%v", status, first, err)
	}
	query := url.Values{"module": {"snapshot"}, "limit": {"1"}, "snapshot_to": {first.SnapshotTo.Format(time.RFC3339Nano)}, "cursor": {first.NextCursor}}
	status, response = client.request(t, http.MethodGet, "/api/v1/runtime-logs?"+query.Encode(), nil, false)
	var second logging.QueryResult
	if err := json.Unmarshal(response.Data, &second); err != nil || status != http.StatusOK || len(second.List) != 1 || second.List[0].Message == first.List[0].Message || !second.SnapshotTo.Equal(first.SnapshotTo) || client.lastHeader.Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot-only pagination changed range: status=%d page=%+v err=%v", status, second, err)
	}
	file, err := os.OpenFile(filepath.Join(client.logManager.Directory(), "runtime.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("{malformed history}\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-logs/export", strings.NewReader(`{"modules":["snapshot"]}`))
	request.AddCookie(client.cookie)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:3000")
	request.Header.Set("X-CSRF-Token", client.csrf)
	recorder := httptest.NewRecorder()
	client.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("X-Log-Partial") != "true" || recorder.Header().Get("X-Log-Entry-Count") != "3" || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("partial export coverage not surfaced: status=%d headers=%v", recorder.Code, recorder.Header())
	}
}
