package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestLocalStructureBackendMovesCompanionsAndRemovesEmptyOldDirectories(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "斗罗大陆")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	video := []byte("video")
	subtitle := []byte("subtitle")
	if err := os.WriteFile(filepath.Join(old, "episode.mkv"), video, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "episode.zh-CN.srt"), subtitle, 0o644); err != nil {
		t.Fatal(err)
	}
	items := []StructurePlanItem{
		{Kind: "video", SourceRelative: "斗罗大陆/episode.mkv", TargetRelative: "电视剧/动画/斗罗大陆 (2020)/Season 01/斗罗大陆 - S01E01.mkv", Size: int64(len(video))},
		{Kind: "sidecar", SourceRelative: "斗罗大陆/episode.zh-CN.srt", TargetRelative: "电视剧/动画/斗罗大陆 (2020)/Season 01/斗罗大陆 - S01E01.zh-CN.srt", Size: int64(len(subtitle))},
	}
	progress := 0
	err := (localMediaLibraryStructureBackend{}).Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{RelativeRoot: "/"}, Storage: models.Storage{RootPath: root}}, items, func(processed, total int) error { progress = processed; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if progress != 2 {
		t.Fatalf("progress=%d", progress)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(items[0].TargetRelative))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(items[1].TargetRelative))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old directory still exists: %v", err)
	}
	// A crash after file moves and empty-directory cleanup must remain resumable.
	if err := (localMediaLibraryStructureBackend{}).Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{RelativeRoot: "/"}, Storage: models.Storage{RootPath: root}}, items, nil); err != nil {
		t.Fatalf("replay after source directory removal: %v", err)
	}
}

type structureCloudDriver struct {
	items            map[string]cloudpkg.Item
	next             int
	moveBatches      [][]string
	recycleBatch     [][]string
	listCalls        map[string]int
	renameCalls      int
	statCalls        int
	nonPipelineReads int
}

func (d *structureCloudDriver) Provider() string { return cloudpkg.ProviderPan115 }
func (d *structureCloudDriver) Capabilities() cloudpkg.Capabilities {
	return cloudpkg.Capabilities{CreateDirectory: true, Move: true, Rename: true, Recycle: true}
}
func (d *structureCloudDriver) Probe(context.Context) (cloudpkg.Account, error) {
	return cloudpkg.Account{}, nil
}
func (d *structureCloudDriver) Stat(ctx context.Context, id string) (cloudpkg.Item, error) {
	d.statCalls++
	if cloudpkg.ReadClassFromContext(ctx) != cloudpkg.ReadClassPipeline {
		d.nonPipelineReads++
	}
	item, ok := d.items[id]
	if !ok {
		return cloudpkg.Item{}, os.ErrNotExist
	}
	return item, nil
}
func (d *structureCloudDriver) List(ctx context.Context, parent string, _ cloudpkg.PageRequest) (cloudpkg.Page, error) {
	if cloudpkg.ReadClassFromContext(ctx) != cloudpkg.ReadClassPipeline {
		d.nonPipelineReads++
	}
	if d.listCalls == nil {
		d.listCalls = make(map[string]int)
	}
	d.listCalls[parent]++
	page := cloudpkg.Page{}
	for _, item := range d.items {
		if item.ParentID == parent {
			page.Items = append(page.Items, item)
		}
	}
	return page, nil
}
func (*structureCloudDriver) DirectURL(context.Context, cloudpkg.DirectURLRequest) (cloudpkg.TemporaryURL, error) {
	return cloudpkg.TemporaryURL{}, nil
}
func (d *structureCloudDriver) CreateDirectory(_ context.Context, parent, name string) (cloudpkg.Item, error) {
	d.next++
	item := cloudpkg.Item{ID: fmt.Sprintf("dir-%d", d.next), ParentID: parent, Name: name, IsDir: true}
	d.items[item.ID] = item
	return item, nil
}
func (d *structureCloudDriver) Move(_ context.Context, id, parent string) error {
	item := d.items[id]
	item.ParentID = parent
	d.items[id] = item
	return nil
}
func (*structureCloudDriver) Copy(context.Context, string, string) error { return errors.New("unused") }
func (d *structureCloudDriver) Rename(_ context.Context, id, name string) error {
	d.renameCalls++
	item := d.items[id]
	item.Name = name
	d.items[id] = item
	return nil
}
func (d *structureCloudDriver) Recycle(_ context.Context, id string) error {
	delete(d.items, id)
	return nil
}
func (d *structureCloudDriver) MoveMany(ctx context.Context, ids []string, parent string) error {
	d.moveBatches = append(d.moveBatches, append([]string(nil), ids...))
	for _, id := range ids {
		if err := d.Move(ctx, id, parent); err != nil {
			return err
		}
	}
	return nil
}
func (*structureCloudDriver) CopyMany(context.Context, []string, string) error {
	return errors.New("unused")
}
func (d *structureCloudDriver) RecycleMany(ctx context.Context, ids []string) error {
	d.recycleBatch = append(d.recycleBatch, append([]string(nil), ids...))
	for _, id := range ids {
		if err := d.Recycle(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func TestPan115StructureBackendMovesByStableIdentityAndCleansEmptyDirectory(t *testing.T) {
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":  {ID: "root", IsDir: true},
		"old":   {ID: "old", ParentID: "root", Name: "斗罗大陆", IsDir: true},
		"video": {ID: "video", ParentID: "old", Name: "episode.mkv", Size: 5},
	}}
	backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(3)
	err := backend.Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}, []StructurePlanItem{{Kind: "video", ProviderID: "video", SourceRelative: "斗罗大陆/episode.mkv", TargetRelative: "电视剧/动画/斗罗大陆/Season 01/斗罗大陆 - S01E01.mkv", Size: 5}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	video := driver.items["video"]
	if video.Name != "斗罗大陆 - S01E01.mkv" || video.ParentID == "old" {
		t.Fatalf("video=%+v", video)
	}
	if _, exists := driver.items["old"]; exists {
		t.Fatal("empty old provider directory was not recycled")
	}
}

func TestPan115StructureBackendBatchesSafeMovesAndListsTargetOnce(t *testing.T) {
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":   {ID: "root", IsDir: true},
		"old":    {ID: "old", ParentID: "root", Name: "old", IsDir: true},
		"target": {ID: "target", ParentID: "root", Name: "target", IsDir: true},
	}}
	items := make([]StructurePlanItem, 0, 235)
	for index := 0; index < 235; index++ {
		id := fmt.Sprintf("video-%03d", index)
		name := fmt.Sprintf("episode-%03d.mkv", index)
		driver.items[id] = cloudpkg.Item{ID: id, ParentID: "old", Name: name, Size: 1}
		items = append(items, StructurePlanItem{Kind: "video", ProviderID: id, SourceRelative: "old/" + name, TargetRelative: "target/" + name, Size: 1})
	}
	backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(3)
	boundary := StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}, preparedParents: map[string]string{"": "root", ".": "root", "target": "target"}}
	if err := backend.Apply(context.Background(), boundary, items, nil); err != nil {
		t.Fatal(err)
	}
	batchSizes := make([]int, 0, len(driver.moveBatches))
	for _, batch := range driver.moveBatches {
		batchSizes = append(batchSizes, len(batch))
	}
	if !reflect.DeepEqual(batchSizes, []int{100, 100, 35}) {
		t.Fatalf("move batch sizes=%v", batchSizes)
	}
	if driver.listCalls["target"] != 4 {
		t.Fatalf("target listings=%d", driver.listCalls["target"])
	}
	if driver.statCalls > 3 {
		t.Fatalf("batch preflight/reconciliation regressed to per-item Stat calls=%d", driver.statCalls)
	}
	if driver.nonPipelineReads != 0 {
		t.Fatalf("structure repair used conservatively paced background reads=%d", driver.nonPipelineReads)
	}
}

func TestPan115StructureBackendResumesMoveBeforeRenameWithoutPerItemStat(t *testing.T) {
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":   {ID: "root", IsDir: true},
		"old":    {ID: "old", ParentID: "root", Name: "old", IsDir: true},
		"target": {ID: "target", ParentID: "root", Name: "target", IsDir: true},
		// The move committed but the rename/checkpoint did not.
		"video": {ID: "video", ParentID: "target", Name: "old-name.mkv", Size: 7},
	}}
	backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(3)
	boundary := StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}, preparedParents: map[string]string{"": "root", ".": "root", "target": "target"}}
	item := StructurePlanItem{Kind: "video", ProviderID: "video", SourceRelative: "old/old-name.mkv", TargetRelative: "target/new-name.mkv", Size: 7}
	if err := backend.Apply(context.Background(), boundary, []StructurePlanItem{item}, nil); err != nil {
		t.Fatal(err)
	}
	if current := driver.items["video"]; current.ParentID != "target" || current.Name != "new-name.mkv" {
		t.Fatalf("resumed item=%+v", current)
	}
	if driver.statCalls != 0 || driver.nonPipelineReads != 0 {
		t.Fatalf("resume used stat=%d background=%d", driver.statCalls, driver.nonPipelineReads)
	}
}

func TestPan115StructureBackendRecyclesOnlyRevalidatedLibraryMember(t *testing.T) {
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":   {ID: "root", IsDir: true},
		"folder": {ID: "folder", ParentID: "root", Name: "incoming", IsDir: true},
		"video":  {ID: "video", ParentID: "folder", Name: "copy.mkv", Size: 4},
		"other":  {ID: "other", ParentID: "0", Name: "outside.mkv", Size: 4},
	}}
	backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(3)
	boundary := StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}
	if err := backend.Recycle(context.Background(), boundary, []StructureRecycleItem{{Kind: "video", SourceRelative: "incoming/copy.mkv", ProviderID: "video", Size: 4}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := driver.items["video"]; exists {
		t.Fatal("selected provider item did not enter recycle bin")
	}
	if err := backend.Recycle(context.Background(), boundary, []StructureRecycleItem{{Kind: "video", SourceRelative: "outside.mkv", ProviderID: "other", Size: 4}}, nil); err == nil {
		t.Fatal("provider item outside the library root was recycled")
	}
	if _, exists := driver.items["other"]; !exists {
		t.Fatal("out-of-bound provider item changed")
	}
}

func TestPan115StructureBackendBatchesRecycleWithDirectoryProof(t *testing.T) {
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":   {ID: "root", IsDir: true},
		"folder": {ID: "folder", ParentID: "root", Name: "incoming", IsDir: true},
	}}
	items := make([]StructureRecycleItem, 0, 235)
	for index := 0; index < 235; index++ {
		id := fmt.Sprintf("recycle-%03d", index)
		name := fmt.Sprintf("copy-%03d.mkv", index)
		driver.items[id] = cloudpkg.Item{ID: id, ParentID: "folder", Name: name, Size: 2}
		items = append(items, StructureRecycleItem{Kind: "video", ProviderID: id, SourceRelative: "incoming/" + name, Size: 2})
	}
	backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(3)
	boundary := StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}
	if err := backend.Recycle(context.Background(), boundary, items, nil); err != nil {
		t.Fatal(err)
	}
	batchSizes := make([]int, 0, len(driver.recycleBatch))
	for _, batch := range driver.recycleBatch {
		batchSizes = append(batchSizes, len(batch))
	}
	if !reflect.DeepEqual(batchSizes, []int{100, 100, 35}) {
		t.Fatalf("recycle batch sizes=%v", batchSizes)
	}
	if driver.listCalls["folder"] != 4 || driver.statCalls != 0 || driver.nonPipelineReads != 0 {
		t.Fatalf("recycle proof list=%d stat=%d background=%d", driver.listCalls["folder"], driver.statCalls, driver.nonPipelineReads)
	}
}

func TestPan115StructureBackendRepairsOnlyExplicitHistoricalProviderRootItem(t *testing.T) {
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":  {ID: "root", ParentID: "0", IsDir: true},
		"video": {ID: "video", ParentID: "0", Name: "movie.mkv", Size: 5},
	}}
	backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(3)
	item := StructurePlanItem{Kind: "video", ProviderID: "video", SourceRelative: "网盘根目录/movie.mkv", TargetRelative: "电影/动画/movie.mkv", AllowProviderRootSource: true, Size: 5}
	if err := backend.Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}, []StructurePlanItem{item}, nil); err != nil {
		t.Fatal(err)
	}
	if driver.items["video"].ParentID == "0" {
		t.Fatalf("historical root item was not moved: %+v", driver.items["video"])
	}
	if err := backend.Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}, []StructurePlanItem{item}, nil); err != nil {
		t.Fatalf("exact completed historical repair was not idempotent: %v", err)
	}

	driver.items["video"] = cloudpkg.Item{ID: "video", ParentID: "0", Name: "movie.mkv", Size: 5}
	item.AllowProviderRootSource = false
	if err := backend.Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}, []StructurePlanItem{item}, nil); err == nil {
		t.Fatal("ordinary structure plan was allowed to move a provider-root item")
	}
}

func TestPan115StructureBackendCleansSiblingOldDirectoriesBeforeTheirSharedParent(t *testing.T) {
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":    {ID: "root", IsDir: true},
		"legacy":  {ID: "legacy", ParentID: "root", Name: "旧电视剧", IsDir: true},
		"season1": {ID: "season1", ParentID: "legacy", Name: "第一季", IsDir: true},
		"season2": {ID: "season2", ParentID: "legacy", Name: "第二季", IsDir: true},
		"video1":  {ID: "video1", ParentID: "season1", Name: "one.mkv", Size: 1},
		"video2":  {ID: "video2", ParentID: "season2", Name: "two.mkv", Size: 1},
	}}
	backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(3)
	items := []StructurePlanItem{
		{Kind: "video", ProviderID: "video1", SourceRelative: "旧电视剧/第一季/one.mkv", TargetRelative: "电视剧/动画/剧/Season 01/剧 - S01E01.mkv", Size: 1},
		{Kind: "video", ProviderID: "video2", SourceRelative: "旧电视剧/第二季/two.mkv", TargetRelative: "电视剧/动画/剧/Season 02/剧 - S02E01.mkv", Size: 1},
	}
	if err := backend.Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}, items, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"season1", "season2", "legacy"} {
		if _, exists := driver.items[id]; exists {
			t.Fatalf("empty provider directory %s was not recycled", id)
		}
	}
}

func TestLocalStructureBackendFailsClosedOnTargetConflict(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "电影"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "old", "movie.mkv"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "电影", "movie.mkv"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := (localMediaLibraryStructureBackend{}).Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{RelativeRoot: "/"}, Storage: models.Storage{RootPath: root}}, []StructurePlanItem{{Kind: "video", SourceRelative: "old/movie.mkv", TargetRelative: "电影/movie.mkv", Size: 3}}, nil)
	if err != errStructureConflict {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "old", "movie.mkv")); err != nil {
		t.Fatal("conflict changed source")
	}
}

func TestLocalStructureBackendRejectsSymlinkedSourceAncestor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "movie.mkv"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "incoming")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	err := (localMediaLibraryStructureBackend{}).Apply(context.Background(), StructureBoundary{Library: models.MediaLibrary{RelativeRoot: "/"}, Storage: models.Storage{RootPath: root}}, []StructurePlanItem{{Kind: "video", SourceRelative: "incoming/movie.mkv", TargetRelative: "电影/movie.mkv", Size: 7}}, nil)
	if err == nil {
		t.Fatal("symlinked source ancestor was accepted")
	}
	if data, readErr := os.ReadFile(filepath.Join(outside, "movie.mkv")); readErr != nil || string(data) != "outside" {
		t.Fatalf("outside file changed: %q err=%v", data, readErr)
	}
}

func TestPan115StructureBackendResumesSameDirectoryRename(t *testing.T) {
	for _, mode := range []string{"completed", "unrelated_name", "wrong_size", "destination_conflict"} {
		t.Run(mode, func(t *testing.T) {
			driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
				"root": {ID: "root", IsDir: true}, "folder": {ID: "folder", ParentID: "root", Name: "movies", IsDir: true},
				"video": {ID: "video", ParentID: "folder", Name: "new.mkv", Size: 7},
			}}
			current := driver.items["video"]
			switch mode {
			case "unrelated_name":
				current.Name = "other.mkv"
			case "wrong_size":
				current.Size = 8
			case "destination_conflict":
				driver.items["other"] = cloudpkg.Item{ID: "other", ParentID: "folder", Name: "new.mkv", Size: 7}
			}
			driver.items["video"] = current
			backend := pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
			connectionID := uint(3)
			boundary := StructureBoundary{Library: models.MediaLibrary{ProviderRootID: "root"}, Storage: models.Storage{ConnectionID: &connectionID, RootPath: "root"}}
			item := StructurePlanItem{Kind: "video", ProviderID: "video", SourceRelative: "movies/old.mkv", TargetRelative: "movies/new.mkv", Size: 7}
			err := backend.Apply(context.Background(), boundary, []StructurePlanItem{item}, nil)
			if (err == nil) != (mode == "completed") {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
			if driver.renameCalls != 0 || len(driver.moveBatches) != 0 || driver.items["video"] != current {
				t.Fatal("retry mutated an already completed or invalid item")
			}
		})
	}
}
