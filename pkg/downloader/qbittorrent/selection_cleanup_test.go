package qbittorrent

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelectionGroupCleanupUnknownDeleteOnlyProbesAndIgnoresNewLifecycle(t *testing.T) {
	source, binding := selectedFixture(t)
	state := &selectedQB{source: source, binding: binding, api: "2.11.3", priorities: [2]int{1, 1}, groupMode: true, allowCleanup: true, lostDeleteACK: true}
	server := httptest.NewServer(state.handler(t))
	defer server.Close()
	client, _ := New(downloader.Config{BaseURL: server.URL})
	ctx := context.Background()
	id := strings.Repeat("a", 64)
	g := downloader.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/managed/.omc-bt/" + id, Revision: 1, Epoch: 1, Union: binding.Selection}
	request := downloader.SubmitRequest{Source: source, Tag: binding.Tag, SavePath: g.Workspace}
	if _, err := client.ApplySelectionGroup(ctx, request, g); err != nil {
		t.Fatal(err)
	}
	binding.Group = &g
	cleanup := downloader.GroupCleanup{Key: strings.Repeat("c", 64), Group: g, Files: binding.Selection, DeleteFiles: true}
	if gone, err := client.CleanupSelectionGroup(ctx, source, binding, cleanup); err == nil || gone || state.deletes != 1 {
		t.Fatal("unknown delete not retained", gone, err, state.deletes)
	}
	cleanup.ProbeOnly = true
	if gone, err := client.CleanupSelectionGroup(ctx, source, binding, cleanup); err != nil || !gone || state.deletes != 1 {
		t.Fatal("replayed uncertain deletion", gone, err, state.deletes)
	}
	// Another generation can now own this hash. Its distinct native tag/root
	// is positive evidence that it is not the old source being reconciled.
	next := g
	next.ID = strings.Repeat("b", 64)
	next.Tag = "omc-group-" + next.ID[:32]
	next.Workspace = "/managed/.omc-bt/" + next.ID
	request.SavePath = next.Workspace
	if _, err := client.ApplySelectionGroup(ctx, request, next); err != nil {
		t.Fatal(err)
	}
	if gone, err := client.CleanupSelectionGroup(ctx, source, binding, cleanup); err != nil || !gone || state.deletes != 1 || !state.exists || state.workspace != next.Workspace {
		t.Fatal("late cleanup touched next lifecycle", gone, err)
	}
}
