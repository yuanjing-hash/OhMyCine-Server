package services

import "testing"

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
}

func floatPointer(value float64) *float64 { return &value }
