package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

type structureCloudDriver struct {
	items map[string]cloudpkg.Item
	next  int
}

func (d *structureCloudDriver) Provider() string { return cloudpkg.ProviderPan115 }
func (d *structureCloudDriver) Capabilities() cloudpkg.Capabilities {
	return cloudpkg.Capabilities{CreateDirectory: true, Move: true, Rename: true, Recycle: true}
}
func (d *structureCloudDriver) Probe(context.Context) (cloudpkg.Account, error) {
	return cloudpkg.Account{}, nil
}
func (d *structureCloudDriver) Stat(_ context.Context, id string) (cloudpkg.Item, error) {
	item, ok := d.items[id]
	if !ok {
		return cloudpkg.Item{}, os.ErrNotExist
	}
	return item, nil
}
func (d *structureCloudDriver) List(_ context.Context, parent string, _ cloudpkg.PageRequest) (cloudpkg.Page, error) {
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
	item := d.items[id]
	item.Name = name
	d.items[id] = item
	return nil
}
func (d *structureCloudDriver) Recycle(_ context.Context, id string) error {
	delete(d.items, id)
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
