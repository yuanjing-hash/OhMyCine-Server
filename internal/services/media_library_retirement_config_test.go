package services

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func finishRetirementConfigurationTest(t *testing.T, s *MediaLibraryService, claim ClaimedJob, row models.MediaLibraryRetirement) {
	t.Helper()
	w := NewMediaLibraryRetirementWorker(s)
	for i := 0; i < 100 && row.Phase != "completed"; i++ {
		if _, _, err := w.step(context.Background(), claim, &row); err != nil {
			t.Fatalf("retirement stranded at %s: %v", row.Phase, err)
		}
	}
	if row.Phase != "completed" {
		t.Fatalf("retirement did not finish: %s", row.Phase)
	}
}

func TestLibraryRetirementConfigurationStorageOrderingAndIsolation(t *testing.T) {
	for _, retirementFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "retirement_first", false: "configuration_first"}[retirementFirst], func(t *testing.T) {
			s, library, actor := retirementFixture(t)
			storage, _ := catalogSourceContext(t, s.catalogStore, library)
			actor.Permissions[authz.PermissionStoragesUpdate] = struct{}{}
			service := NewStorageService(s.db, s.audit)
			service.SetCatalogSnapshotStore(s.catalogStore)
			service.SetMediaChangeService(NewMediaChangeService(s.db))
			var claim ClaimedJob
			var row models.MediaLibraryRetirement
			if retirementFirst {
				claim, row = claimRetirement(t, s, library, actor)
			}
			disabled := false
			_, err := service.UpdateContext(context.Background(), actor, storage.ID, UpdateStorageInput{Enabled: &disabled}, RequestContext{})
			if retirementFirst && ErrorCode(err) != CodeConflict || !retirementFirst && err != nil {
				t.Fatalf("storage update ordering: %v", err)
			}
			if !retirementFirst {
				claim, row = claimRetirement(t, s, library, actor)
			} else {
				name := "display-only rename"
				if _, err := service.UpdateContext(context.Background(), actor, storage.ID, UpdateStorageInput{Name: &name}, RequestContext{}); err != nil {
					t.Fatalf("cosmetic storage rename blocked: %v", err)
				}
				other := storage
				other.ID, other.Name, other.NameNormalized = 0, "other storage", "other storage"
				other.RootPath = t.TempDir()
				other.RootPathNormalized, other.RootDisplayPath = other.RootPath, other.RootPath
				if err := s.db.Create(&other).Error; err != nil {
					t.Fatal(err)
				}
				if _, err := service.UpdateContext(context.Background(), actor, other.ID, UpdateStorageInput{Enabled: &disabled}, RequestContext{}); err != nil {
					t.Fatalf("unrelated storage blocked: %v", err)
				}
			}
			finishRetirementConfigurationTest(t, s, claim, row)
		})
	}
}

func TestLibraryRetirementConfigurationConnectionOrdering(t *testing.T) {
	for _, retirementFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "retirement_first", false: "configuration_first"}[retirementFirst], func(t *testing.T) {
			store, connectionService, actor, library, connection := catalogConnectionFixture(t, true)
			s := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
			s.SetCatalogSnapshotStore(store)
			s.SetQueueService(NewQueueService(store.writeDB, s.audit))
			actor.Permissions[authz.PermissionMediaLibrariesDelete] = struct{}{}
			var claim ClaimedJob
			var row models.MediaLibraryRetirement
			if retirementFirst {
				claim, row = claimRetirement(t, s, library, actor)
			}
			disabled := false
			_, err := connectionService.Update(actor, connection.ID, UpdateConnectionInput{Revision: connection.Revision, Enabled: &disabled}, RequestContext{})
			if retirementFirst && ErrorCode(err) != CodeConflict || !retirementFirst && err != nil {
				t.Fatalf("connection update ordering: %v", err)
			}
			if !retirementFirst {
				claim, row = claimRetirement(t, s, library, actor)
			} else {
				name := "display-only connection"
				if _, err := connectionService.Update(actor, connection.ID, UpdateConnectionInput{Revision: connection.Revision, Name: &name}, RequestContext{}); err != nil {
					t.Fatalf("cosmetic connection rename blocked: %v", err)
				}
			}
			finishRetirementConfigurationTest(t, s, claim, row)
		})
	}
}

func TestLibraryRetirementConfigurationProfileAndLateNotifier(t *testing.T) {
	for _, lateNotifier := range []bool{false, true} {
		t.Run(map[bool]string{false: "retirement_first", true: "profile_commit_then_retirement_then_notifier"}[lateNotifier], func(t *testing.T) {
			s, library, actor := retirementFixture(t)
			storage, profile := catalogSourceContext(t, s.catalogStore, library)
			profile.ID, profile.Code, profile.Protected = 0, nil, false
			profile.Kind, profile.Name, profile.NameNormalized = models.MediaClassificationProfileKindCustom, "retirement custom", "retirement custom"
			if err := s.db.Create(&profile).Error; err != nil {
				t.Fatal(err)
			}
			next := library
			next.ProfileID = profile.ID
			if err := s.db.Transaction(func(tx *gorm.DB) error {
				if _, err := applyCatalogLibraryChangeTx(tx, library, &next, storage, storage, profile); err != nil {
					return err
				}
				return tx.Model(&library).Update("profile_id", profile.ID).Error
			}); err != nil {
				t.Fatal(err)
			}
			library = next
			actor.Permissions[authz.PermissionMediaClassificationProfilesUpdate] = struct{}{}
			profiles := NewMediaClassificationProfileService(s.db, s.audit, nil)
			input := UpdateMediaClassificationProfileInput{Revision: profile.Revision, Name: "renamed profile", Rules: json.RawMessage(profile.RulesJSON)}
			var claim ClaimedJob
			var row models.MediaLibraryRetirement
			if lateNotifier {
				if _, err := profiles.Update(actor, profile.ID, input, RequestContext{}); err != nil {
					t.Fatal(err)
				}
			}
			claim, row = claimRetirement(t, s, library, actor)
			if !lateNotifier {
				if _, err := profiles.Update(actor, profile.ID, input, RequestContext{}); ErrorCode(err) != CodeConflict {
					t.Fatalf("profile revision advanced during retirement: %v", err)
				}
			} else {
				// Commit-before-notifier is a real lifecycle window, not authority
				// to mutate the newly frozen retirement head.
				if err := s.ProfileRevisionChanged(profile.ID, profile.Revision+1); err != nil {
					t.Fatal(err)
				}
			}
			finishRetirementConfigurationTest(t, s, claim, row)
		})
	}
}
