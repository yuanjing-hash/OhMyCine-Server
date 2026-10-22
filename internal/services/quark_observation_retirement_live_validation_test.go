//go:build quark_live

package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func quarkObservationRetirementFixture(t *testing.T) (QuarkObservationRetirementLiveInput, *quarkObservationMutationFake) {
	t.Helper()
	prior, d := quarkObservationMutationFixture(t)
	if report, err := RunQuarkObservationMutationLiveValidation(context.Background(), prior); err != nil {
		t.Fatalf("synthetic predecessor stage=%s err=%v", report.Stage, err)
	}
	start := time.Now().UTC().Add(-11 * time.Minute)
	in := QuarkObservationRetirementLiveInput{Base: prior.Base, RunID: "retirement-run-0001", StartedAt: start, Deadline: start.Add(25 * time.Minute), Predecessor: QuarkObservationRetirementPredecessor{RunID: prior.RunID, RenamedName: prior.RenamedName, DirectoryName: prior.DirectoryName, DirectoryTVID: d.directory.ID, JournalSHA256: strings.Repeat("a", 64), KeySHA256: strings.Repeat("b", 64)}}
	in.Mutate = func(_ context.Context, stage string) error {
		switch stage {
		case "move_out":
			d.sample.ParentID = "synthetic-outside-owned-library"
			d.directory.ModifiedAt = time.Now().UTC()
		case "move_back":
			d.sample.ParentID = d.directory.ID
			d.directory.ModifiedAt = time.Now().UTC()
		default:
			return errors.New("unexpected_actor_stage")
		}
		return nil
	}
	return in, d
}

func TestQuarkObservationRetirementWaitBoundaries(t *testing.T) {
	now := time.Now().UTC()
	in := QuarkObservationRetirementLiveInput{StartedAt: now.Add(-time.Minute), Deadline: now.Add(24 * time.Minute)}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	in.Progress = func(progress QuarkObservationRetirementProgress) {
		calls++
		if progress.Stage != "wait_confirm" || progress.WaitSeconds < 600 || progress.WaitSeconds > 605 {
			t.Fatal("wait progress was not the real remaining interval")
		}
		cancel()
	}
	if err := retirementWait(ctx, in, now); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("wait did not respect cancellation", err)
	}
	for _, bad := range []time.Time{{}, now.Add(time.Minute), in.StartedAt.Add(-time.Second)} {
		if err := retirementWait(context.Background(), in, bad); err == nil {
			t.Fatal("invalid missing timestamp accepted")
		}
	}
	in.Deadline = now.Add(time.Minute)
	if err := retirementWait(context.Background(), in, now); err == nil {
		t.Fatal("insufficient original deadline accepted")
	}
}

func TestQuarkObservationRetirementOfflineSeededContinuation(t *testing.T) {
	in, d := quarkObservationRetirementFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	actorCalls := map[string]int{}
	actor := in.Mutate
	in.Mutate = func(ctx context.Context, phase string) error { actorCalls[phase]++; return actor(ctx, phase) }
	in.Progress = func(progress QuarkObservationRetirementProgress) {
		if progress.Stage == "wait_confirm" && progress.WaitSeconds > 0 {
			cancel()
		}
	}
	report, err := RunQuarkObservationRetirementLiveValidation(ctx, in)
	if err == nil || report.Stage != "wait_confirm" || actorCalls["move_out"] != 1 || actorCalls["move_back"] != 0 {
		t.Fatalf("real first observation did not reach cancellable wait: %+v %v calls=%v", report, err, actorCalls)
	}

	// OFFLINE ONLY: seed elapsed-time evidence in this synthetic provider's
	// disposable DB to exercise the continuation without a ten-minute unit test.
	// The exported live input has no fake clock / skip-wait / fact injection.
	// This test is explicitly NOT evidence of a real ten-minute live interval.
	r, err := quarkObservationLiveOpen(context.Background(), in.Base, false)
	if err != nil {
		t.Fatal(err)
	}
	var cp quarkObservationRetirementCheckpoint
	if err = r.db.First(&cp, 1).Error; err != nil {
		t.Fatal(err)
	}
	var state quarkRetirementState
	if err = json.Unmarshal([]byte(cp.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	if state.UserDigest == "" || !state.Authority.Manual || state.Authority.TMDBID != 42 || state.Authority.Title != quarkRetirementTitle {
		t.Fatal("nonempty manual/user baseline missing")
	}
	old := time.Now().UTC().Add(-quarkRetirementWait - time.Second)
	state.FirstMissingAt = old
	state.FirstRound.StartedAt, state.FirstRound.CompletedAt = old.Add(-time.Second), old.Add(time.Second)
	if err = r.db.Model(&models.ProviderObservationItem{}).Where("batch_id = '' AND resource_id = ?", in.Base.Sample.ID).Updates(map[string]any{"missing_since": old, "missing_last_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	cp.StateJSON = string(mustMutationJSON(state))
	if err = r.db.Save(&cp).Error; err != nil {
		t.Fatal(err)
	}
	if err = r.close(); err != nil {
		t.Fatal(err)
	}
	in.Progress = nil
	report, err = RunQuarkObservationRetirementLiveValidation(context.Background(), in)
	if err != nil {
		t.Fatalf("seeded continuation stage=%s err=%v", report.Stage, err)
	}
	if !report.Completed || !report.TwoCompleteCoverages || !report.MissingIntervalSatisfied || !report.LatestManualMetadataPreserved || !report.NonemptyFavoritePreserved || !report.NonemptyHistoryPreserved || !report.RevokedBeforeArtifacts || !report.RevokedDBReopen || !report.CompletedDBReopen || !report.OwnedSTRMRemoved || !report.LateCleanupSafe || !report.NoopUnchanged || report.NoopPages < 2 || report.NoopRequests < 2 || !report.ManualScanNow || !report.SyntheticMetadata || report.AutomaticTimingTested || report.PlaybackTested || report.CloudDeleteTested || actorCalls["move_out"] != 1 || actorCalls["move_back"] != 1 {
		t.Fatalf("incomplete bounded evidence: %+v calls=%v", report, actorCalls)
	}
	raw := string(mustMutationJSON(report))
	for _, private := range []string{"http://", in.Base.StateDir, in.Base.Sample.ID, in.Predecessor.DirectoryTVID, "synthetic-account"} {
		if strings.Contains(raw, private) {
			t.Fatal("private report")
		}
	}
	before := d.reads.Load()
	in.Mutate = func(context.Context, string) error { t.Fatal("completed actor invoked"); return nil }
	report, err = RunQuarkObservationRetirementLiveValidation(context.Background(), in)
	if err != nil || !report.Completed || !report.CompletedDBReopen || report.Requests != 0 || d.reads.Load() != before {
		t.Fatalf("completed reopen read cloud or changed state: %+v %v", report, err)
	}
	// The normal compactor ran during retirement. After all service pools and
	// their runtime lock have closed, immutable creation evidence must still
	// identify the original upload, rather than the current renamed/moved fact.
	created, err := QuarkObservationCreationIdentity(context.Background(), in.Base.StateDir, "synthetic-account", in.Base.Root.ID, in.Base.RootPath, in.Base.Sample.Name, in.Base.Sample.Size)
	if err != nil || created.ID != in.Base.Sample.ID || created.ParentID != in.Base.Root.ID || created.Name != in.Base.Sample.Name || created.Size != in.Base.Sample.Size || d.reads.Load() != before {
		t.Fatalf("cold creation evidence changed or read provider: %v", err)
	}
}
