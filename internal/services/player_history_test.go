package services

import (
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestPlayerHistorySyncIsUserScopedAndRejectsOlderProgress(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	service := NewPlayerHistoryService(queue.db)
	base := PlayerHistoryChange{SyncKey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceKind: "server", SourceLocator: "http://127.0.0.1:3000", SourceID: "server-a", MediaIdentity: "entry|1|work|2", Title: "Movie", Position: 120, Duration: floatPointer(1000), UpdatedAt: 2_000}
	first, err := service.Sync(actor, 0, []PlayerHistoryChange{base})
	if err != nil || first.Cursor == 0 || len(first.Changes) != 1 || first.Changes[0].Position != 120 {
		t.Fatalf("first sync=%+v err=%v", first, err)
	}
	older := base
	older.Position, older.UpdatedAt = 30, 1_000
	second, err := service.Sync(actor, first.Cursor, []PlayerHistoryChange{older})
	if err != nil || len(second.Changes) != 0 || second.Cursor != first.Cursor {
		t.Fatalf("older sync=%+v err=%v", second, err)
	}
	tie := base
	tie.Completed = true
	tie.Position = 1000
	third, err := service.Sync(actor, first.Cursor, []PlayerHistoryChange{tie})
	if err != nil || len(third.Changes) != 1 || !third.Changes[0].Completed || third.Changes[0].Position != 1000 {
		t.Fatalf("completion tie=%+v err=%v", third, err)
	}
	fourth, err := service.Sync(actor, third.Cursor, []PlayerHistoryChange{tie})
	if err != nil || len(fourth.Changes) != 0 || fourth.Cursor != third.Cursor {
		t.Fatalf("idempotent retry=%+v err=%v", fourth, err)
	}

	page, err := service.List(actor, 1, 24, "server")
	if err != nil || page.Total != 1 || len(page.List) != 1 || !page.List[0].Completed {
		t.Fatalf("history page=%+v err=%v", page, err)
	}
	foreign := Actor{User: models.User{ID: actor.User.ID + 99}}
	foreignPage, err := service.List(foreign, 1, 24, "server")
	if err != nil || foreignPage.Total != 0 || len(foreignPage.List) != 0 {
		t.Fatalf("foreign history page=%+v err=%v", foreignPage, err)
	}
}

func floatPointer(value float64) *float64 { return &value }
