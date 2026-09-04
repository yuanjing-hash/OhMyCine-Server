package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type fakeChangeSource struct {
	page   cloud.ChangePage
	seen   cloud.ChangeCursor
	limits []int
}

func (f *fakeChangeSource) Changes(_ context.Context, cursor cloud.ChangeCursor, limit int) (cloud.ChangePage, error) {
	f.seen = cursor
	f.limits = append(f.limits, limit)
	return f.page, nil
}

type fakeEventNotifier struct {
	err    error
	events []models.ProviderEvent
}

func (f *fakeEventNotifier) ProviderEventsChanged(_ context.Context, _ uint, events []models.ProviderEvent) error {
	f.events = append(f.events, events...)
	return f.err
}

func TestProviderEventsPersistBeforeCursorAndDeduplicate(t *testing.T) {
	db, _, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := connections.Create(actor, ConnectionInput{Name: "event account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	source := &fakeChangeSource{page: cloud.ChangePage{Events: []cloud.ChangeEvent{
		{ID: "10", Time: timestamp, Kind: cloud.ChangeRenamed, ItemID: "file-1", ParentID: "root", Name: "New.mkv"},
		{ID: "9", Time: timestamp, Kind: cloud.ChangeCreated, ItemID: "file-1", ParentID: "root", Name: "Old.mkv"},
	}, NextCursor: cloud.ChangeCursor{Time: timestamp, ID: "10"}}}
	service := NewProviderEventService(db, nil)
	inserted, more, err := service.IngestOnce(context.Background(), connection.ID, source)
	if err != nil || more || inserted != 2 {
		t.Fatalf("first ingest = inserted %d more %v err %v", inserted, more, err)
	}
	inserted, _, err = service.IngestOnce(context.Background(), connection.ID, source)
	if err != nil || inserted != 0 {
		t.Fatalf("replay ingest = inserted %d err %v", inserted, err)
	}
	if !source.seen.Time.Equal(timestamp) || source.seen.ID != "10" || len(source.limits) != 2 || source.limits[0] != providerEventBatchLimit {
		t.Fatalf("unexpected persisted cursor or bounded request: %#v %#v", source.seen, source.limits)
	}
	var count int64
	if err := db.Model(&models.ProviderEvent{}).Where("connection_id = ?", connection.ID).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("event count = %d err %v", count, err)
	}
	var cursor models.ProviderCursor
	if err := db.First(&cursor, "connection_id = ? AND stream = ?", connection.ID, providerLifeStream).Error; err != nil || cursor.CursorID != "10" {
		t.Fatalf("cursor = %#v err %v", cursor, err)
	}
}

func TestProviderEventsInvalidFreshEventPersistsFallbackBeforeCursor(t *testing.T) {
	db, _, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := connections.Create(actor, ConnectionInput{Name: "invalid event account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().Truncate(time.Second)
	source := &fakeChangeSource{page: cloud.ChangePage{
		Events:     []cloud.ChangeEvent{{ID: "11", Time: timestamp, Kind: "unknown", ItemID: "file", ParentID: "root"}},
		NextCursor: cloud.ChangeCursor{Time: timestamp, ID: "11"},
	}}
	service := NewProviderEventService(db)
	if inserted, _, err := service.IngestOnce(context.Background(), connection.ID, source); err != nil || inserted != 1 {
		t.Fatalf("inserted=%d err=%v", inserted, err)
	}
	var row models.ProviderEvent
	if err := db.First(&row, "connection_id = ?", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	payload, ok := decodePersistedProviderEvent(row)
	if !ok || payload.Kind != cloud.ChangeFallback {
		t.Fatalf("invalid event advanced without fallback: row=%+v payload=%+v ok=%v", row, payload, ok)
	}
}

func TestProviderEventsRemainPendingUntilNotificationSucceeds(t *testing.T) {
	db, _, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := connections.Create(actor, ConnectionInput{Name: "notify account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().Truncate(time.Second)
	source := &fakeChangeSource{page: cloud.ChangePage{Events: []cloud.ChangeEvent{{ID: "evt", Time: timestamp, Kind: cloud.ChangeMoved, ItemID: "file", ParentID: "new", PreviousParentID: "old"}}, NextCursor: cloud.ChangeCursor{Time: timestamp, ID: "evt"}}}
	notifier := &fakeEventNotifier{err: errors.New("temporary wake failure")}
	service := NewProviderEventService(db, notifier)
	if _, _, err := service.IngestOnce(context.Background(), connection.ID, source); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ProcessPending(context.Background(), connection.ID); err == nil || processed != 0 {
		t.Fatalf("failed notification processed = %d err %v", processed, err)
	}
	notifier.err = nil
	if processed, err := service.ProcessPending(context.Background(), connection.ID); err != nil || processed != 1 {
		t.Fatalf("recovered notification processed = %d err %v", processed, err)
	}
	var row models.ProviderEvent
	if err := db.First(&row, "connection_id = ?", connection.ID).Error; err != nil || row.ProcessedAt == nil {
		t.Fatalf("event was not acknowledged: %#v err %v", row, err)
	}
}

func TestProviderEventsNotifyEveryConsumerBeforeAcknowledgement(t *testing.T) {
	db, _, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := connections.Create(actor, ConnectionInput{Name: "fanout account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().Truncate(time.Second)
	source := &fakeChangeSource{page: cloud.ChangePage{Events: []cloud.ChangeEvent{{ID: "fanout", Time: timestamp, Kind: cloud.ChangeCreated, ItemID: "file", ParentID: "root"}}, NextCursor: cloud.ChangeCursor{Time: timestamp, ID: "fanout"}}}
	failing := &fakeEventNotifier{err: errors.New("media library wake failed")}
	downloads := &fakeEventNotifier{}
	service := NewProviderEventService(db, failing, downloads)
	if _, _, err := service.IngestOnce(context.Background(), connection.ID, source); err != nil {
		t.Fatal(err)
	}
	if processed, err := service.ProcessPending(context.Background(), connection.ID); err == nil || processed != 0 {
		t.Fatalf("failed fanout processed = %d err %v", processed, err)
	}
	if len(downloads.events) != 1 {
		t.Fatalf("healthy consumer notifications=%d, want 1", len(downloads.events))
	}
	failing.err = nil
	if processed, err := service.ProcessPending(context.Background(), connection.ID); err != nil || processed != 1 {
		t.Fatalf("recovered fanout processed = %d err %v", processed, err)
	}
}

func TestProviderCursorGapIsPersistedBeforeFallbackNotification(t *testing.T) {
	db, _, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	connection, err := connections.Create(actor, ConnectionInput{Name: "gap account", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	if err := db.Create(&models.ProviderCursor{ConnectionID: connection.ID, Stream: providerLifeStream, CursorTime: base, CursorID: "1", UpdatedAt: base}).Error; err != nil {
		t.Fatal(err)
	}
	next := base.Add(time.Second)
	source := &fakeChangeSource{page: cloud.ChangePage{NextCursor: cloud.ChangeCursor{Time: next, ID: "9"}, FullFallback: true}}
	notifier := &fakeEventNotifier{}
	service := NewProviderEventService(db, notifier)
	if inserted, _, err := service.IngestOnce(context.Background(), connection.ID, source); err != nil || inserted != 1 {
		t.Fatalf("inserted=%d err=%v", inserted, err)
	}
	if processed, err := service.ProcessPending(context.Background(), connection.ID); err != nil || processed != 1 || len(notifier.events) != 1 {
		t.Fatalf("processed=%d events=%d err=%v", processed, len(notifier.events), err)
	}
	payload, ok := decodePersistedProviderEvent(notifier.events[0])
	if !ok || payload.Kind != cloud.ChangeFallback || notifier.events[0].ProcessedAt != nil {
		t.Fatalf("fallback event=%+v payload=%+v ok=%v", notifier.events[0], payload, ok)
	}
}
