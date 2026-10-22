//go:build quark_live

package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

func TestQuarkObservationCreationIdentityIndependentImmutableSource(t *testing.T) {
	prior, d := quarkObservationMutationFixture(t)
	base := prior.Base
	bundle, err := quark.ParseCredentials(base.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	read := func() (cloud.Item, error) {
		return QuarkObservationCreationIdentity(context.Background(), base.StateDir, bundle.TV.AccountID, base.Root.ID, base.RootPath, base.Sample.Name, base.Sample.Size)
	}
	before := d.reads.Load()
	first, err := read()
	if err != nil || first.ID != base.Sample.ID || first.ParentID != base.Root.ID || first.Name != base.Sample.Name || first.Size != base.Sample.Size || d.reads.Load() != before {
		t.Fatal("original positive snapshot not read exactly without provider IO")
	}
	// Advance the real catalog through observed rename and move. Creation
	// identity must still come from the immutable first positive publication.
	if report, e := RunQuarkObservationMutationLiveValidation(context.Background(), prior); e != nil {
		t.Fatalf("synthetic mutation predecessor: %s %v", report.Stage, e)
	}
	before = d.reads.Load()
	after, err := read()
	if err != nil || after != first || d.reads.Load() != before {
		t.Fatal("later current-head path replaced creation evidence")
	}
	for _, mode := range []string{"account", "root", "display_path", "name", "size"} {
		t.Run("input_"+mode, func(t *testing.T) {
			account, root, path, name, size := bundle.TV.AccountID, base.Root.ID, base.RootPath, base.Sample.Name, base.Sample.Size
			switch mode {
			case "account":
				account = "another-account"
			case "root":
				root = quark.QualifyID(quark.RouteTV, account, "another-root")
			case "display_path":
				path += "/other"
			case "name":
				name = prior.RenamedName
			case "size":
				size++
			}
			if _, e := QuarkObservationCreationIdentity(context.Background(), base.StateDir, account, root, path, name, size); e == nil {
				t.Fatal("mismatching creation request accepted")
			}
		})
	}
	var scope models.ProviderObservationScope
	var original, initial models.CatalogSnapshot
	var fact models.CatalogEntryFact
	observationMutationOfflineDB(t, prior, func(db *gorm.DB) error {
		if err := db.First(&scope).Error; err != nil {
			return err
		}
		if err := db.First(&initial, "published_revision = ? AND state = ?", 1, "published").Error; err != nil {
			return err
		}
		if err := db.First(&original, "published_revision = ? AND state = ?", 2, "published").Error; err != nil {
			return err
		}
		return db.First(&fact, "snapshot_id = ?", original.ID).Error
	})
	for _, mode := range []string{"scope_provider", "scope_kind", "scope_account", "scope_fence", "fact_namespace", "fact_account", "fact_size", "fact_tombstone", "missing_rev2", "changed_source", "changed_config", "missing_rev1", "nonempty_rev1"} {
		t.Run(mode, func(t *testing.T) {
			observationMutationOfflineDB(t, prior, func(db *gorm.DB) error {
				switch mode {
				case "scope_provider":
					return db.Model(&models.ProviderObservationScope{}).Where("id = ?", scope.ID).Update("provider", "pan115").Error
				case "scope_kind":
					return db.Model(&models.ProviderObservationScope{}).Where("id = ?", scope.ID).Update("kind", "intake").Error
				case "scope_account":
					return db.Model(&models.ProviderObservationScope{}).Where("id = ?", scope.ID).Update("account_key", "other-account").Error
				case "scope_fence":
					return db.Model(&models.ProviderObservationScope{}).Where("id = ?", scope.ID).Update("fence", "other-fence").Error
				case "fact_namespace", "fact_account":
					route, account := quark.RouteAgent, bundle.TV.AccountID
					if mode == "fact_account" {
						route, account = quark.RouteTV, "another-account"
					}
					return db.Model(&models.CatalogEntryFact{}).Where("snapshot_id = ? AND id = ?", fact.SnapshotID, fact.ID).Update("provider_id", quark.QualifyID(route, account, "sample")).Error
				case "fact_size":
					return db.Model(&models.CatalogEntryFact{}).Where("snapshot_id = ? AND id = ?", fact.SnapshotID, fact.ID).Update("size", fact.Size+1).Error
				case "fact_tombstone":
					return db.Model(&models.CatalogEntryFact{}).Where("snapshot_id = ? AND id = ?", fact.SnapshotID, fact.ID).Update("tombstone", true).Error
				case "missing_rev2":
					return db.Model(&models.CatalogSnapshot{}).Where("id = ?", original.ID).Update("published_revision", 12345).Error
				case "changed_source":
					return db.Model(&models.CatalogSnapshot{}).Where("id = ?", original.ID).Update("source_fingerprint", "different-source").Error
				case "changed_config":
					return db.Model(&models.CatalogSnapshot{}).Where("id = ?", original.ID).Update("config_fingerprint", "different-config").Error
				case "missing_rev1":
					return db.Model(&models.CatalogSnapshot{}).Where("id = ?", initial.ID).Update("published_revision", 12346).Error
				case "nonempty_rev1":
					copy := fact
					copy.SnapshotID = initial.ID
					return db.Create(&copy).Error
				}
				return nil
			})
			if _, e := read(); e == nil {
				t.Fatalf("invalid original evidence accepted: %s", mode)
			}
			// Restore only this test's disposable rows; never use this repair in
			// a live acceptance run. The helper itself remains read-only.
			observationMutationOfflineDB(t, prior, func(db *gorm.DB) error {
				if mode == "nonempty_rev1" {
					if e := db.Where("snapshot_id = ?", initial.ID).Delete(&models.CatalogEntryFact{}).Error; e != nil {
						return e
					}
				}
				for _, row := range []any{&scope, &original, &initial, &fact} {
					if e := db.Save(row).Error; e != nil {
						return e
					}
				}
				return nil
			})
			if item, e := read(); e != nil || item != first {
				t.Fatal("synthetic baseline not restored")
			}
		})
	}
	if d.reads.Load() != before {
		t.Fatal("immutable evidence lookup called provider")
	}
}

func TestQuarkObservationCreationIdentityIndependentCompactionAndCollection(t *testing.T) {
	prior, d := quarkObservationMutationFixture(t)
	base := prior.Base
	bundle, err := quark.ParseCredentials(base.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	read := func() (cloud.Item, error) {
		return QuarkObservationCreationIdentity(ctx, base.StateDir, bundle.TV.AccountID, base.Root.ID, base.RootPath, base.Sample.Name, base.Sample.Size)
	}
	original, err := read()
	if err != nil {
		t.Fatal(err)
	}
	beforeReads := d.reads.Load()
	r, err := quarkObservationLiveOpen(ctx, base, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	var initial []models.CatalogSnapshot
	if err := r.db.Where("published_revision IN ? AND state = ?", []int{1, 2}, "published").Order("published_revision").Find(&initial).Error; err != nil || len(initial) != 2 {
		t.Fatal("missing original published snapshots")
	}
	var headBefore models.CatalogHead
	if err := r.db.First(&headBefore, "library_id = ?", r.row.ID).Error; err != nil {
		t.Fatal(err)
	}
	compacted, err := r.catalog.Compact(ctx, CatalogCompactionInput{LibraryID: r.row.ID, Force: true})
	if err != nil || !compacted {
		t.Fatalf("actual production compaction failed: %t %v", compacted, err)
	}
	var headAfter models.CatalogHead
	if err := r.db.First(&headAfter, "library_id = ?", r.row.ID).Error; err != nil || headAfter.Revision != headBefore.Revision+1 {
		t.Fatal("compaction did not publish a separate physical head revision")
	}
	for _, old := range initial {
		var saved models.CatalogSnapshot
		if err := r.db.First(&saved, "id = ?", old.ID).Error; err != nil || saved.State != "published" || saved.PublishedRevision != old.PublishedRevision {
			t.Fatal("compaction changed original published evidence")
		}
		var activeLayers int64
		if err := r.db.Model(&models.CatalogHeadLayer{}).Where("snapshot_id = ?", old.ID).Count(&activeLayers).Error; err != nil || activeLayers != 0 {
			t.Fatal("test did not actually detach original snapshots from current head")
		}
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if actual, err := read(); err != nil || actual != original || d.reads.Load() != beforeReads {
		t.Fatal("cold immutable lookup lost original evidence after actual compaction")
	}

	// This is deliberately a disposable synthetic DB. Acceptance runtimes do
	// not start maintenance; prove separately that the reader fails closed if
	// production maintenance really collects its now-unreferenced evidence.
	r, err = quarkObservationLiveOpen(ctx, base, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	maintenance, err := r.catalog.Maintain(ctx, 16)
	if err != nil || maintenance.Collected == 0 {
		t.Fatalf("actual maintenance did not collect detached evidence: %+v %v", maintenance, err)
	}
	var retained int64
	if err := r.db.Model(&models.CatalogSnapshot{}).Where("id IN ? AND state = ?", []string{initial[0].ID, initial[1].ID}, "published").Count(&retained).Error; err != nil || retained == 2 {
		t.Fatal("negative case still retained both original published snapshots")
	}
	var currentFacts int64
	var currentBase models.CatalogHeadLayer
	if err := r.db.First(&currentBase, "library_id = ? AND rank = ?", r.row.ID, 0).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.db.Model(&models.CatalogEntryFact{}).Where("snapshot_id = ? AND tombstone = ?", currentBase.SnapshotID, false).Count(&currentFacts).Error; err != nil || currentFacts != 1 {
		t.Fatal("maintenance removed current sample instead of only detached evidence")
	}
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); err == nil || d.reads.Load() != beforeReads {
		t.Fatal("missing original evidence fell back to current head or provider")
	}
}
