package database

import "gorm.io/gorm"

func migrateStructureReviewWorkspace(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE media_library_structure_review_sessions (id TEXT PRIMARY KEY, owner_id INTEGER NOT NULL, library_id INTEGER NOT NULL, diagnosis_job_id TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 0, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(owner_id) REFERENCES users(id) ON DELETE CASCADE, FOREIGN KEY(library_id) REFERENCES media_libraries(id) ON DELETE CASCADE, UNIQUE(owner_id,library_id,diagnosis_job_id))`,
		`CREATE INDEX idx_structure_review_session_owner ON media_library_structure_review_sessions(owner_id)`,
		`CREATE INDEX idx_structure_review_session_library ON media_library_structure_review_sessions(library_id)`,
		`CREATE INDEX idx_structure_review_current ON media_library_structure_review_sessions(library_id,diagnosis_job_id)`,
		`CREATE TABLE media_library_structure_review_choices (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, subject_key TEXT NOT NULL, subject_kind TEXT NOT NULL CHECK(subject_kind IN ('issue','recognition')), issue_token TEXT NOT NULL DEFAULT '', recognition_id INTEGER, action TEXT NOT NULL CHECK(action IN ('repair','keep_recommended','keep_member','keep_all_versions','skip','manual_recognition')), member_token TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'draft' CHECK(state IN ('draft','submitted')), created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, FOREIGN KEY(session_id) REFERENCES media_library_structure_review_sessions(id) ON DELETE CASCADE, FOREIGN KEY(recognition_id) REFERENCES media_library_recognitions(id) ON DELETE CASCADE, UNIQUE(session_id,subject_key))`,
		`CREATE INDEX idx_structure_review_choice_session ON media_library_structure_review_choices(session_id)`,
		`CREATE INDEX idx_structure_review_choice_issue ON media_library_structure_review_choices(issue_token)`,
		`CREATE INDEX idx_structure_review_choice_recognition ON media_library_structure_review_choices(recognition_id)`,
		`ALTER TABLE media_library_structure_repair_drafts ADD COLUMN review_session_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE media_library_structure_repair_drafts ADD COLUMN review_revision INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX idx_structure_repair_draft_review ON media_library_structure_repair_drafts(review_session_id)`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
