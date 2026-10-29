package nodeagent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestSharedOperationAdmissionQueueCancelAndDecrease(t *testing.T) {
	gate := newOperationAdmission(2)
	ctx := context.Background()
	a, _ := gate.acquire(ctx)
	b, _ := gate.acquire(ctx)
	if err := gate.setLimit(1); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		release, err := gate.acquire(waitCtx)
		if release != nil {
			release()
		}
		finished <- err
	}()
	time.Sleep(10 * time.Millisecond)
	a()
	select {
	case err := <-finished:
		t.Fatal("decrease admitted while at limit", err)
	case <-time.After(15 * time.Millisecond):
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	b()
	release, err := gate.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active != 0 || len(gate.queue) != 0 {
		t.Fatal("admission lease leaked")
	}
}

func TestAgentSharesSingleBudgetAcrossOwnedLanes(t *testing.T) {
	agent := &Agent{config: Config{MaxConcurrentOperations: 1}}
	first, err := agent.acquireOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		release, err := agent.acquireOperation(ctx)
		if release != nil {
			release()
		}
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("second lane bypassed shared budget")
	case <-time.After(15 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	first()
}

func TestDownloadSelectionPersistsImmutableBindingAndRejectsDrift(t *testing.T) {
	file := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := uuid.NewString()
	binding := downloadpkg.SelectionContext{Tag: "omc-" + id, RootName: "Pack", MultiFile: true, Selection: resourcecontent.Selection{Version: 1, Identity: resourcecontent.Identity{V1: "0123456789abcdef0123456789abcdef01234567"}, Files: []resourcecontent.SelectedFile{{Index: 7, Path: "episode.mkv", Size: 10}}}}
	if err = store.saveDownloadSelection(ctx, "server-1", "downloader-1", id, "native", binding); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(file)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	read, native, err := store.downloadSelection(ctx, "server-1", "downloader-1", id)
	if err != nil || native != "native" || read.Selection.Files[0].Index != 7 {
		t.Fatal(read, native, err)
	}
	if err = store.saveDownloadSelection(ctx, "server-1", "downloader-1", id, "native", binding); err != nil {
		t.Fatal("idempotent replay", err)
	}
	binding.Selection.Files[0].Size = 11
	if err = store.saveDownloadSelection(ctx, "server-1", "downloader-1", id, "native", binding); !errors.Is(err, ErrPlanConflict) {
		t.Fatal("selection drift accepted", err)
	}
	if _, _, err = store.downloadSelection(ctx, "other-server", "downloader-1", id); !errors.Is(err, ErrOperationNotFound) {
		t.Fatal("cross-server selection visible", err)
	}
}
