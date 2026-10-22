package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func fileBusinessRecords(t *testing.T, text string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["event_code"] != nil {
			records = append(records, event)
		}
	}
	return records
}

func fileBusinessEvent(records []map[string]any, code string) map[string]any {
	for _, event := range records {
		if event["event_code"] == code {
			return event
		}
	}
	return nil
}

func TestFileBusinessLocalTransferShowsActualNamesAndConflictResult(t *testing.T) {
	for _, test := range []struct {
		name, policy, code, outcome string
		conflict                    bool
	}{
		{"move_and_rename", models.MediaLibraryConflictOverwrite, "file.result", "confirmed", false},
		{"conflict_rename", models.MediaLibraryConflictRename, "file.result", "confirmed", true},
		{"conflict_skip", models.MediaLibraryConflictSkip, "file.skipped", "skipped", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			queue, _, download, source, destination := transferFixture(t, models.MediaLibraryTransferMove, test.policy, test.conflict)
			var output strings.Builder
			service := NewTransferService(queue.db, queue.audit, queue, zerolog.New(&output))
			manifest := downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.2024.mkv", Size: minimumAutomaticTransferVideoBytes}}}
			if err := service.Enqueue(download, manifest); err != nil {
				t.Fatal(err)
			}
			claim, err := queue.Claim([]string{"transfer"})
			if err != nil || claim == nil {
				t.Fatalf("claim=%v error=%v", claim, err)
			}
			result := NewTransferWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
			if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
				t.Fatalf("result=%+v", result)
			}
			records := fileBusinessRecords(t, output.String())
			event := fileBusinessEvent(records, test.code)
			if event == nil || event["outcome"] != test.outcome || event["old_name"] != "Movie.2024.mkv" || event["size_bytes"] != float64(minimumAutomaticTransferVideoBytes) || event["job_id"] != claim.Job.ID {
				t.Fatalf("file event=%v", event)
			}
			if event["source_relative_path"] == "" || event["target_relative_path"] == "" || event["new_name"] == "" {
				t.Fatalf("missing source/target naming facts: %v", event)
			}
			if test.policy == models.MediaLibraryConflictRename && !strings.Contains(fmt.Sprint(event["new_name"]), "(1)") {
				t.Fatalf("final conflict name was not recorded: %v", event)
			}
			for _, root := range []string{source, download.StagingAbsolutePath, download.TargetStorageRoot} {
				if strings.Contains(fmt.Sprint(records), root) {
					t.Fatalf("private root was logged: %q", root)
				}
			}
			if test.policy == models.MediaLibraryConflictSkip {
				if _, err := os.Stat(source); err != nil {
					t.Fatalf("skipped source changed: %v", err)
				}
			} else {
				if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("move source still present: %v", err)
				}
				actual := filepath.Join(filepath.Dir(destination), fmt.Sprint(event["new_name"]))
				if payload, err := os.ReadFile(actual); err != nil || string(payload) != "video-data" {
					t.Fatalf("target=%q error=%v", payload, err)
				}
			}
			if end := fileBusinessEvent(records, "transfer.finish"); end == nil || end["outcome"] != "finished" {
				t.Fatalf("terminal=%v", end)
			}
		})
	}
}

type fileLogFailingCloudDriver struct {
	*fakeMutationCloudDriver
	renameFailure, moveUncertain bool
}

func (d *fileLogFailingCloudDriver) Rename(ctx context.Context, id, name string) error {
	if d.renameFailure {
		d.renameCalls++
		return cloudpkg.Error(cloudpkg.CodeUnavailable, true, errors.New("Cookie=private-cookie https://upstream.invalid/?passkey=private-key /private/provider/root raw-body"))
	}
	return d.fakeMutationCloudDriver.Rename(ctx, id, name)
}
func (d *fileLogFailingCloudDriver) MoveMany(ctx context.Context, ids []string, parent string) error {
	if d.moveUncertain {
		d.moveBatchCalls++
		return cloudpkg.Error(cloudpkg.CodeMutationUnknown, true, errors.New("upstream private raw-body"))
	}
	return d.fakeMutationCloudDriver.MoveMany(ctx, ids, parent)
}

func TestFileBusinessCloudSeparatesMovedRenameFailureAndUncertainSubmission(t *testing.T) {
	for _, renameFailure := range []bool{true, false} {
		t.Run(fmt.Sprint("rename_failure_", renameFailure), func(t *testing.T) {
			fixture := newCloudTransferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictOverwrite, false)
			var output strings.Builder
			fixture.service.log = zerolog.New(&output)
			driver := &fileLogFailingCloudDriver{fakeMutationCloudDriver: fixture.driver, renameFailure: renameFailure, moveUncertain: !renameFailure}
			fixture.service.connections.drivers[fixture.connection] = driver
			result := fixture.run(t)
			if result.RetryAt == nil {
				t.Fatalf("expected original retry semantics: %+v", result)
			}
			records := fileBusinessRecords(t, output.String())
			if renameFailure {
				if event := fileBusinessEvent(records, "cloud.move.position"); event == nil || event["outcome"] != "confirmed" {
					t.Fatalf("missing confirmed position: %v", records)
				}
				if event := fileBusinessEvent(records, "cloud.rename.partial"); event == nil || event["outcome"] != "partial" {
					t.Fatalf("missing partial rename: %v", records)
				}
				if fixture.driver.items[fixture.sourceID].ParentID == "source-root" || fixture.driver.items[fixture.sourceID].Name != "Movie.2024.mkv" {
					t.Fatal("fixture did not actually move before failing rename")
				}
			} else {
				if event := fileBusinessEvent(records, "cloud.move.batch"); event == nil || event["outcome"] != "uncertain" {
					t.Fatalf("uncertain acknowledgement was hidden: %v", records)
				}
				if fileBusinessEvent(records, "cloud.move.position") != nil {
					t.Fatal("unconfirmed source was shown as moved")
				}
				if fixture.driver.items[fixture.sourceID].ParentID != "source-root" || fixture.driver.moveCalls != 0 {
					t.Fatal("uncertain fixture changed source")
				}
			}
			if fixture.driver.moveBatchCalls != 1 || fileBusinessEvent(records, "cloud.file.verified") != nil {
				t.Fatal("logging retried provider or manufactured file completion")
			}
			for _, secret := range []string{"private-cookie", "private-key", "upstream.invalid", "raw-body", fixture.sourceID, "library-root", "source-storage-root"} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("private provider fact leaked: %q", secret)
				}
			}
		})
	}
}

func TestFileBusinessLoggingDoesNotAddProviderCalls(t *testing.T) {
	type calls struct{ list, stat, move, moveBatch, copy, copyBatch, rename, recycle int }
	var baseline calls
	for _, enabled := range []bool{false, true} {
		fixture := newCloudTransferFixture(t, models.MediaLibraryTransferMove, models.MediaLibraryConflictOverwrite, false)
		var output strings.Builder
		if enabled {
			fixture.service.log = zerolog.New(&output)
		}
		result := fixture.run(t)
		if result.ErrorCode != "" || result.RetryAt != nil {
			t.Fatalf("result=%+v", result)
		}
		d := fixture.driver
		got := calls{d.listCalls, d.statCalls, d.moveCalls, d.moveBatchCalls, d.copyCalls, d.copyBatchCalls, d.renameCalls, d.recycleBatchCalls}
		if !enabled {
			baseline = got
			continue
		}
		if got != baseline {
			t.Fatalf("logging changed provider calls: got=%+v baseline=%+v", got, baseline)
		}
		if event := fileBusinessEvent(fileBusinessRecords(t, output.String()), "cloud.file.verified"); event == nil || event["source_relative_path"] != "Movie.2024.mkv" || event["target_relative_path"] != "电影/Movie (2024)/Movie (2024).mkv" {
			t.Fatalf("actual file display absent: %v", event)
		}
	}
}

func TestFileBusinessCloudSkippedFilesKeepExactBoundedSummary(t *testing.T) {
	for _, mode := range []string{models.MediaLibraryTransferMove, models.MediaLibraryTransferCopy} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCloudTransferFixture(t, mode, models.MediaLibraryConflictSkip, true)
			var output strings.Builder
			fixture.service.log = zerolog.New(&output)
			for index := 0; index < 25; index++ {
				sourceID, targetID := fmt.Sprintf("subtitle-source-%d", index), fmt.Sprintf("subtitle-target-%d", index)
				sourceName, targetName := fmt.Sprintf("Movie.2024.lang%02d.srt", index), fmt.Sprintf("Movie (2024).lang%02d.srt", index)
				fixture.driver.items[sourceID] = cloudpkg.Item{ID: sourceID, ParentID: "source-root", Name: sourceName, Size: 1, SHA1: "SOURCE-SUBTITLE"}
				fixture.driver.items[targetID] = cloudpkg.Item{ID: targetID, ParentID: "movie-dir", Name: targetName, Size: 2, SHA1: "EXISTING-SUBTITLE"}
				fixture.manifest.Files = append(fixture.manifest.Files, downloadpkg.File{RelativePath: sourceName, Size: 1, ProviderItemID: sourceID, ProviderParentID: "source-root", SHA1: "SOURCE-SUBTITLE"})
			}
			if result := fixture.run(t); result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
				t.Fatalf("result=%+v", result)
			}
			records := fileBusinessRecords(t, output.String())
			summary := fileBusinessEvent(records, "cloud.file.summary")
			samples := 0
			for _, event := range records {
				if event["event_code"] == "cloud.file.skipped" {
					samples++
				}
			}
			if summary == nil || summary["skipped_count"] != float64(26) || summary["total_count"] != float64(26) || summary["sample_count"] != float64(samples) || summary["omitted_count"] != float64(26-samples) || samples > 20 {
				t.Fatalf("skip count/sample/omission mismatch: samples=%d summary=%v", samples, summary)
			}
			if fixture.driver.moveCalls+fixture.driver.moveBatchCalls+fixture.driver.copyCalls+fixture.driver.copyBatchCalls+fixture.driver.renameCalls != 0 {
				t.Fatal("skipped files were moved, copied or renamed")
			}
			// The existing copy executor may recycle its empty temporary
			// directory. The skipped source and conflicting target files stay.
			if fixture.driver.items[fixture.sourceID].ParentID != "source-root" || fixture.driver.items["existing-video"].ID == "" {
				t.Fatal("skip changed the primary source or target")
			}
			for index := 0; index < 25; index++ {
				if fixture.driver.items[fmt.Sprintf("subtitle-source-%d", index)].ParentID != "source-root" || fixture.driver.items[fmt.Sprintf("subtitle-target-%d", index)].ID == "" {
					t.Fatal("skip changed a subtitle source or target")
				}
			}
		})
	}
}

func TestFileBusinessVersionedReorganizationReportsPhysicalAndCatalogFactsSeparately(t *testing.T) {
	for _, cloud := range []bool{false, true} {
		t.Run(fmt.Sprint("cloud_", cloud), func(t *testing.T) {
			fixture := newVersionedReorganizationFixture(t, cloud)
			var output strings.Builder
			fixture.worker.service.log = zerolog.New(&output)
			if cloud {
				fixture.driver.renameErrOnce = true
				if result := fixture.run(); result.ErrorCode == "" {
					t.Fatal("expected original rename failure")
				}
				records := fileBusinessRecords(t, output.String())
				var accepted, partial bool
				for _, event := range records {
					if event["event_code"] != "reorganization.file" {
						continue
					}
					accepted = accepted || event["outcome"] == "accepted"
					partial = partial || event["outcome"] == "partial"
					if event["outcome"] == "confirmed" {
						t.Fatal("failed rename was presented as a confirmed final file")
					}
				}
				if !accepted || !partial || fileBusinessEvent(records, "reorganization.catalog.published") != nil {
					t.Fatalf("partial physical operation incorrectly published: %v", records)
				}
				output.Reset()
			}
			if result := fixture.run(); result.ErrorCode != "" {
				t.Fatalf("result=%+v", result)
			}
			records := fileBusinessRecords(t, output.String())
			var confirmed map[string]any
			for _, event := range records {
				if event["event_code"] == "reorganization.file" && event["outcome"] == "confirmed" {
					confirmed = event
				}
			}
			if confirmed == nil || confirmed["source_relative_path"] != fixture.plan.Items[0].OldRelativePath || confirmed["target_relative_path"] != fixture.plan.Items[0].NewRelativePath || confirmed["job_id"] != fixture.claim.Job.ID {
				t.Fatalf("physical display=%v", confirmed)
			}
			if event := fileBusinessEvent(records, "reorganization.catalog.published"); event == nil || event["outcome"] != "confirmed" {
				t.Fatalf("catalog publication=%v", event)
			}
			if strings.Contains(output.String(), fixture.storage.RootPath) {
				t.Fatal("private root leaked")
			}
			if cloud && (fixture.driver.moveCalls != 1 || fixture.driver.renameCalls != 2) {
				t.Fatal("log instrumentation repeated an already accepted provider move")
			}
		})
	}
}
