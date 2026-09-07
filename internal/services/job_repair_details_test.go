package services

import (
	"context"
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"strings"
	"testing"
	"time"
)

func TestJobRepairDetailsPagingAuthorizationAndRedaction(t *testing.T) {
	s, repair, plan, _, _ := catalogStructureRepairFixture(t)
	q := NewQueueService(s.db, NewAuditService(s.db))
	job, err := q.Enqueue(EnqueueJobInput{OwnerID: repair.OwnerID, JobType: "media_library_repair", DisplayName: "整理", Payload: map[string]any{"repair_id": repair.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&repair).Updates(map[string]any{"job_id": job.ID}).Error; err != nil {
		t.Fatal(err)
	}
	repair.JobID = &job.ID
	if _, err := s.ensureStructureRepairItems(context.Background(), repair, plan, "local", nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 53; i++ {
		row := models.MediaLibraryStructureRepairItem{RepairID: repair.ID, Ordinal: i, Action: "move", Kind: "video", SourceRelative: "Show/E01.mkv", Status: "failed", CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := s.db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	actor := Actor{Permissions: map[string]struct{}{authz.PermissionJobsReadAll: {}, authz.PermissionMediaLibrariesRead: {}}}
	page, err := q.RepairDetails(actor, job.ID, "", 2, 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 53 || len(page.List) != 3 || page.List[0].Ordinal != 50 {
		t.Fatalf("page=%+v", page)
	}
	failed, err := q.RepairDetails(actor, job.ID, "failed", 1, 50)
	if err != nil || failed.Total != 52 || failed.Counts["pending"] != 1 {
		t.Fatalf("filtered=%+v error=%v", failed, err)
	}
	all, err := q.RepairDetails(actor, job.ID, "all", 1, 100)
	if err != nil || all.Total != 53 || len(all.List) != 53 {
		t.Fatalf("all=%+v error=%v", all, err)
	}
	delete(actor.Permissions, authz.PermissionMediaLibrariesRead)
	if _, err := q.RepairDetails(actor, job.ID, "", 1, 50); ErrorCode(err) != CodePermissionDenied {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(page)
	for _, private := range []string{"provider_id", "plan_json", "repair_id", "lease"} {
		if strings.Contains(string(raw), private) {
			t.Fatal(private)
		}
	}
}
func TestJobHistoryPagesDoNotDiscardOlderEvents(t *testing.T) {
	q, actor, _ := queueFixture(t)
	job := enqueueFake(t, q, actor, "history", "")
	for i := 0; i < 205; i++ {
		event := models.JobStatusEvent{JobID: job.ID, EventType: "progress", CreatedAt: time.Now()}
		if err := q.db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	page, err := q.TimelinePage(actor, job.ID, 5, 50)
	if err != nil || page.Total != 206 || len(page.List) != 6 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
func TestJobRepairDetailRejectsUnsafeStoredFields(t *testing.T) {
	for _, value := range []string{"/private/root/a.mkv", `C:\private\a.mkv`, "../private/a.mkv", "https://example.invalid/cookie"} {
		if jobRepairRelativePath(value) != "" {
			t.Fatal("unsafe path accepted")
		}
	}
	code, message := jobRepairSafeError("cookie=synthetic")
	if strings.Contains(code+message, "synthetic") || code != CodeMediaLibraryStructureApplyFailed {
		t.Fatal("unsafe error exposed")
	}
}
