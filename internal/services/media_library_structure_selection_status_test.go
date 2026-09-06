package services

import (
	"context"
	"reflect"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureSelectionStatusPreservesOnlyCurrentDraftChoices(t *testing.T) {
	s, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 2)
	page, err := s.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 50})
	if err != nil || len(page.List) != 2 {
		t.Fatalf("issues=%+v err=%v", page, err)
	}
	one, two := page.List[0].Token, page.List[1].Token
	status, err := s.SelectionStatus(context.Background(), actor, library.ID, []string{one, two})
	if err != nil || status.Revision != diagnostics.Revision || len(status.InvalidIssueTokens) != 0 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	// An identity correction replaces only its affected issue. An unrelated
	// cross-page draft must survive; old intent must not transfer to a new token.
	if err := s.db.Model(&models.MediaLibraryStructureIssue{}).Where("token = ?", one).Update("token", "replacement-issue").Error; err != nil {
		t.Fatal(err)
	}
	status, err = s.SelectionStatus(context.Background(), actor, library.ID, []string{one, two, "foreign-token", one})
	if err != nil || !reflect.DeepEqual(status.InvalidIssueTokens, []string{one, "foreign-token"}) {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	status, err = s.SelectionStatus(context.Background(), actor, library.ID, nil)
	if err != nil || status.InvalidIssueTokens == nil || len(status.InvalidIssueTokens) != 0 {
		t.Fatalf("empty draft=%+v err=%v", status, err)
	}
	if _, err := s.SelectionStatus(context.Background(), actor, library.ID+1, []string{two}); err == nil {
		t.Fatal("missing library accepted")
	}
	if _, err := s.SelectionStatus(context.Background(), actor, library.ID, make([]string, maxStructureSelections+1)); err == nil {
		t.Fatal("unbounded draft accepted")
	}
	if _, err := s.SelectionStatus(context.Background(), actor, library.ID, []string{" "}); err == nil {
		t.Fatal("invalid token accepted")
	}
	if err := s.db.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", library.ID).Update("status", "queued").Error; err != nil {
		t.Fatal(err)
	}
	status, err = s.SelectionStatus(context.Background(), actor, library.ID, []string{two})
	if err != nil || !reflect.DeepEqual(status.InvalidIssueTokens, []string{two}) {
		t.Fatalf("running diagnosis status=%+v err=%v", status, err)
	}
	delete(actor.Permissions, authz.PermissionMediaLibrariesScan)
	if _, err := s.SelectionStatus(context.Background(), actor, library.ID, []string{two}); err == nil {
		t.Fatal("read-only actor checked repair draft")
	}
}
