package cloud

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type selectionDriver struct {
	MutationDriver
	tree     map[string][]ShareItem
	output   map[string][]Item
	received []string
	lostACK  bool
}

func (d *selectionDriver) InspectShare(ctx context.Context, raw string) (ShareSnapshot, error) {
	return d.InspectShareDirectory(ctx, raw, "0")
}
func (d *selectionDriver) InspectShareDirectory(_ context.Context, _ string, id string) (ShareSnapshot, error) {
	return ShareSnapshot{ShareCode: "share", ReceiveCode: "abcd", Items: d.tree[id]}, nil
}
func (d *selectionDriver) List(_ context.Context, id string, _ PageRequest) (Page, error) {
	return Page{Items: d.output[id]}, nil
}
func (d *selectionDriver) CreateDirectory(_ context.Context, parent, name string) (Item, error) {
	item := Item{ID: parent + "/" + name, ParentID: parent, Name: name, IsDir: true}
	d.output[parent] = append(d.output[parent], item)
	return item, nil
}
func (d *selectionDriver) ReceiveShare(_ context.Context, s ShareSnapshot, parent string) error {
	for _, file := range s.Items {
		if file.IsDir {
			panic("directory must never be received")
		}
		d.received = append(d.received, file.ID)
		d.output[parent] = append(d.output[parent], Item{ID: "copy-" + file.ID, ParentID: parent, Name: file.Name, Size: file.Size})
	}
	if d.lostACK {
		return errors.New("lost ack")
	}
	return nil
}
func TestSelectedSharePreservesScopeAndRecoversLostACK(t *testing.T) {
	d := &selectionDriver{tree: map[string][]ShareItem{"0": {{ID: "dir", Name: "Movies", IsDir: true}, {ID: "other", Name: "Other.mkv", Size: 20}}, "dir": {{ID: "one", Name: "One.mkv", Size: 10}, {ID: "two", Name: "Two.mkv", Size: 30}}}, output: map[string][]Item{}, lostACK: true}
	_, tree, err := InspectShareTree(context.Background(), d, "share")
	if err != nil || len(tree) != 4 {
		t.Fatalf("tree=%v err=%v", tree, err)
	}
	selected := ShareSelection{Version: 1, Files: []ShareTreeItem{{ID: "one", RelativePath: "Movies/One.mkv", Size: 10}}}
	if err := ReceiveSelectedShare(context.Background(), d, "share", selected, "target"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(d.received) != "[one]" || len(d.output["target/Movies"]) != 1 {
		t.Fatalf("selection widened: %+v", d)
	}

	// Resume a partially materialized multi-file selection: only the missing leaf is sent.
	partial := &selectionDriver{tree: d.tree, output: map[string][]Item{"target": {{ID: "dir-copy", ParentID: "target", Name: "Movies", IsDir: true}}, "dir-copy": {{ID: "one-copy", ParentID: "dir-copy", Name: "One.mkv", Size: 10}}}}
	both := ShareSelection{Version: 1, Files: []ShareTreeItem{{ID: "one", RelativePath: "Movies/One.mkv", Size: 10}, {ID: "two", RelativePath: "Movies/Two.mkv", Size: 30}}}
	if err := ReceiveSelectedShare(context.Background(), partial, "share", both, "target"); err != nil || fmt.Sprint(partial.received) != "[two]" {
		t.Fatalf("partial retry widened: %v %v", partial.received, err)
	}
	d.tree = nil // Completed output recovers without a now-deleted share.
	if err := ReceiveSelectedShare(context.Background(), d, "share", selected, "target"); err != nil || len(d.received) != 1 {
		t.Fatalf("retry duplicated: %v %v", d.received, err)
	}
	d.output["target/Movies"] = append(d.output["target/Movies"], Item{ID: "extra", ParentID: "target/Movies", Name: "Unexpected.mkv"})
	if err := ReceiveSelectedShare(context.Background(), d, "share", selected, "target"); err == nil {
		t.Fatal("unknown output accepted")
	}
}
func TestSelectedShareRejectsChangedSourceAndUnsafeTree(t *testing.T) {
	d := &selectionDriver{tree: map[string][]ShareItem{"0": {{ID: "file", Name: "Movie.mkv", Size: 2}}}, output: map[string][]Item{}}
	selection := ShareSelection{Version: 1, Files: []ShareTreeItem{{ID: "file", RelativePath: "Movie.mkv", Size: 1}}}
	if err := ReceiveSelectedShare(context.Background(), d, "share", selection, "target"); err == nil || len(d.received) != 0 {
		t.Fatal("changed source received")
	}
	for _, name := range []string{"../escape", "a/b", "..", "/absolute"} {
		d.tree["0"] = []ShareItem{{ID: "file", Name: name}}
		if _, _, err := InspectShareTree(context.Background(), d, "share"); err == nil {
			t.Fatalf("unsafe path accepted: %s", name)
		}
	}
	d.tree["0"] = make([]ShareItem, MaxSharePreviewEntries+1)
	for i := range d.tree["0"] {
		d.tree["0"][i] = ShareItem{ID: fmt.Sprint(i + 1), Name: fmt.Sprint(i + 1)}
	}
	if _, items, err := InspectShareTree(context.Background(), d, "share"); err == nil || items != nil {
		t.Fatal("oversized share exposed partially")
	}
}
