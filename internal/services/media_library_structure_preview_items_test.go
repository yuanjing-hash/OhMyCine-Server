package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructurePreviewItemsExposeExactFrozenVersionNames(t *testing.T) {
	s, actor, library, diagnosis := prepareStructureSelectionConflicts(t, 2)
	issues, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Code: "duplicate_target"})
	if err != nil {
		t.Fatal(err)
	}
	input := MediaLibraryStructureSelectionInput{Revision: diagnosis.Revision}
	for _, issue := range issues.List {
		input.Selections = append(input.Selections, MediaLibraryStructureSelection{IssueToken: issue.Token, Action: StructureSelectionKeepAllVersions})
	}
	preview, err := s.PreviewSelectionRepair(context.Background(), actor, library.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Items.Total != 4 || preview.RecycleCount != 0 {
		t.Fatalf("preview=%+v", preview)
	}
	var targets []string
	for page := 1; page <= 4; page++ {
		items, err := s.SelectionPreviewItems(context.Background(), actor, library.ID, preview.ConfirmationToken, page, 1)
		if err != nil || len(items.List) != 1 || items.Total != 4 {
			t.Fatalf("page=%+v err=%v", items, err)
		}
		targets = append(targets, items.List[0].ExpectedPath)
	}
	if len(targets) != 4 || !strings.Contains(strings.Join(targets, "\n"), "(2).mkv") {
		t.Fatalf("missing numbered final version: %v", targets)
	}
	raw, err := json.Marshal(preview.Items)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"provider_id", "primary-", "copy-", "rule_fingerprint", "plan_hash", "source_revision"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("preview leaked %s", secret)
		}
	}
	var count int64
	if err := s.db.Model(&models.MediaLibraryStructureRepair{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("preview enqueued mutation count=%d err=%v", count, err)
	}
	other := actor
	other.User.ID++
	if _, err := s.SelectionPreviewItems(context.Background(), other, library.ID, preview.ConfirmationToken, 1, 50); err == nil {
		t.Fatal("foreign actor read preview")
	}
	if _, err := s.SelectionPreviewItems(context.Background(), actor, library.ID+1, preview.ConfirmationToken, 1, 50); err == nil {
		t.Fatal("foreign library read preview")
	}
	if _, err := s.SelectionPreviewItems(context.Background(), actor, library.ID, preview.ConfirmationToken, 0, 50); err == nil {
		t.Fatal("invalid page accepted")
	}
	if err := s.db.Model(&models.MediaLibraryStructureRepairDraft{}).Where("library_id = ?", library.ID).Update("consumed_at", time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectionPreviewItems(context.Background(), actor, library.ID, preview.ConfirmationToken, 1, 50); err == nil {
		t.Fatal("consumed preview readable")
	}
}

func TestRecognitionDirectReadIsLibraryScoped(t *testing.T) {
	structure, actor, library := structureConfirmationFixture(t)
	s := &MediaLibraryService{db: structure.db}
	record := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "manual-target", ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, Status: mediaRecognitionStatusUnrecognized, Title: "待识别作品"}
	if err := s.db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	item, err := s.Recognition(context.Background(), actor, library.ID, encodeRecognitionToken(record.ID))
	if err != nil || item.Title != record.Title || item.Token != encodeRecognitionToken(record.ID) {
		t.Fatalf("item=%+v err=%v", item, err)
	}
	if _, err := s.Recognition(context.Background(), actor, library.ID+1, encodeRecognitionToken(record.ID)); err == nil {
		t.Fatal("foreign library accepted")
	}
	delete(actor.Permissions, authz.PermissionMediaLibrariesRead)
	if _, err := s.Recognition(context.Background(), actor, library.ID, encodeRecognitionToken(record.ID)); err == nil {
		t.Fatal("unauthorized direct read")
	}
}

func TestStructurePreviewItemsRejectStaleSnapshots(t *testing.T) {
	s, actor, library, diagnosis := prepareStructureSelectionConflicts(t, 1)
	issues, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Code: "duplicate_target"})
	if err != nil || len(issues.List) != 1 {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
	preview, err := s.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnosis.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: issues.List[0].Token, Action: StructureSelectionKeepAllVersions}}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.verifyStructureClaim(preview.ConfirmationToken)
	if err != nil {
		t.Fatal(err)
	}
	var draft models.MediaLibraryStructureRepairDraft
	if err := s.db.First(&draft, "id = ?", claim.DraftID).Error; err != nil {
		t.Fatal(err)
	}
	replacement := enqueueFake(t, s.queue, actor, "Replacement diagnosis", "replacement-preview-diagnosis")
	tests := []struct {
		name, table, key, column string
		value, original          any
	}{
		{"new_scan", "media_libraries", "id", "baseline_generation", draft.Generation + 1, draft.Generation},
		{"new_source", "media_library_structure_auto_states", "library_id", "source_revision", draft.SourceRevision + 1, draft.SourceRevision},
		{"new_diagnosis", "media_library_structure_diagnoses", "library_id", "job_id", replacement.ID, draft.DiagnosisJobID},
		{"expired", "media_library_structure_repair_drafts", "id", "expires_at", time.Now().UTC().Add(-time.Hour), draft.ExpiresAt},
		{"legacy_manifest", "media_library_structure_repair_drafts", "id", "preview_items_json", "", draft.PreviewItemsJSON},
		{"corrupt_manifest", "media_library_structure_repair_drafts", "id", "preview_items_json", "{", draft.PreviewItemsJSON},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var key any = library.ID
			if tt.table == "media_library_structure_repair_drafts" {
				key = draft.ID
			}
			query := s.db.Table(tt.table).Where(tt.key+" = ?", key)
			if err := query.Update(tt.column, tt.value).Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.db.Table(tt.table).Where(tt.key+" = ?", key).Update(tt.column, tt.original).Error; err != nil {
					t.Error(err)
				}
			})
			if _, err := s.SelectionPreviewItems(context.Background(), actor, library.ID, preview.ConfirmationToken, 1, 50); err == nil {
				t.Fatal("stale or corrupt preview accepted")
			}
		})
	}
	page, err := s.SelectionPreviewItems(context.Background(), actor, library.ID, preview.ConfirmationToken, 100000, 200)
	if err != nil || len(page.List) != 0 || page.Total != 2 {
		t.Fatalf("out-of-range page=%+v err=%v", page, err)
	}
}

func TestStructurePreviewIncludesUnmovedWinner(t *testing.T) {
	items, err := structurePreviewItems(StructurePlan{Items: []StructurePlanItem{{Kind: "video", SourceRelative: "/Movie (1).mkv", TargetRelative: "/Movie (2).mkv"}}}, []structureSelectionResolved{{
		issue:     models.MediaLibraryStructureIssue{Kind: "video"},
		selection: MediaLibraryStructureSelection{Action: StructureSelectionKeepAllVersions},
		members:   []models.MediaLibraryStructureIssueMember{{SourcePath: "/Movie.mkv"}, {SourcePath: "/Movie (1).mkv"}},
	}})
	if err != nil || len(items) != 2 || items[1].Action != "keep" || items[1].CurrentPath != items[1].ExpectedPath || items[1].ExpectedPath != "Movie.mkv" {
		t.Fatalf("unmoved winner missing: %+v err=%v", items, err)
	}
}
