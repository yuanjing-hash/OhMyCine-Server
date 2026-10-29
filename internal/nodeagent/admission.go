package nodeagent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// operationAdmission is shared by every Node-owned data/metadata execution
// lane. Lowering the limit keeps accepted work but admits no extra work until
// active usage falls below the new bound. Queue waits always have an owner.
type operationAdmission struct {
	mu            sync.Mutex
	limit, active int
	queue         []uint64
	next          uint64
	changed       chan struct{}
}

func newOperationAdmission(limit int) *operationAdmission {
	if limit < 1 || limit > 32 {
		limit = 1
	}
	return &operationAdmission{limit: limit, changed: make(chan struct{})}
}
func (g *operationAdmission) notify() { close(g.changed); g.changed = make(chan struct{}) }
func (g *operationAdmission) setLimit(limit int) error {
	if limit < 1 || limit > 32 {
		return errors.New("node_concurrency_invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limit = limit
	g.notify()
	return nil
}
func (g *operationAdmission) acquire(ctx context.Context) (func(), error) {
	g.mu.Lock()
	if len(g.queue) >= 128 {
		g.mu.Unlock()
		return nil, errors.New("node_operation_busy")
	}
	g.next++
	ticket := g.next
	g.queue = append(g.queue, ticket)
	remove := func() {
		for i, v := range g.queue {
			if v == ticket {
				g.queue = append(g.queue[:i], g.queue[i+1:]...)
				break
			}
		}
		g.notify()
	}
	for {
		if err := ctx.Err(); err != nil {
			remove()
			g.mu.Unlock()
			return nil, err
		}
		if g.active < g.limit && len(g.queue) > 0 && g.queue[0] == ticket {
			g.queue = g.queue[1:]
			g.active++
			g.notify()
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { g.mu.Lock(); g.active--; g.notify(); g.mu.Unlock() }) }, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		g.mu.Lock()
	}
}
func (a *Agent) acquireOperation(ctx context.Context) (func(), error) {
	a.admissionOnce.Do(func() { a.admission = newOperationAdmission(a.config.MaxConcurrentOperations) })
	return a.admission.acquire(ctx)
}

// Persistent operation identity remains in Store. This watcher only owns the
// in-process queue/running context; cancellation never creates another plan.
func (a *Agent) ownedOperationContext(parent context.Context, serverID, key, digest string, credentialExpiry time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !credentialExpiry.After(a.now()) {
					cancel()
					return
				}
				if _, err := a.store.ActiveOperationPlan(ctx, serverID, key, digest, a.now()); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}
