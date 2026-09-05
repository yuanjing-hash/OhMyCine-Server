package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestStructureSelectionReviewConflictsRequireIdentityOrIndexReview(t *testing.T) {
	for _, code := range []string{"catalog_duplicate_conflict", "recognition_suspect_conflict"} {
		t.Run(code, func(t *testing.T) {
			service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
			var issue models.MediaLibraryStructureIssue
			if err := service.db.Where("library_id = ?", library.ID).First(&issue).Error; err != nil {
				t.Fatal(err)
			}
			if err := service.db.Model(&issue).Update("code", code).Error; err != nil {
				t.Fatal(err)
			}
			legacyPlan := StructurePlan{LibraryID: library.ID, DiagnosisJobID: issue.DiagnosisJobID, ResolvedIssues: []string{issue.Token}}
			if err := service.validateStructureSelectionSafety(context.Background(), legacyPlan); ErrorCode(err) != CodeConflict {
				t.Fatalf("legacy plan could resolve review-only conflict: %v", err)
			}
			for _, action := range []string{StructureSelectionRepair, StructureSelectionKeepRecommended, StructureSelectionKeepMember, StructureSelectionKeepAllVersions} {
				_, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: issue.Token, Action: action, MemberToken: issue.RecommendedMemberToken}}})
				if ErrorCode(err) != CodeConflict {
					t.Fatalf("review-only %s accepted %s: %v", code, action, err)
				}
				if code == "catalog_duplicate_conflict" && !strings.Contains(err.Error(), "索引") || code == "recognition_suspect_conflict" && !strings.Contains(err.Error(), "修正识别") {
					t.Fatalf("review guidance is missing: %v", err)
				}
			}
			preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, BulkActions: []MediaLibraryStructureBulkAction{{Codes: []string{code}, Action: StructureSelectionKeepRecommended}}})
			if err != nil || preview.SkippedCount != 1 || preview.MoveCount != 0 || preview.RecycleCount != 0 || len(preview.Selections) != 1 || preview.Selections[0].Action != StructureSelectionSkip {
				t.Fatalf("bulk recommendation must skip review-only conflict: %+v err=%v", preview, err)
			}
			preview, err = service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: issue.Token, Action: StructureSelectionSkip}}})
			if err != nil || preview.SkippedCount != 1 {
				t.Fatalf("explicit skip failed: %+v err=%v", preview, err)
			}
		})
	}
}

func prepareStructureDuplicateProviderFacts(t *testing.T) (*MediaLibraryStructureService, Actor, models.MediaLibrary, *structureCloudDriver) {
	t.Helper()
	service, actor, library, _ := prepareStructureSelectionConflicts(t, 1)
	connection := models.Connection{Name: "Selection safety", NameNormalized: "selection-safety", Provider: cloudpkg.ProviderPan115, CredentialCiphertext: "test-only", Enabled: true}
	if err := service.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.Storage{}).Where("id = ?", library.StorageID).Updates(map[string]any{"type": models.StorageTypePan115, "root_path": "root", "connection_id": connection.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&library).Update("provider_root_id", "root").Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.First(&library, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Updates(map[string]any{"provider_id": "same-video", "size": 7}).Error; err != nil {
		t.Fatal(err)
	}
	driver := &structureCloudDriver{items: map[string]cloudpkg.Item{
		"root":       {ID: "root", IsDir: true},
		"incoming":   {ID: "incoming", ParentID: "root", Name: "incoming", IsDir: true},
		"same-video": {ID: "same-video", ParentID: "incoming", Name: "冲突影片 (1).mkv", Size: 7},
	}}
	service.backends.Register(pan115MediaLibraryStructureBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }})
	return service, actor, library, driver
}

func TestStructureSelectionDuplicateProviderFactsNeverRecycleRealFile(t *testing.T) {
	service, actor, library, driver := prepareStructureDuplicateProviderFacts(t)
	if err := service.EnqueueDiagnosis(context.Background(), library.ID, 0, library.BaselineGeneration, "manual"); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
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
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50, Actionable: true})
	if err != nil || page.Total != 1 || len(page.List) != 1 || page.List[0].Code != "catalog_duplicate_conflict" {
		t.Fatalf("duplicate facts were not classified for index review: %+v err=%v", page, err)
	}
	for _, action := range []string{StructureSelectionKeepRecommended, StructureSelectionKeepMember, StructureSelectionKeepAllVersions} {
		_, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: action, MemberToken: page.List[0].RecommendedMemberToken}}})
		if ErrorCode(err) != CodeConflict {
			t.Fatalf("same physical provider file accepted %s: %v", action, err)
		}
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, BulkActions: []MediaLibraryStructureBulkAction{{Codes: []string{"catalog_duplicate_conflict"}, Action: StructureSelectionKeepRecommended}}})
	if err != nil || preview.SkippedCount != 1 || preview.RecycleCount != 0 || preview.MoveCount != 0 {
		t.Fatalf("duplicate facts bulk preview=%+v err=%v", preview, err)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if result := service.runRepair(context.Background(), fastScanTestRuntime{}, repair.ID); result.ErrorCode != "" {
		t.Fatalf("skip repair failed: %+v", result)
	}
	assertStructureProviderFilePreserved(t, driver)
}

func TestStructureSelectionWorkerRejectsLegacyDuplicateProviderPlans(t *testing.T) {
	for _, scenario := range []string{"recycle-and-move-same-id", "canonical-winner-not-in-moves", "duplicate-recycle", "canonical-winner-version"} {
		t.Run(scenario, func(t *testing.T) {
			service, actor, library, driver := prepareStructureDuplicateProviderFacts(t)
			plan := StructurePlan{Version: 1, LibraryID: library.ID, Generation: library.BaselineGeneration, RuleFingerprint: libraryRuleFingerprint(library)}
			loser := StructureRecycleItem{Kind: "video", ProviderID: "same-video", SourceRelative: "incoming/冲突影片 (1).mkv", Size: 7}
			winner := StructurePlanItem{Kind: "video", ProviderID: "same-video", SourceRelative: "incoming/冲突影片.mkv", TargetRelative: "电影/冲突影片 (2024)/冲突影片 (2024).mkv", Size: 7}
			switch scenario {
			case "recycle-and-move-same-id":
				plan.RecycleItems, plan.Items = []StructureRecycleItem{loser}, []StructurePlanItem{winner}
			case "canonical-winner-not-in-moves":
				plan.RecycleItems = []StructureRecycleItem{loser}
			case "duplicate-recycle":
				second := loser
				second.SourceRelative = "incoming/冲突影片.mkv"
				plan.RecycleItems = []StructureRecycleItem{loser, second}
			case "canonical-winner-version":
				winner.SourceRelative = loser.SourceRelative
				winner.TargetRelative = "电影/冲突影片 (2024)/冲突影片 (2024) (2).mkv"
				plan.Items = []StructurePlanItem{winner}
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			repair := models.MediaLibraryStructureRepair{ID: uuid.NewString(), OwnerID: actor.User.ID, LibraryID: library.ID, Scope: models.MediaLibraryStructureScopeFull, Generation: plan.Generation, RuleFingerprint: plan.RuleFingerprint, PlanJSON: string(raw), StateJSON: `{}`, Phase: "queued", CreatedAt: now, UpdatedAt: now}
			if err := service.db.Create(&repair).Error; err != nil {
				t.Fatal(err)
			}
			if result := service.runRepair(context.Background(), fastScanTestRuntime{}, repair.ID); result.ErrorCode == "" {
				t.Fatal("unsafe legacy plan was executed")
			}
			assertStructureProviderFilePreserved(t, driver)
			var count int64
			if err := service.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != 2 {
				t.Fatalf("blocked plan changed catalog facts: count=%d err=%v", count, err)
			}
		})
	}
}

func assertStructureProviderFilePreserved(t *testing.T, driver *structureCloudDriver) {
	t.Helper()
	item, err := driver.Stat(context.Background(), "same-video")
	if err != nil || item.Name != "冲突影片 (1).mkv" || item.ParentID != "incoming" || item.Size != 7 || len(driver.items) != 3 {
		t.Fatalf("real provider file was changed: %+v err=%v items=%+v", item, err, driver.items)
	}
}

func TestStructureSelectionRejectsPartialDuplicateIdentitiesInPhysicalConflict(t *testing.T) {
	// A mixed A/A/B group is labelled duplicate_target, not catalog_duplicate;
	// every member must still have its own physical identity before selection.
	for _, action := range []string{StructureSelectionKeepRecommended, StructureSelectionKeepAllVersions} {
		issue := models.MediaLibraryStructureIssue{Token: "issue", Code: "duplicate_target", ExpectedPath: "target.mkv", ConflictSourceCount: 3, RecommendedMemberToken: "a"}
		members := []models.MediaLibraryStructureIssueMember{{Token: "a", SourcePath: "a.mkv"}, {Token: "a-copy", SourcePath: "a (1).mkv"}, {Token: "b", SourcePath: "b.mkv"}}
		base := StructurePlan{ConflictGroups: []StructureConflictGroup{{Code: issue.Code, TargetRelative: issue.ExpectedPath, Members: []StructurePlanItem{{SourceRelative: "a.mkv", ProviderID: "a"}, {SourceRelative: "a (1).mkv", ProviderID: "a"}, {SourceRelative: "b.mkv", ProviderID: "b"}}}}}
		var plan StructurePlan
		err := appendStructureSelection(&plan, base, issue, members, MediaLibraryStructureSelection{Action: action, MemberToken: "a"}, "draft")
		if ErrorCode(err) != CodeConflict || len(plan.RecycleItems) != 0 || len(plan.Items) != 0 {
			t.Fatalf("mixed duplicate identities accepted %s: %+v err=%v", action, plan, err)
		}
	}
}

func TestStructureSelectionSafetyIncludesActiveCompanionFacts(t *testing.T) {
	service, _, library, _ := prepareStructureSelectionConflicts(t, 1)
	assets := []models.MediaLibrarySourceAsset{
		{LibraryID: library.ID, ProviderID: "same-caption", RelativePath: "/incoming/字幕.srt", Name: "字幕.srt", Extension: ".srt", Active: true},
		{LibraryID: library.ID, ProviderID: "same-caption", RelativePath: "/incoming/字幕 (1).srt", Name: "字幕 (1).srt", Extension: ".srt", Active: true},
	}
	if err := service.db.Create(&assets).Error; err != nil {
		t.Fatal(err)
	}
	plan := StructurePlan{LibraryID: library.ID, RecycleItems: []StructureRecycleItem{{Kind: "sidecar", SourceRelative: "incoming/字幕 (1).srt", ProviderID: "same-caption"}}}
	if err := service.validateStructureSelectionSafety(context.Background(), plan); ErrorCode(err) != CodeConflict {
		t.Fatalf("canonical caption omitted from moves was not protected: %v", err)
	}
	unrelated := StructurePlan{LibraryID: library.ID, Items: []StructurePlanItem{{Kind: "video", SourceRelative: "incoming/冲突影片.mkv", ProviderID: "primary-冲突影片"}}}
	if err := service.validateStructureSelectionSafety(context.Background(), unrelated); err != nil {
		t.Fatalf("unrelated companion conflict blocked a distinct file: %v", err)
	}
	if err := service.db.Model(&assets[0]).Update("active", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.validateStructureSelectionSafety(context.Background(), plan); err != nil {
		t.Fatalf("inactive companion fact was treated as another file: %v", err)
	}
}
