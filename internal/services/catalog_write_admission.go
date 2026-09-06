package services

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"
)

// CatalogWriteAdmission schedules whole bounded write transactions, never SQL
// callbacks inside a transaction. All participants must use the same instance.
// Unregistered writers retain SQLite's scheduling and have no fairness promise.
type CatalogWriteAdmission struct {
	mu         sync.Mutex
	active     bool
	foreground []*catalogWriteWaiter
	background []*catalogWriteWaiter
	stats      CatalogWriteAdmissionStats
}

type CatalogWriteAdmissionStats struct {
	ForegroundCompleted uint64
	BackgroundCompleted uint64
	ForegroundQueued    int
	BackgroundQueued    int
	MaxForegroundWait   time.Duration
	MaxBackgroundWait   time.Duration
	MaxWriteDuration    time.Duration
}

type catalogWriteWaiter struct {
	ready      chan struct{}
	granted    bool
	foreground bool
	queuedAt   time.Time
}

func NewCatalogWriteAdmission() *CatalogWriteAdmission { return &CatalogWriteAdmission{} }

func (a *CatalogWriteAdmission) WithForeground(ctx context.Context, write func() error) error {
	return a.withWrite(ctx, true, write)
}
func (a *CatalogWriteAdmission) WithBackground(ctx context.Context, write func() error) error {
	return a.withWrite(ctx, false, write)
}

func (a *CatalogWriteAdmission) withWrite(ctx context.Context, foreground bool, write func() error) error {
	if write == nil {
		return ErrCatalogInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A nil admission preserves existing isolated legacy fixtures, without
	// pretending they participate in foreground scheduling.
	if a == nil {
		return write()
	}
	w := &catalogWriteWaiter{ready: make(chan struct{}), foreground: foreground, queuedAt: time.Now()}
	a.mu.Lock()
	if (foreground && len(a.foreground) >= 256) || (!foreground && len(a.background) >= 64) {
		a.mu.Unlock()
		return ErrCatalogBudget
	}
	if foreground {
		a.foreground = append(a.foreground, w)
	} else {
		a.background = append(a.background, w)
	}
	a.dispatchLocked()
	a.mu.Unlock()
	select {
	case <-w.ready:
	case <-ctx.Done():
		a.mu.Lock()
		if w.granted {
			a.active = false
		} else {
			queue := &a.background
			if foreground {
				queue = &a.foreground
			}
			for i, item := range *queue {
				if item == w {
					*queue = append((*queue)[:i], (*queue)[i+1:]...)
					break
				}
			}
		}
		a.dispatchLocked()
		a.mu.Unlock()
		return ctx.Err()
	}
	started := time.Now()
	defer func() {
		a.mu.Lock()
		a.active = false
		if foreground {
			a.stats.ForegroundCompleted++
			a.stats.MaxForegroundWait = max(a.stats.MaxForegroundWait, started.Sub(w.queuedAt))
		} else {
			a.stats.BackgroundCompleted++
			a.stats.MaxBackgroundWait = max(a.stats.MaxBackgroundWait, started.Sub(w.queuedAt))
		}
		a.stats.MaxWriteDuration = max(a.stats.MaxWriteDuration, time.Since(started))
		a.dispatchLocked()
		a.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return write()
}

func (a *CatalogWriteAdmission) dispatchLocked() {
	if a.active {
		return
	}
	var next *catalogWriteWaiter
	if len(a.foreground) > 0 {
		next = a.foreground[0]
		a.foreground = a.foreground[1:]
	} else if len(a.background) > 0 {
		next = a.background[0]
		a.background = a.background[1:]
	} else {
		return
	}
	a.active, next.granted = true, true
	close(next.ready)
}

func (a *CatalogWriteAdmission) Stats() CatalogWriteAdmissionStats {
	if a == nil {
		return CatalogWriteAdmissionStats{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	stats := a.stats
	stats.ForegroundQueued, stats.BackgroundQueued = len(a.foreground), len(a.background)
	return stats
}

func (s *CatalogSnapshotStore) Admission() *CatalogWriteAdmission { return s.admission }

// Always enter admission BEFORE opening the immediate transaction. PublishTx
// remains transaction-bound: its caller wraps the outer transaction explicitly.
func (s *CatalogSnapshotStore) writeCatalogBatch(ctx context.Context, write func(*gorm.DB) error) error {
	return s.admission.WithBackground(ctx, func() error { return s.writeDB.WithContext(ctx).Transaction(write) })
}
