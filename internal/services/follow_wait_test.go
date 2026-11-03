package services

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestFollowModernContentRetrySurvivesPersistenceAndAuthorizedReads(t *testing.T) {
	f, driver := newFollowPreviewFixture(t)
	a := f.worker.sites.adapters["pansou_tg"].(*followSharesAdapter)
	a.items = a.items[1:]
	deadline := f.follows.now().Add(5 * time.Minute)
	driver.readErr = cloud.WithRetryNotBefore(cloud.Error(cloud.CodeRateLimited, true, errors.New("Cookie=private; https://secret.example/path")), deadline, cloud.RetryReasonProviderCooldown)
	run := f.run(t)
	if run.ErrorCode != "follow_submit_failed" || run.RetryNotBefore == nil || !run.RetryNotBefore.Equal(deadline) {
		t.Fatalf("modern content deadline lost: %+v", run)
	}
	reads := len(driver.reads)
	// Close/reopen the actual disposable SQLite DB, rather than relying on an
	// in-memory error or the old service/driver instance to supply the deadline.
	var files []struct {
		Name string
		File string
	}
	if err := f.follows.db.Raw("PRAGMA database_list").Scan(&files).Error; err != nil {
		t.Fatal(err)
	}
	path := ""
	for _, file := range files {
		if file.Name == "main" {
			path = file.File
		}
	}
	oldSQL, _ := f.follows.db.DB()
	if err := oldSQL.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	newSQL, _ := reopened.DB()
	defer newSQL.Close()
	s := NewFollowService(reopened, nil, nil, nil, nil)
	s.now = func() time.Time { return deadline.Add(-time.Minute) }
	page, err := s.List(f.actor, 1, 100, "")
	if err != nil || len(page.List) != 1 || page.List[0].Wait == nil {
		t.Fatalf("list=%+v err=%v", page, err)
	}
	item := page.List[0]
	if !item.Wait.NotBefore.Equal(deadline) || !item.Wait.RecordedAt.Equal(*run.FinishedAt) || item.NextRunAt == nil || !item.NextRunAt.Equal(run.FinishedAt.Add(time.Hour)) {
		t.Fatalf("wait changed schedule or observation: %+v", item)
	}
	history, err := s.Runs(f.actor, item.ID)
	if err != nil || len(history) != 1 || history[0].Wait == nil {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	encoded, _ := json.Marshal([]any{page, history})
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "secret.example") || reads != len(driver.reads) {
		t.Fatal("list leaked cause or probed provider")
	}
	// Current waits expire by policy identity, not by erasing historical facts.
	for _, updates := range []map[string]any{{"revision": 2}, {"revision": 1, "lifecycle_revision": 2}, {"lifecycle_revision": 1, "status": models.FollowStatusPaused}, {"status": models.FollowStatusActive, "last_error_code": "", "last_error_message": ""}} {
		if err := s.db.Model(&models.FollowSubscription{}).Where("id=?", item.ID).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
		current, err := s.Get(f.actor, item.ID)
		if err != nil || current.Wait != nil {
			t.Fatalf("stale current wait: %+v err=%v", current, err)
		}
	}
	history, err = s.Runs(f.actor, item.ID)
	if err != nil || history[0].Wait == nil {
		t.Fatal("history was rewritten")
	}
	observed := []followRetryObservation{}
	observeFollowRetry(&observed, sharePreviewProviderError(driver.readErr), followCandidate{Season: 1, Episodes: []int{13}})
	applyFollowRetryObservations(&run, observed, map[[2]int]struct{}{{1, 15}: {}})
	if run.RetryNotBefore != nil {
		t.Fatal("unrelated candidate left a countdown")
	}
}
