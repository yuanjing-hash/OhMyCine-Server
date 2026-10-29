package qbittorrent

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestSelectionGroupIncrementalUnionAndSubsetOutputs(t *testing.T) {
	source, own := selectedFixture(t)
	state := &selectedQB{source: source, binding: own, api: "2.11.3", priorities: [2]int{1, 1}, groupMode: true, indexShift: 7}
	server := httptest.NewServer(state.handler(t))
	defer server.Close()
	client, _ := New(downloader.Config{BaseURL: server.URL})
	ctx := context.Background()
	id := strings.Repeat("a", 64)
	group := downloader.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/managed/.omc-bt/" + id, Revision: 1, Epoch: 1, Union: own.Selection}
	request := downloader.SubmitRequest{Source: source, Tag: own.Tag, SavePath: group.Workspace}
	task, err := client.ApplySelectionGroup(ctx, request, group)
	if err != nil {
		t.Fatal(err)
	}
	if state.adds != 1 || state.resumes != 0 || state.priorities != [2]int{1, 0} {
		t.Fatal("unsafe first selection")
	}
	d1, _ := group.Union.Digest()
	group.Revision++
	group.Epoch++
	group.PreviousDigests = []string{d1}
	group.Resume = true
	group.Union.Files = append(append([]resourcecontent.SelectedFile(nil), group.Union.Files...), resourcecontent.SelectedFile{Index: 1, Path: "b.mkv", Size: 20})
	if _, err = client.ApplySelectionGroup(ctx, request, group); err != nil {
		t.Fatal(err)
	}
	if state.adds != 1 || state.priorities != [2]int{1, 1} || state.resumes != 1 {
		t.Fatal("increment duplicated or wrong union")
	}
	state.mu.Lock()
	state.progress = [2]float64{1, 0}
	state.mu.Unlock()
	firstBinding := own
	firstBinding.Group = &group
	first, err := client.GetSelected(ctx, task.ID, firstBinding)
	if err != nil || !first.Completed || *first.BytesTotal != 10 {
		t.Fatal(first, err)
	}
	second := own
	second.Tag = "omc-" + uuid.NewString()
	second.Selection = resourcecontent.Selection{Version: 1, Identity: own.Selection.Identity, Files: []resourcecontent.SelectedFile{{Index: 1, Path: "b.mkv", Size: 20}}}
	second.Group = &group
	other, err := client.GetSelected(ctx, task.ID, second)
	if err != nil || other.Completed || *other.BytesTotal != 20 {
		t.Fatal(other, err)
	}
	// Release E13 while E15 remains active. Apply the authority's remaining
	// union, never a native cancel/delete of the shared hash.
	d2, _ := group.Union.Digest()
	group.PreviousDigests = []string{d2}
	group.Revision++
	group.Epoch++
	group.Union = second.Selection
	if _, err = client.ApplySelectionGroup(ctx, request, group); err != nil {
		t.Fatal(err)
	}
	if state.adds != 1 || state.priorities != [2]int{0, 1} || state.state != "downloading" {
		t.Fatal("cancel harmed other reference")
	}
	state.mu.Lock()
	state.tags += ",user-owned"
	before := state.writes
	state.mu.Unlock()
	if _, err = client.ApplySelectionGroup(ctx, request, group); err == nil || state.writes != before {
		t.Fatal("adopted user task")
	}
}

func TestSelectionGroupFenceStopsBeforeResumeAndRecoversStoppedTask(t *testing.T) {
	source, own := selectedFixture(t)
	state := &selectedQB{source: source, binding: own, api: "2.11.3", priorities: [2]int{1, 1}, groupMode: true}
	server := httptest.NewServer(state.handler(t))
	defer server.Close()
	client, _ := New(downloader.Config{BaseURL: server.URL})
	id := strings.Repeat("d", 64)
	g := downloader.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/managed/.omc-bt/" + id, Revision: 1, Epoch: 1, Union: own.Selection, Resume: true}
	ctx := downloader.WithSelectionFence(context.Background(), func() error {
		state.mu.Lock()
		writes := state.writes
		state.mu.Unlock()
		if writes >= 2 {
			return downloader.Error("downloader_selection_group_busy", true, nil)
		}
		return nil
	})
	r := downloader.SubmitRequest{Source: source, Tag: own.Tag, SavePath: g.Workspace}
	if _, err := client.ApplySelectionGroup(ctx, r, g); err == nil || state.resumes != 0 || state.adds != 1 || !stopped(state.state) {
		t.Fatal("stale authority started media", err, state.resumes)
	}
	g.Epoch++
	if _, err := client.ApplySelectionGroup(context.Background(), r, g); err != nil || state.adds != 1 || state.resumes != 1 {
		t.Fatal("new authority did not reconcile original stopped task", err)
	}
}
