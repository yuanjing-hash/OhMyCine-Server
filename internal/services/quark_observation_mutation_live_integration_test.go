//go:build quark_live

package services

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// These helpers open only each test's disposable database, after the live
// assembler has closed it. They never open an account or deployment database.
func observationMutationOfflineDB(t *testing.T, in QuarkObservationMutationLiveInput, edit func(*gorm.DB) error) {
	t.Helper()
	db, err := database.Open(filepath.Join(in.Base.StateDir, "observation.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = edit(db); err != nil {
		t.Fatal(err)
	}
}

func TestQuarkObservationMutationIndependentActorIsNotObservation(t *testing.T) {
	in, d := quarkObservationMutationFixture(t)
	originalActor := in.Mutate
	var original models.CatalogHead
	observationMutationOfflineDB(t, in, func(db *gorm.DB) error { return db.First(&original).Error })
	calls := 0
	in.Mutate = func(context.Context, string) error { calls++; return nil }
	beforeReads := d.reads.Load()
	report, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err == nil || calls != 1 || d.renames != 0 || d.moves != 0 || d.reads.Load() <= beforeReads || report.RenameObserved || report.MoveObserved || report.Stage == "completed" {
		t.Fatalf("callback success was mistaken for provider observation: %+v %v", report, err)
	}
	observationMutationOfflineDB(t, in, func(db *gorm.DB) error {
		var current models.CatalogHead
		if err := db.First(&current).Error; err != nil {
			return err
		}
		if current.Revision != original.Revision {
			t.Fatal("unchanged provider created a catalog mutation")
		}
		var cp quarkObservationMutationCheckpoint
		if err := db.First(&cp, 1).Error; err != nil {
			return err
		}
		if cp.Stage != "rename_one_observe" {
			t.Fatal("unobserved actor advanced beyond directory observation phase")
		}
		return nil
	})
	// The provider's directory view becomes current later. Recovery must read
	// that view without calling the already accepted rename actor again.
	d.sample.Name = in.RenamedName
	in.Mutate = func(ctx context.Context, stage string) error {
		if stage == "rename_one" {
			t.Fatal("stale listing recovery repeated rename actor")
		}
		return originalActor(ctx, stage)
	}
	recovered, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err != nil || !recovered.RenameObserved || !recovered.MoveObserved || !recovered.NoopUnchanged || calls != 1 || d.moves != 1 {
		t.Fatalf("later real listing could not resume original observation: %+v %v", recovered, err)
	}
}

func TestQuarkObservationMutationIndependentFrozenSourceAndKey(t *testing.T) {
	for _, kind := range []string{"profile_revision", "signing_key_identity", "run_id"} {
		t.Run(kind, func(t *testing.T) {
			in, d := quarkObservationMutationFixture(t)
			in.Mutate = func(context.Context, string) error { return errors.New("synthetic pre-mutation stop") }
			if _, err := RunQuarkObservationMutationLiveValidation(context.Background(), in); err == nil {
				t.Fatal("initial stop did not preserve checkpoint")
			}
			observationMutationOfflineDB(t, in, func(db *gorm.DB) error {
				switch kind {
				case "profile_revision":
					return db.Model(&models.MediaLibrary{}).Where("id > 0").Update("profile_revision", gorm.Expr("profile_revision + 1")).Error
				case "signing_key_identity":
					return db.Model(&models.ProxySigningKey{}).Where("id <> ''").Update("created_at", time.Now().UTC().Add(time.Hour)).Error
				}
				return nil
			})
			if kind == "run_id" {
				in.RunID = "another-synthetic-run"
			}
			before := d.reads.Load()
			in.Mutate = func(context.Context, string) error { t.Fatal("changed frozen contract reached actor"); return nil }
			report, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
			if err == nil || d.reads.Load() != before || d.renames != 0 || d.moves != 0 || report.Stage == "completed" {
				t.Fatalf("changed contract accepted %+v %v", report, err)
			}
		})
	}
}

func TestQuarkObservationMutationIndependentPostPublicationCheckpointRecovery(t *testing.T) {
	in, d := quarkObservationMutationFixture(t)
	actor := in.Mutate
	in.Mutate = func(context.Context, string) error { return errors.New("synthetic setup stop") }
	if _, err := RunQuarkObservationMutationLiveValidation(context.Background(), in); err == nil {
		t.Fatal("setup did not stop")
	}
	observationMutationOfflineDB(t, in, func(db *gorm.DB) error {
		return db.Exec("CREATE TRIGGER independent_mutation_checkpoint_failure BEFORE UPDATE OF stage ON quark_live_mutation_checkpoint WHEN NEW.stage='mkdir_move_one' BEGIN SELECT RAISE(ABORT,'synthetic checkpoint failure'); END").Error
	})
	actorCalls := map[string]int{}
	in.Mutate = func(ctx context.Context, stage string) error {
		actorCalls[stage]++
		return actor(ctx, stage)
	}
	report, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err == nil || d.renames != 1 || d.moves != 0 || report.Stage == "completed" {
		t.Fatalf("did not stop after rename output: %+v %v", report, err)
	}
	var head models.CatalogHead
	observationMutationOfflineDB(t, in, func(db *gorm.DB) error {
		if err := db.First(&head).Error; err != nil {
			return err
		}
		return db.Exec("DROP TRIGGER independent_mutation_checkpoint_failure").Error
	})
	report, err = RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err != nil || report.Stage != "completed" || !report.StableIdentity || !report.OldOwnedArtifactsRemoved || !report.NoopUnchanged || d.renames != 1 || d.moves != 1 || actorCalls["rename_one"] != 1 || actorCalls["mkdir_move_one"] != 1 {
		t.Fatalf("durable recovery repeated or lost change: %+v %v", report, err)
	}
	observationMutationOfflineDB(t, in, func(db *gorm.DB) error {
		var final models.CatalogHead
		if err := db.First(&final).Error; err != nil {
			return err
		}
		if final.Revision != head.Revision+1 {
			t.Fatal("rename replay published a second revision")
		}
		return nil
	})
}
