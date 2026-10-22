//go:build quark_live

package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkObservationMutationFake struct {
	*quarkObservationLiveFake
	directory      cloud.Item
	renames, moves int
}

func (d *quarkObservationMutationFake) List(_ context.Context, id string, page cloud.PageRequest) (cloud.Page, error) {
	d.reads.Add(1)
	if page.Offset != 0 {
		return cloud.Page{}, ErrObservationInvalid
	}
	var items []cloud.Item
	if id == d.root.ID {
		if d.directory.ID != "" {
			items = append(items, d.directory)
		}
		if d.sample.ParentID == id {
			items = append(items, d.sample)
		}
	} else if id == d.directory.ID && id != "" {
		if d.sample.ParentID == id {
			items = append(items, d.sample)
		}
	} else {
		return cloud.Page{}, ErrObservationInvalid
	}
	total := int64(len(items))
	return cloud.Page{Items: items, Offset: 0, Total: &total}, nil
}

func (d *quarkObservationMutationFake) Stat(ctx context.Context, id string) (cloud.Item, error) {
	if id == d.directory.ID && id != "" {
		return d.directory, nil
	}
	return d.quarkObservationLiveFake.Stat(ctx, id)
}

// Synthetic actor changes only its in-memory provider. It never supplies facts
// to observer tables or mutates catalog rows. Production List/Stat discover it.
func quarkObservationMutationFixture(t *testing.T) (QuarkObservationMutationLiveInput, *quarkObservationMutationFake) {
	t.Helper()
	base, driver := quarkObservationLiveFixture(t)
	d := &quarkObservationMutationFake{quarkObservationLiveFake: driver}
	base.Registry = cloud.NewRegistry()
	if err := base.Registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return d, nil }); err != nil {
		t.Fatal(err)
	}
	report, err := RunQuarkObservationLiveValidation(context.Background(), base)
	if err != nil {
		t.Fatalf("initial fixture stage=%s error=%v", report.Stage, err)
	}
	in := QuarkObservationMutationLiveInput{Base: base, RunID: "synthetic-run-0001", RenamedName: "Native.Observation.Renamed.2026.mp4", DirectoryName: "synthetic-moved"}
	in.Mutate = func(_ context.Context, stage string) error {
		switch stage {
		case "rename_one":
			if d.sample.Name != in.RenamedName {
				d.sample.Name = in.RenamedName
				d.renames++
			}
		case "mkdir_move_one":
			if d.directory.ID == "" {
				d.directory = cloud.Item{ID: quark.QualifyID(quark.RouteTV, "synthetic-account", "child"), ParentID: d.root.ID, Name: in.DirectoryName, IsDir: true, ModifiedAt: time.Now().UTC()}
			}
			if d.sample.ParentID != d.directory.ID {
				d.sample.ParentID = d.directory.ID
				d.moves++
			}
		default:
			return errors.New("invalid_stage")
		}
		return nil
	}
	return in, d
}

func TestQuarkObservationMutationLiveOffline(t *testing.T) {
	in, d := quarkObservationMutationFixture(t)
	report, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err != nil {
		t.Fatalf("stage=%s error=%v", report.Stage, err)
	}
	if !report.ReusedDatabase || !report.RenameObserved || !report.MoveObserved || !report.StableIdentity || !report.OldOwnedArtifactsRemoved || !report.NoopUnchanged || report.NoopPages < 2 || report.NoopRequests < 2 || report.STRMCount != 1 || !report.ManualScanNow || report.AutomaticTimingTested || report.PlaybackTested || d.renames != 1 || d.moves != 1 {
		t.Fatalf("invalid report %+v", report)
	}
	raw, _ := json.Marshal(report)
	for _, secret := range []string{"synthetic", in.Base.Sample.ID, in.Base.StateDir, "http://", "private-token"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private report")
		}
	}
	before := d.reads.Load()
	in.Mutate = func(context.Context, string) error { t.Fatal("completed callback"); return nil }
	reopened, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err != nil || !reopened.CompletedReopen || reopened.Requests != 0 || reopened.ReadyNotifications != 0 || d.reads.Load() != before {
		t.Fatalf("completed reopen %+v %v", reopened, err)
	}
}

func TestQuarkObservationMutationLivePendingResume(t *testing.T) {
	in, d := quarkObservationMutationFixture(t)
	actor := in.Mutate
	failed := false
	in.Mutate = func(ctx context.Context, stage string) error {
		if err := actor(ctx, stage); err != nil {
			return err
		}
		if stage == "rename_one" && !failed {
			failed = true
			return errors.New("synthetic-private-unknown")
		}
		return nil
	}
	report, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err == nil || report.Stage != "rename_one" || strings.Contains(err.Error(), "private") || d.renames != 1 || d.moves != 0 {
		t.Fatalf("unknown boundary %+v %v", report, err)
	}
	report, err = RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err != nil || report.Stage != "completed" || d.renames != 1 || d.moves != 1 {
		t.Fatalf("resume %+v %v", report, err)
	}
}

func TestQuarkObservationMutationLiveAccessSnapshotIdentity(t *testing.T) {
	in, _ := quarkObservationMutationFixture(t)
	r, err := quarkObservationLiveOpen(context.Background(), in.Base, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	before, err := r.mutationContract(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _ := quark.ParseCredentials(in.Base.Credentials)
	bundle.TV.Generation++
	bundle.TV.AccessToken = "new-private-access-only"
	in.Base.Credentials, _ = json.Marshal(bundle)
	if err = r.mutationAccessSnapshot(in.Base); err != nil {
		t.Fatal("same identity TV renewal rejected")
	}
	after, err := r.mutationContract(context.Background(), in)
	if err != nil || before != after {
		t.Fatal("renewal changed source/key contract")
	}
	bundle.TV.DeviceID = "other-device"
	in.Base.Credentials, _ = json.Marshal(bundle)
	if err = r.mutationAccessSnapshot(in.Base); err == nil {
		t.Fatal("different device accepted")
	}
	if afterReject, e := r.mutationContract(context.Background(), in); e != nil || afterReject != before {
		t.Fatal("rejected refresh changed persisted contract")
	}
}

func TestQuarkObservationMutationLiveDelayedVisibilityResume(t *testing.T) {
	in, d := quarkObservationMutationFixture(t)
	actor := in.Mutate
	in.Mutate = func(context.Context, string) error { return nil }
	report, err := RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err == nil || report.Stage != "rename_one_scan" {
		t.Fatalf("old listing accepted: %+v %v", report, err)
	}
	// The synthetic provider becomes consistent after the first bounded round.
	// This changes provider state only; no observation/catalog table is edited.
	d.sample.Name = in.RenamedName
	in.Mutate = func(ctx context.Context, stage string) error {
		if stage == "rename_one" {
			t.Fatal("confirmed actor repeated")
		}
		return actor(ctx, stage)
	}
	report, err = RunQuarkObservationMutationLiveValidation(context.Background(), in)
	if err != nil || report.Stage != "completed" || d.moves != 1 {
		t.Fatalf("visibility resume: %+v %v", report, err)
	}
}
