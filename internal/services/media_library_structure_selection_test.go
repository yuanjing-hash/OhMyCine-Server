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
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type structureNoRecycleBackend struct{}

func (structureNoRecycleBackend) StorageType() string { return models.StorageTypeLocal }
func (structureNoRecycleBackend) ValidateRecycle(context.Context, StructureBoundary) error {
	return errors.New("recoverable recycle unavailable")
}
func (structureNoRecycleBackend) Recycle(context.Context, StructureBoundary, []StructureRecycleItem, StructureProgress) error {
	return errors.New("must not execute")
}
func (structureNoRecycleBackend) Apply(context.Context, StructureBoundary, []StructurePlanItem, StructureProgress) error {
	return nil
}

func prepareStructureSelectionConflicts(t *testing.T, groupCount int) (*MediaLibraryStructureService, Actor, models.MediaLibrary, MediaLibraryStructureDiagnostics) {
	t.Helper()
	service, actor, library := structureConfirmationFixture(t)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{
		"movie_directory_template": "电影/{title} ({year})",
		"movie_filename_template":  "{title} ({year})",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, Status: "pending", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	year := 2024
	for index := 0; index < groupCount; index++ {
		title := "冲突影片"
		if groupCount > 1 {
			title += string(rune('A' + index))
		}
		tmdbID := int64(9000 + index)
		first := filepath.Join(storage.RootPath, "incoming", title+".mkv")
		copyPath := filepath.Join(storage.RootPath, "incoming", title+" (1).mkv")
		if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(first, []byte("primary"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath, []byte("copy"), 0o644); err != nil {
			t.Fatal(err)
		}
		firstInfo, err := os.Stat(first)
		if err != nil {
			t.Fatal(err)
		}
		copyInfo, err := os.Stat(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		entries := []models.MediaLibraryEntry{
			{LibraryID: library.ID, RelativePath: "/incoming/" + title + ".mkv", ProviderID: "primary-" + title, MediaType: "movie", Title: title, WorkKey: "movie:tmdb:" + uintID(uint(tmdbID)), MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, Size: 7, ModifiedAt: firstInfo.ModTime().UTC()},
			{LibraryID: library.ID, RelativePath: "/incoming/" + title + " (1).mkv", ProviderID: "copy-" + title, MediaType: "movie", Title: title, WorkKey: "movie:tmdb:" + uintID(uint(tmdbID)), MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, Size: 4, ModifiedAt: copyInfo.ModTime().UTC()},
		}
		if err := service.db.Create(&entries).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, 0, library.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim diagnosis=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("diagnosis worker=%+v", result)
	}
	if err := service.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := service.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	return service, actor, library, diagnostics
}

func TestStructureSelectionKeepsRecommendedAndRecyclesLoser(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true})
	if err != nil || page.Total != 1 || len(page.List) != 1 {
		t.Fatalf("issues=%+v err=%v", page, err)
	}
	issue := page.List[0]
	if issue.RecommendedMemberToken == "" || len(issue.Members) != 2 {
		t.Fatalf("conflict recommendation missing: %+v", issue)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: issue.Token, Action: StructureSelectionKeepRecommended}}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.IssueCount != 1 || preview.MoveCount != 1 || preview.RecycleCount != 1 || preview.SkippedCount != 0 || preview.Selections[0].MemberToken != issue.RecommendedMemberToken {
		t.Fatalf("preview=%+v", preview)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var proof models.CatalogPhysicalWrite
	if err := service.db.Where("owner_kind=? AND owner_id=?", CatalogPhysicalRepair, repair.ID).First(&proof).Error; err != nil || proof.State != "admitted" || repair.JobID == nil || proof.JobID != *repair.JobID {
		t.Fatalf("selection repair did not register fresh physical owner: %+v err=%v", proof, err)
	}
	if err := service.db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, library.ID) }); err != nil {
		t.Fatalf("unentered selection repair blocked drain: %v", err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claimed == nil || repair.JobID == nil || claimed.Job.ID != *repair.JobID {
		t.Fatalf("claim repair=%+v record=%+v err=%v", claimed, repair, err)
	}
	if result := NewMediaLibraryRepairWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("repair worker=%+v", result)
	}
	if err := service.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := service.db.First(&proof, proof.ID).Error; err != nil || proof.State != "settled" {
		t.Fatalf("selection repair left unsettled physical evidence: %+v err=%v", proof, err)
	}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(storage.RootPath, "电影", "冲突影片 (2024)", "冲突影片 (2024).mkv")
	data, err := os.ReadFile(canonical)
	if err != nil || string(data) != "primary" {
		t.Fatalf("canonical=%q err=%v", data, err)
	}
	recycled := false
	_ = filepath.WalkDir(filepath.Join(storage.RootPath, ".ohmycine-recycle"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && strings.HasSuffix(filepath.ToSlash(path), "/incoming/冲突影片 (1).mkv") {
			recycled = true
		}
		return nil
	})
	if !recycled {
		t.Fatal("losing source was not moved to recoverable local recycle directory")
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("one-time confirmation was replayable: %v", err)
	}
	remaining, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil || remaining.Total != 0 {
		t.Fatalf("resolved issue remained visible: %+v err=%v", remaining, err)
	}
}

func TestStructureSelectionAdmissionRefusesRetirementWithoutConsumingDraft(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true})
	if err != nil || len(page.List) != 1 {
		t.Fatalf("issues=%+v err=%v", page, err)
	}
	input := MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepAllVersions}}}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := service.verifyStructureClaim(preview.ConfirmationToken)
	if err != nil {
		t.Fatal(err)
	}
	var draft models.MediaLibraryStructureRepairDraft
	if err := service.db.First(&draft, "id=?", claim.DraftID).Error; err != nil {
		t.Fatal(err)
	}
	plan, _, _, err := service.buildSelectionPlan(context.Background(), library.ID, input, draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Retirement commits after preview construction but before queue admission.
	if err := service.db.Create(&models.MediaLibraryRetirement{ID: "selection-retirement", LibraryID: library.ID, ActorID: actor.User.ID, Phase: "draining", Revision: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.enqueueSelectionPlan(actor, draft, plan, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("retiring library accepted selection repair: %v", err)
	}
	if err := service.db.First(&draft, "id=?", draft.ID).Error; err != nil || draft.ConsumedAt != nil {
		t.Fatalf("failed admission consumed confirmation: %+v err=%v", draft, err)
	}
	for _, model := range []any{&models.MediaLibraryStructureRepair{}, &models.CatalogPhysicalWrite{}} {
		var count int64
		if err := service.db.Model(model).Where("library_id=?", library.ID).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("failed admission left owner/proof count=%d err=%v", count, err)
		}
	}
	var jobs int64
	if err := service.db.Model(&models.Job{}).Where("job_type=?", JobTypeMediaLibraryRepair).Count(&jobs).Error; err != nil || jobs != 0 {
		t.Fatalf("failed admission left repair job count=%d err=%v", jobs, err)
	}
}

func TestStructureSelectionWorkerRejectsSameSizeLocalReplacement(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}})
	if err != nil {
		t.Fatal(err)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claimed == nil || repair.JobID == nil || claimed.Job.ID != *repair.JobID {
		t.Fatalf("claim repair=%+v err=%v", claimed, err)
	}
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	loser := filepath.Join(storage.RootPath, "incoming", "冲突影片 (1).mkv")
	if err := os.WriteFile(loser, []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	changedAt := time.Now().UTC().Add(2 * time.Second)
	if err := os.Chtimes(loser, changedAt, changedAt); err != nil {
		t.Fatal(err)
	}
	if result := NewMediaLibraryRepairWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode == "" {
		t.Fatal("same-size replacement was recycled under a stale plan")
	}
	if data, err := os.ReadFile(loser); err != nil || string(data) != "evil" {
		t.Fatalf("replacement changed: %q err=%v", data, err)
	}
}

func TestStructureSelectionBulkCoversAllPagesAndExplicitSelectionWins(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 3)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 1, Actionable: true})
	if err != nil || page.Total != 3 || len(page.List) != 1 {
		t.Fatalf("paged issues=%+v err=%v", page, err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{
		Revision:    diagnostics.Revision,
		BulkActions: []MediaLibraryStructureBulkAction{{Codes: []string{"duplicate_target"}, Action: StructureSelectionKeepRecommended}},
		Selections:  []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionSkip}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.IssueCount != 3 || preview.SkippedCount != 1 || preview.RecycleCount != 2 || preview.MoveCount != 2 {
		t.Fatalf("bulk preview did not cover authoritative result set: %+v", preview)
	}
}

func TestStructureSelectionAutomaticallyIncludesRepairableVideoAndSidecar(t *testing.T) {
	service, actor, library := structureConfirmationFixture(t)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{
		"movie_directory_template": "电影/{title} ({year})",
		"movie_filename_template":  "{title} ({year})",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: library.ID, SourceRevision: 1, Status: "pending", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	year, tmdbID := 1995, int64(123)
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/旧目录/影片.mkv", ProviderID: "video", MediaType: "movie", Title: "影片", WorkKey: "movie:tmdb:123", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, LastGeneration: library.BaselineGeneration},
		{LibraryID: library.ID, RelativePath: "/未知/unknown.mkv", ProviderID: "unknown-video", MatchStatus: mediaRecognitionStatusUnrecognized, LastGeneration: library.BaselineGeneration},
	}
	asset := models.MediaLibrarySourceAsset{LibraryID: library.ID, Generation: library.BaselineGeneration, ProviderID: "poster", ParentProviderID: "old-directory", RelativePath: "/旧目录/poster.jpg", Name: "poster.jpg", Extension: ".jpg", Size: 10, Active: true}
	if err := service.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, 0, library.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim diagnosis=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("diagnosis=%+v", result)
	}
	if err := service.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := service.Diagnostics(context.Background(), actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.IssueCount != 3 || diagnostics.RepairableCount != 2 || diagnostics.Unrecognized != 1 {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, IncludeAutomaticRepairs: true})
	if err != nil {
		t.Fatal(err)
	}
	if preview.IssueCount != 2 || preview.MoveCount != 2 || preview.RecycleCount != 0 || preview.SkippedCount != 0 {
		t.Fatalf("automatic preview=%+v", preview)
	}
	kinds := map[string]bool{}
	for _, item := range preview.Items.List {
		kinds[item.Kind] = true
	}
	if !kinds["video"] || !kinds["sidecar"] {
		t.Fatalf("automatic preview omitted video or sidecar: %+v", preview.Items.List)
	}
	claim, err := service.verifyStructureClaim(preview.ConfirmationToken)
	if err != nil {
		t.Fatal(err)
	}
	var draft models.MediaLibraryStructureRepairDraft
	if err := service.db.First(&draft, "id = ?", claim.DraftID).Error; err != nil {
		t.Fatal(err)
	}
	var frozenIntent MediaLibraryStructureSelectionInput
	if err := json.Unmarshal([]byte(draft.SelectionsJSON), &frozenIntent); err != nil || !frozenIntent.IncludeAutomaticRepairs || len(frozenIntent.Selections) != 0 {
		t.Fatalf("automatic preview did not preserve compact intent: %+v err=%v", frozenIntent, err)
	}
	if draft.PreviewItemsJSON != structurePreviewRowsMarker {
		t.Fatalf("preview was not stored relationally: %q", draft.PreviewItemsJSON)
	}
	var previewRows int64
	if err := service.db.Model(&models.MediaLibraryStructureRepairDraftPreviewItem{}).Where("draft_id = ?", draft.ID).Count(&previewRows).Error; err != nil || previewRows != int64(preview.Items.Total) {
		t.Fatalf("relational preview rows=%d total=%d err=%v", previewRows, preview.Items.Total, err)
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); err != nil {
		t.Fatalf("automatic preview could not cross the confirmation boundary: %v", err)
	}
}

func TestStructureSelectionMoveOrderingMatchesStableDependencySemantics(t *testing.T) {
	items := []StructurePlanItem{
		{SourceRelative: "a.mkv", TargetRelative: "b.mkv"},
		{SourceRelative: "c.mkv", TargetRelative: "free-c.mkv"},
		{SourceRelative: "b.mkv", TargetRelative: "free-b.mkv"},
		{SourceRelative: "d.mkv", TargetRelative: "e.mkv"},
		{SourceRelative: "e.mkv", TargetRelative: "free-e.mkv"},
	}
	ordered := orderStructureSelectionMoves(items)
	want := []string{"c.mkv", "b.mkv", "a.mkv", "e.mkv", "d.mkv"}
	if len(ordered) != len(want) {
		t.Fatalf("ordered %d moves, want %d", len(ordered), len(want))
	}
	for index, source := range want {
		if ordered[index].SourceRelative != source {
			t.Fatalf("order[%d]=%q want %q; full=%+v", index, ordered[index].SourceRelative, source, ordered)
		}
	}
}

func TestStructureSelectionMoveOrderingPreservesSortedCycleFallback(t *testing.T) {
	items := []StructurePlanItem{
		{SourceRelative: "z.mkv", TargetRelative: "free.mkv"},
		{SourceRelative: "b.mkv", TargetRelative: "a.mkv"},
		{SourceRelative: "a.mkv", TargetRelative: "b.mkv"},
	}
	ordered := orderStructureSelectionMoves(items)
	want := []string{"z.mkv", "a.mkv", "b.mkv"}
	for index, source := range want {
		if ordered[index].SourceRelative != source {
			t.Fatalf("cycle fallback[%d]=%q want %q; full=%+v", index, ordered[index].SourceRelative, source, ordered)
		}
	}
}

func TestStructureSelectionPlanIndexPreservesUnicodeEqualFoldPathMatching(t *testing.T) {
	base := StructurePlan{
		Items:          []StructurePlanItem{{Kind: "video", SourceRelative: `Folder/ſample.mkv`, TargetRelative: `Target/Movie.mkv`}},
		ConflictGroups: []StructureConflictGroup{{Code: "duplicate_target", TargetRelative: `Target/ſpecial.mkv`}},
	}
	index := newStructureSelectionPlanIndex(base)
	if _, exists := index.items[structureSelectionItemKey("video", `folder/sample.MKV`, `target/movie.MKV`)]; !exists {
		t.Fatal("indexed item lookup no longer matches the former Unicode EqualFold semantics")
	}
	if _, exists := index.conflictGroups[structureSelectionConflictKey("duplicate_target", `target/special.MKV`)]; !exists {
		t.Fatal("indexed conflict lookup no longer matches the former Unicode EqualFold semantics")
	}
}

func TestStructureSelectionLargeAutomaticBatchPlanningCompletesWithinBound(t *testing.T) {
	const itemCount = 16773
	base := StructurePlan{Items: make([]StructurePlanItem, itemCount)}
	issues := make([]models.MediaLibraryStructureIssue, itemCount)
	for index := 0; index < itemCount; index++ {
		source := fmt.Sprintf("incoming/%05d.mkv", index)
		target := fmt.Sprintf("library/%05d.mkv", index)
		base.Items[index] = StructurePlanItem{Kind: "video", SourceRelative: source, TargetRelative: target, ProviderID: fmt.Sprintf("provider-%05d", index)}
		issues[index] = models.MediaLibraryStructureIssue{Token: fmt.Sprintf("issue-%05d", index), Kind: "video", Code: "path_mismatch", CurrentPath: source, ExpectedPath: target, Repairable: true}
	}

	started := time.Now()
	index := newStructureSelectionPlanIndex(base)
	plan := StructurePlan{}
	for _, issue := range issues {
		selection := MediaLibraryStructureSelection{IssueToken: issue.Token, Action: StructureSelectionRepair}
		if err := appendIndexedStructureSelection(&plan, base, index, issue, nil, selection, "large-draft"); err != nil {
			t.Fatal(err)
		}
	}
	plan.Items = orderStructureSelectionMoves(plan.Items)
	elapsed := time.Since(started)
	if len(plan.Items) != itemCount || len(plan.ResolvedIssues) != itemCount {
		t.Fatalf("large plan items=%d resolved=%d", len(plan.Items), len(plan.ResolvedIssues))
	}
	if elapsed > 5*time.Second {
		t.Fatalf("large automatic selection planning took %s; expected indexed bounded planning", elapsed)
	}
}

func TestStructureSelectionLargePreviewResponseOmitsDuplicateSelectionList(t *testing.T) {
	selections := make([]MediaLibraryStructureSelection, maxStructurePreviewResponseSelections+1)
	if got := structurePreviewResponseSelections(selections); got != nil {
		t.Fatalf("large response repeated %d frozen selections", len(got))
	}
	if got := structurePreviewResponseSelections(selections[:maxStructurePreviewResponseSelections]); len(got) != maxStructurePreviewResponseSelections {
		t.Fatalf("bounded compatibility response selections=%d", len(got))
	}
	preview := MediaLibraryStructureSelectionPreview{IssueCount: len(selections), Selections: structurePreviewResponseSelections(selections)}
	raw, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"selections"`) {
		t.Fatalf("large initial response still contains duplicate selections: %s", raw)
	}
}

func TestStructureSelectionRejectsStaleSourceRevision(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ?", library.ID).Update("source_revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale source revision was accepted: %v", err)
	}
}

func TestStructureSelectionConfirmationRejectsTamperActorAndExpiry(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	selection := MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken+"x", RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("tampered confirmation was accepted: %v", err)
	}
	otherActor := actor
	otherActor.User.ID++
	if _, err := service.EnqueueSelectionRepair(context.Background(), otherActor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("cross-actor confirmation was accepted: %v", err)
	}
	claim, err := service.verifyStructureClaim(preview.ConfirmationToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibraryStructureRepairDraft{}).Where("id = ?", claim.DraftID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("expired confirmation was accepted: %v", err)
	}
}

func TestStructureSelectionPreviewRejectsUnrecoverableConflictHandling(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	service.backends = NewMediaLibraryStructureBackendRegistry(structureNoRecycleBackend{})
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepRecommended}}})
	if ErrorCode(err) != CodeMediaLibraryStructureUnavailable {
		t.Fatalf("unrecoverable conflict handling was previewed: %v", err)
	}
}

func TestStructureSelectionKeepsEveryConflictMemberAsVersion(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	var storage models.Storage
	if err := service.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	occupiedRelative := "/电影/冲突影片 (2024)/冲突影片 (2024) (2).mkv"
	occupiedPath := filepath.Join(storage.RootPath, filepath.FromSlash(strings.TrimPrefix(occupiedRelative, "/")))
	if err := os.MkdirAll(filepath.Dir(occupiedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(occupiedPath, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: occupiedRelative, ProviderID: "healthy-version-two", MatchStatus: mediaRecognitionStatusPending, Size: 8}).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepAllVersions}}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.MoveCount != 2 || preview.RecycleCount != 0 {
		t.Fatalf("version preview=%+v", preview)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryRepair})
	if err != nil || claimed == nil || repair.JobID == nil || claimed.Job.ID != *repair.JobID {
		t.Fatalf("claim repair=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryRepairWorker(service).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("repair worker=%+v", result)
	}
	for _, relative := range []string{"电影/冲突影片 (2024)/冲突影片 (2024).mkv", "电影/冲突影片 (2024)/冲突影片 (2024) (3).mkv"} {
		if _, err := os.Stat(filepath.Join(storage.RootPath, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("version %s missing: %v", relative, err)
		}
	}
	if data, err := os.ReadFile(occupiedPath); err != nil || string(data) != "sentinel" {
		t.Fatalf("occupied version was modified: %q err=%v", data, err)
	}
}

func TestStructureSelectionRejectsIssueTokenFromAnotherLibrary(t *testing.T) {
	firstService, firstActor, firstLibrary, _ := prepareStructureSelectionConflicts(t, 1)
	page, err := firstService.StructureIssues(context.Background(), firstActor, firstLibrary.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	secondLibrary := firstLibrary
	secondLibrary.ID = 0
	secondLibrary.Name = "Other library"
	secondLibrary.NameNormalized = "other-library"
	secondLibrary.StructureStatus = models.MediaLibraryStructurePending
	secondLibrary.StructureCheckedAt = nil
	if err := firstService.db.Create(&secondLibrary).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstService.db.Create(&models.MediaLibraryStructureAutoState{LibraryID: secondLibrary.ID, SourceRevision: 1, Status: "pending", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := firstService.EnqueueDiagnosis(context.Background(), secondLibrary.ID, 0, secondLibrary.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	claimed, err := firstService.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim second diagnosis=%+v err=%v", claimed, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(firstService).Run(context.Background(), fastScanTestRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("second diagnosis=%+v", result)
	}
	if err := firstService.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	secondDiagnostics, err := firstService.Diagnostics(context.Background(), firstActor, secondLibrary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstService.PreviewSelectionRepair(context.Background(), firstActor, secondLibrary.ID, MediaLibraryStructureSelectionInput{Revision: secondDiagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionSkip}}}); ErrorCode(err) != CodeConflict {
		t.Fatalf("cross-library issue token was accepted: %v", err)
	}
}
