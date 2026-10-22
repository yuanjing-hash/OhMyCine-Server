package services

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// Exercises real writes/reuse rather than asserting source text or templates:
// an unchanged multi-file run must not rewrite files or emit one INFO per file.
func TestArtifactBusinessReuseIsBoundedAndPreservesFiles(t *testing.T) {
	management, queue, _, library, root := strmManagementFixture(t)
	var log bytes.Buffer
	service := NewMediaArtifactService(management.db, queue, nil, zerolog.New(&log).With().Timestamp().Logger())
	run := models.MediaArtifactRun{ID: "artifact-observation-fixture", LibraryID: library.ID, Generation: library.ArtifactGeneration, PolicyJSON: `{}`, Status: models.MediaArtifactStatusRunning}
	if err := management.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	write := func(index int, content string, manifest *artifactManifestIndex) string {
		t.Helper()
		outcome, err := service.writeLocalArtifact(root, run, localArtifactSpec{Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: fmt.Sprintf("/Show/%02d.nfo", index), Manifest: manifest, Content: func(models.MediaArtifact) ([]byte, error) { return []byte(content), nil }})
		if err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	for index := 0; index < 32; index++ {
		if got := write(index, "unchanged NFO", nil); got != "written" {
			t.Fatal(got)
		}
	}
	before, err := os.Stat(filepath.Join(root, "Show", "00.nfo"))
	if err != nil {
		t.Fatal(err)
	}
	log.Reset()
	state := &artifactBusinessState{counts: map[string]int{}}
	ctx := context.WithValue(context.Background(), artifactBusinessKey{}, state)
	manifest := newArtifactManifestIndex(32)
	manifest.lazy, manifest.observationContext = true, ctx
	for index := 0; index < 32; index++ {
		if got := write(index, "unchanged NFO", manifest); got != "skipped" {
			t.Fatal(got)
		}
	}
	if events := businessRecords(t, &log, "event_code", "artifact.file"); len(events) != 20 {
		t.Fatalf("unchanged INFO events=%d want20", len(events))
	}
	if state.omitted != 12 || state.counts["skipped"] != 32 {
		t.Fatalf("omitted=%d counts=%v", state.omitted, state.counts)
	}
	after, err := os.Stat(filepath.Join(root, "Show", "00.nfo"))
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("unchanged file was rewritten: before=%v after=%v err=%v", before, after, err)
	}
	if got := write(0, "updated NFO", nil); got != "updated" {
		t.Fatal(got)
	}
	log.Reset()
	if _, err := service.writeLocalArtifact(root, run, localArtifactSpec{Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/../../private-root-marker/file.nfo", Content: func(models.MediaArtifact) ([]byte, error) { t.Fatal("outside-root renderer ran"); return nil, nil }}); err == nil {
		t.Fatal("outside-root artifact path was accepted")
	}
	if strings.Contains(log.String(), "private-root-marker") {
		t.Fatal("unvalidated path was emitted as a scope-relative location")
	}
	for _, invalid := range []string{`C:\private-root-marker\file.nfo`, `\\server\share\file.nfo`, "../file.nfo", "/../file.nfo", "https://host/path?token=private"} {
		if artifactDisplayPath(invalid) != "" {
			t.Fatalf("malicious display path survived: %q", invalid)
		}
	}
}

func TestCatalogScanBusinessSamplesFollowCommittedChanges(t *testing.T) {
	service, library, storage, profile, _ := catalogScanFixture(t, false)
	var log bytes.Buffer
	service.log = zerolog.New(&log).Level(zerolog.InfoLevel).With().Timestamp().Logger()
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	files := make([]medialibrary.File, 0, 33)
	for index := 0; index < 33; index++ {
		files = append(files, medialibrary.File{RelativePath: fmt.Sprintf("/Movies/Fixture.%02d.2026.mkv", index), Size: 100, ModifiedAt: time.Now().UTC()})
	}
	published, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files}, true, service.catalogScanCommit)
	if err != nil {
		t.Fatal(err)
	}
	if published.Added != 33 {
		t.Fatalf("real catalog counters changed: %+v", published)
	}
	if records := businessRecords(t, &log, "event_code", "library.catalog.changed"); len(records) != 20 {
		t.Fatalf("INFO samples=%d want20", len(records))
	}
	summary := businessRecords(t, &log, "event_code", "library.catalog.samples")
	if len(summary) != 1 || summary[0]["omitted_count"] != float64(published.Added+published.Updated+published.Removed-20) {
		t.Fatalf("omission summary did not reflect committed counters: %+v", summary)
	}
}
