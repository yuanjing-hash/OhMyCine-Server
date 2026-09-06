package services

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"gorm.io/gorm"
)

func waitCatalogAdmissionQueue(t *testing.T, a *CatalogWriteAdmission, foreground, background int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := a.Stats()
		if s.ForegroundQueued == foreground && s.BackgroundQueued == background {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("admission queue not reached: %+v", a.Stats())
}

func TestCatalogWriteAdmissionForegroundPrecedesQueuedBackground(t *testing.T) {
	a := NewCatalogWriteAdmission()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 3)
	order := make(chan string, 3)
	go func() {
		done <- a.WithBackground(context.Background(), func() error { order <- "first-background"; close(entered); <-release; return nil })
	}()
	<-entered
	go func() {
		done <- a.WithBackground(context.Background(), func() error { order <- "queued-background"; return nil })
	}()
	waitCatalogAdmissionQueue(t, a, 0, 1)
	go func() {
		done <- a.WithForeground(context.Background(), func() error { order <- "foreground"; return nil })
	}()
	waitCatalogAdmissionQueue(t, a, 1, 1)
	close(release)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range []string{"first-background", "foreground", "queued-background"} {
		if got := <-order; got != expected {
			t.Fatalf("got %s, expected %s", got, expected)
		}
	}
	s := a.Stats()
	if s.ForegroundCompleted != 1 || s.BackgroundCompleted != 2 || s.ForegroundQueued != 0 || s.BackgroundQueued != 0 {
		t.Fatalf("admission stats: %+v", s)
	}
}

func TestCatalogWriteAdmissionCancellationRemovesDemand(t *testing.T) {
	a := NewCatalogWriteAdmission()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- a.WithBackground(context.Background(), func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		done <- a.WithForeground(ctx, func() error { t.Error("canceled waiter executed"); return nil })
	}()
	waitCatalogAdmissionQueue(t, a, 1, 0)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	waitCatalogAdmissionQueue(t, a, 0, 0)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := a.WithForeground(context.Background(), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogWriteAdmissionSchedulesBeforeActualSQLiteTransactions(t *testing.T) {
	store, _, _, _ := catalogFixture(t)
	if err := store.writeDB.Exec("CREATE TABLE catalog_admission_probe(id INTEGER PRIMARY KEY, label TEXT NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	a := store.Admission()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 3)
	go func() {
		done <- store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error {
			if err := tx.Exec("INSERT INTO catalog_admission_probe(label) VALUES('first')").Error; err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	go func() {
		done <- store.writeCatalogBatch(context.Background(), func(tx *gorm.DB) error {
			return tx.Exec("INSERT INTO catalog_admission_probe(label) VALUES('background')").Error
		})
	}()
	waitCatalogAdmissionQueue(t, a, 0, 1)
	go func() {
		done <- a.WithForeground(context.Background(), func() error {
			return store.writeDB.Transaction(func(tx *gorm.DB) error {
				return tx.Exec("INSERT INTO catalog_admission_probe(label) VALUES('history-heartbeat')").Error
			})
		})
	}()
	waitCatalogAdmissionQueue(t, a, 1, 1)
	close(release)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	var labels []string
	if err := store.writeDB.Table("catalog_admission_probe").Order("id").Pluck("label", &labels).Error; err != nil {
		t.Fatal(err)
	}
	if len(labels) != 3 || labels[1] != "history-heartbeat" || labels[2] != "background" {
		t.Fatalf("SQLite commit order: %v", labels)
	}
}
