package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Separate point lookups let SQLite use the (session_id, subject_key) index for
// both identities. An OR inside one correlated subquery scans every choice in
// the session for every issue, making a large skip-all workspace quadratic.
const structureReviewChoiceExistsSQL = "(EXISTS (SELECT 1 FROM media_library_structure_review_choices rc WHERE rc.session_id = ? AND rc.subject_key = ('issue:' || media_library_structure_issues.token)) OR (media_library_structure_issues.recognition_id IS NOT NULL AND EXISTS (SELECT 1 FROM media_library_structure_review_choices rc WHERE rc.session_id = ? AND rc.subject_key = ('recognition:' || CAST(media_library_structure_issues.recognition_id AS TEXT)))))"

// Summary counts belong to the actor's complete current diagnosis, independently
// of pagination/category/review filters. Use the same source/snapshot fence as
// the issue list; never infer a healthy zero from a failed read.
func populateStructureReviewSummaryTx(tx *gorm.DB, libraryID uint, sessionID string, result *MediaLibraryStructureIssuePage) error {
	var library models.MediaLibrary
	if err := tx.First(&library, libraryID).Error; err != nil {
		return err
	}
	result.DiagnosisRevision = structureDiagnosticRevision(library)
	base, err := currentStructureIssueScopeTx(tx, tx.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ? AND code <> ?", libraryID, "missing_season_episode"), libraryID)
	if err != nil {
		return err
	}
	var groups []struct {
		Code       string
		Handled    bool
		Repairable bool
		Total      int64
	}
	if err := base.Select("code, "+structureReviewChoiceExistsSQL+" AS handled, (repairable OR (state = 'manual_identity_resolved' AND conflict_source_count <= 1)) AS repairable, COUNT(*) AS total", sessionID, sessionID).
		Group("code, handled, (repairable OR (state = 'manual_identity_resolved' AND conflict_source_count <= 1))").Scan(&groups).Error; err != nil {
		return err
	}
	for _, group := range groups {
		classes := &result.PendingClassifications
		if group.Handled {
			result.HandledTotal += group.Total
			classes = &result.HandledClassifications
		} else {
			result.PendingTotal += group.Total
			if group.Repairable {
				result.PendingRepairableCount += group.Total
			}
		}
		addStructureReviewClassification(classes, group.Code, int(group.Total))
	}
	return nil
}

func addStructureReviewClassification(classes *StructureIssueClassifications, code string, count int) {
	switch code {
	case "media_unrecognized":
		classes.Unrecognized += count
	case "naming_mismatch":
		classes.NamingMismatch += count
	case "location_mismatch":
		classes.LocationMismatch += count
	case "invalid_path":
		classes.InvalidPath += count
	case "template_unavailable":
		classes.TemplateError += count
	case "duplicate_target":
		classes.DuplicateTarget += count
	case "recognition_suspect_conflict":
		classes.RecognitionSuspectConflict += count
	case "catalog_duplicate_conflict":
		classes.CatalogDuplicateConflict += count
	case "sidecar_target_conflict":
		classes.SidecarConflict += count
	}
}
