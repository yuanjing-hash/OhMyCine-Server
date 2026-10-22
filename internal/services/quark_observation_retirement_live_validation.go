//go:build quark_live

package services

// Acceptance-only continuation of a completed, receipt-owned observation run.
// The caller owns credentials, the two frozen move receipts, and the cumulative
// transport budget. This file never supplies cloud facts or changes an observer
// clock, queue timestamp, missing timestamp, lease, or catalog row directly.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type QuarkObservationRetirementPredecessor struct {
	RunID, RenamedName, DirectoryName, DirectoryTVID string
	JournalSHA256, KeySHA256                         string
}

type QuarkObservationRetirementLiveInput struct {
	Base                QuarkObservationLiveInput
	RunID               string
	Predecessor         QuarkObservationRetirementPredecessor
	StartedAt, Deadline time.Time
	// Only move_out / move_back. The handler uses the same frozen receipt on
	// recovery; nil means accepted and independently confirmed, not observed.
	Mutate   func(context.Context, string) error
	Progress func(QuarkObservationRetirementProgress)
}

type QuarkObservationRetirementProgress struct {
	Stage       string `json:"stage"`
	WaitSeconds int64  `json:"wait_seconds"`
}

type QuarkObservationRetirementLiveReport struct {
	Version                       int    `json:"version"`
	Stage                         string `json:"stage"`
	Completed                     bool   `json:"completed"`
	TwoCompleteCoverages          bool   `json:"two_complete_coverages"`
	MissingIntervalSatisfied      bool   `json:"missing_interval_satisfied"`
	LatestManualMetadataPreserved bool   `json:"latest_manual_metadata_preserved"`
	NonemptyFavoritePreserved     bool   `json:"nonempty_favorite_preserved"`
	NonemptyHistoryPreserved      bool   `json:"nonempty_history_preserved"`
	RevokedBeforeArtifacts        bool   `json:"revoked_before_artifacts"`
	RevokedDBReopen               bool   `json:"revoked_db_reopen"`
	CompletedDBReopen             bool   `json:"completed_db_reopen"`
	OwnedSTRMRemoved              bool   `json:"owned_strm_removed"`
	LateCleanupSafe               bool   `json:"late_cleanup_safe"`
	NoopUnchanged                 bool   `json:"noop_unchanged"`
	NoopPages                     int64  `json:"noop_pages"`
	NoopRequests                  int    `json:"noop_requests"`
	Requests                      int    `json:"requests_this_invocation"`
	ObservedIntervalSeconds       int64  `json:"observed_interval_seconds"`
	ManualScanNow                 bool   `json:"manual_scan_now"`
	SyntheticMetadata             bool   `json:"synthetic_metadata"`
	AutomaticTimingTested         bool   `json:"automatic_timing_tested"`
	PlaybackTested                bool   `json:"playback_tested"`
	CloudDeleteTested             bool   `json:"cloud_delete_tested"`
}

type quarkObservationRetirementCheckpoint struct {
	ID        uint `gorm:"primaryKey"`
	Contract  string
	Stage     string
	StateJSON string
}

func (quarkObservationRetirementCheckpoint) TableName() string {
	return "quark_live_retirement_checkpoint"
}

type quarkRetirementAuthority struct {
	EntryID, RecognitionID uint
	CreatedAt              time.Time
	ProviderID, WorkKey    string
	RelativePath           string
	Size                   int64
	Manual                 bool
	TMDBID                 int64
	Title, MetadataJSON    string
}

type quarkRetirementRound struct {
	Stage                    string
	Before, Expected         uint64
	BeforeStarted, StartedAt time.Time
	CompletedAt              time.Time
	PagesBefore, Pages       int64
	RequestsBefore, Requests int
}

type quarkRetirementState struct {
	Original, Baseline, Restored quarkObservationMutationSnapshot
	Authority                    quarkRetirementAuthority
	UserDigest                   string
	HistoryUpdatedAt             int64
	Round                        quarkRetirementRound
	FirstRound                   quarkRetirementRound
	FirstMissingAt               time.Time
	FirstMissingBatch            string
	FirstMissingRound            uint64
	DeletionHead                 uint64
	DeletionGeneration           uint64
	DeletionRunID                string
	Report                       QuarkObservationRetirementLiveReport
}

const quarkRetirementWait = 10*time.Minute + 5*time.Second
const quarkRetirementTitle = "Isolated retirement edited title"
const quarkRetirementOverview = "Synthetic local metadata for observation recovery validation."

func retirementRelative(in QuarkObservationRetirementLiveInput) string {
	return "/" + in.Predecessor.DirectoryName + "/" + in.Predecessor.RenamedName
}

func retirementStageValid(stage string) bool {
	switch stage {
	case "manual_match", "manual_edit", "manual_artifacts", "user_state", "move_out", "first_observe", "wait_confirm", "second_observe", "revoked_verify", "revoked_reopen", "retire_artifacts", "move_back", "restore_observe", "restore_artifacts", "late_cleanup", "noop_observe", "noop_artifacts", "completed_reopen", "completed":
		return true
	}
	return false
}

func retirementInput(in QuarkObservationRetirementLiveInput) error {
	if err := quarkObservationLiveInputModeCheck(in.Base, false); err != nil {
		return err
	}
	id := regexp.MustCompile(`^[a-zA-Z0-9_-]{8,64}$`)
	hash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	p := in.Predecessor
	if !id.MatchString(in.RunID) || !id.MatchString(p.RunID) || in.RunID == p.RunID || !hash.MatchString(p.JournalSHA256) || !hash.MatchString(p.KeySHA256) || !quarkMutationNameValid(p.RenamedName) || !strings.EqualFold(filepath.Ext(p.RenamedName), ".mp4") || !quarkMutationNameValid(p.DirectoryName) || p.DirectoryTVID == "" || in.Mutate == nil || in.StartedAt.IsZero() || in.Deadline.Sub(in.StartedAt) != 25*time.Minute || in.StartedAt.After(time.Now().Add(5*time.Second)) {
		return errors.New("invalid_retirement_contract")
	}
	return nil
}

func (r *quarkObservationLiveRuntime) retirementContract(ctx context.Context, in QuarkObservationRetirementLiveInput) (string, quarkObservationMutationSnapshot, error) {
	p := in.Predecessor
	priorInput := QuarkObservationMutationLiveInput{Base: in.Base, RunID: p.RunID, RenamedName: p.RenamedName, DirectoryName: p.DirectoryName}
	want, err := r.mutationContract(ctx, priorInput)
	if err != nil {
		return "", quarkObservationMutationSnapshot{}, err
	}
	var prior quarkObservationMutationCheckpoint
	if err = r.db.WithContext(ctx).First(&prior, 1).Error; err != nil || prior.Stage != "completed" || prior.Contract != want {
		return "", quarkObservationMutationSnapshot{}, errors.New("predecessor_not_completed")
	}
	var previous quarkObservationMutationSnapshot
	var report QuarkObservationMutationLiveReport
	if json.Unmarshal([]byte(prior.PreviousJSON), &previous) != nil || json.Unmarshal([]byte(prior.ReportJSON), &report) != nil || previous.EntryID == 0 || previous.RelativePath != retirementRelative(in) || report.Stage != "completed" || !report.RenameObserved || !report.MoveObserved || !report.NoopUnchanged || report.STRMCount != 1 {
		return "", previous, errors.New("predecessor_invalid")
	}
	return mutationDigest([]any{want, prior, in.RunID, p, in.StartedAt.UTC(), in.Deadline.UTC()}), previous, nil
}

func (r *quarkObservationLiveRuntime) retirementScope(ctx context.Context) (models.ProviderObservationScope, error) {
	var rows []models.ProviderObservationScope
	err := r.db.WithContext(ctx).Where("library_id = ?", r.row.ID).Limit(2).Find(&rows).Error
	if err != nil || len(rows) != 1 || !rows[0].Enabled || rows[0].Kind != "library" || rows[0].Provider != "quark" || rows[0].RootID != r.row.ProviderRootID {
		return models.ProviderObservationScope{}, errors.New("retirement_scope")
	}
	return rows[0], nil
}

func (r *quarkObservationLiveRuntime) retirementItem(ctx context.Context, in QuarkObservationRetirementLiveInput) (models.ProviderObservationItem, error) {
	var item models.ProviderObservationItem
	scope, err := r.retirementScope(ctx)
	if err != nil {
		return item, err
	}
	err = observationCurrent(r.db.WithContext(ctx), scope.ID).Where("resource_id = ?", in.Base.Sample.ID).First(&item).Error
	if err != nil || item.ResourceID != in.Base.Sample.ID || item.IsDir || item.Name != in.Predecessor.RenamedName || item.ParentID != in.Predecessor.DirectoryTVID || item.Size != in.Base.Sample.Size {
		return item, errors.New("sample_identity_changed")
	}
	return item, nil
}

func (r *quarkObservationLiveRuntime) retirementParent(ctx context.Context, in QuarkObservationRetirementLiveInput) error {
	scope, err := r.retirementScope(ctx)
	if err != nil {
		return err
	}
	var directory models.ProviderObservationItem
	err = observationCurrent(r.db.WithContext(ctx), scope.ID).Where("resource_id = ?", in.Predecessor.DirectoryTVID).First(&directory).Error
	if err != nil || !directory.IsDir || directory.ParentID != in.Base.Root.ID || directory.Name != in.Predecessor.DirectoryName || directory.DownlistedAt != nil || directory.MissingSince != nil {
		return errors.New("parent_identity_changed")
	}
	return nil
}

// Read the current effective catalog on the pinned reader, never legacy rows.
func (r *quarkObservationLiveRuntime) retirementAuthority(ctx context.Context, in QuarkObservationRetirementLiveInput) (quarkRetirementAuthority, error) {
	var out quarkRetirementAuthority
	snapshot, err := r.snapshotAt(ctx, in.Base, retirementRelative(in), false)
	if err != nil || snapshot.row.RecognitionID == nil {
		return out, errors.New("authority_entry")
	}
	var recognition models.MediaLibraryRecognition
	err = r.catalog.Read(ctx, []uint{r.row.ID}, func(reader *CatalogReader) error {
		return reader.Recognitions().First(&recognition, *snapshot.row.RecognitionID).Error
	})
	if err != nil {
		return out, err
	}
	out = quarkRetirementAuthority{EntryID: snapshot.row.ID, CreatedAt: snapshot.row.CreatedAt, ProviderID: snapshot.row.ProviderID, WorkKey: snapshot.row.WorkKey, RelativePath: snapshot.row.RelativePath, Size: snapshot.row.Size, RecognitionID: recognition.ID, Manual: recognition.ManualOverride, Title: recognition.Title, MetadataJSON: recognition.MetadataJSON}
	if recognition.TMDBID != nil {
		out.TMDBID = *recognition.TMDBID
	}
	return out, nil
}

func (r *quarkObservationLiveRuntime) retirementAbsent(ctx context.Context) (uint64, error) {
	var count int64
	err := r.catalog.Read(ctx, []uint{r.row.ID}, func(reader *CatalogReader) error { return reader.Entries().Count(&count).Error })
	if err != nil || count != 0 {
		return 0, errors.New("catalog_not_retired")
	}
	var head models.CatalogHead
	if err = r.db.WithContext(ctx).First(&head, "library_id = ?", r.row.ID).Error; err != nil {
		return 0, err
	}
	return head.Revision, nil
}

// A physical catalog compaction may add a head revision without changing any
// media semantics. Verify its actual immutable E/R facts instead of accepting
// an arbitrary head jump or disabling the normal production compactor.
func (r *quarkObservationLiveRuntime) retirementVersionFacts(ctx context.Context, revision uint64, kind string, authority quarkRetirementAuthority, absent, tombstones bool) error {
	return r.readDB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var snapshot models.CatalogSnapshot
		var head models.CatalogHead
		if err := tx.First(&head, "library_id = ?", r.row.ID).Error; err != nil {
			return err
		}
		if err := tx.Where("library_id = ? AND published_revision = ? AND state = ?", r.row.ID, revision, "published").First(&snapshot).Error; err != nil || snapshot.Kind != kind || snapshot.SourceEpoch != head.SourceEpoch || snapshot.SourceFingerprint != head.SourceFingerprint || snapshot.ConfigFingerprint != head.ConfigFingerprint || snapshot.ParentRevision != revision-1 {
			return errors.New("unexpected_catalog_version")
		}
		var entries []models.CatalogEntryFact
		var recognitions []models.CatalogRecognitionFact
		if err := tx.Where("snapshot_id = ?", snapshot.ID).Limit(2).Find(&entries).Error; err != nil {
			return err
		}
		if err := tx.Where("snapshot_id = ?", snapshot.ID).Limit(2).Find(&recognitions).Error; err != nil {
			return err
		}
		if absent && !tombstones {
			if len(entries) != 0 || len(recognitions) != 0 {
				return errors.New("compaction_not_empty")
			}
			return nil
		}
		if len(entries) != 1 || len(recognitions) != 1 || entries[0].ID != authority.EntryID || recognitions[0].ID != authority.RecognitionID || entries[0].Tombstone != tombstones || recognitions[0].Tombstone != tombstones {
			return errors.New("unexpected_version_facts")
		}
		if !tombstones {
			// Physical entry facts intentionally retain shared override masks. Read
			// their effective projection using the same catalog overlay, rather than
			// comparing a stale raw work key to the current recognition's work key.
			var e models.MediaLibraryEntry
			reader := &CatalogReader{tx: tx, layers: []models.CatalogHeadLayer{{LibraryID: r.row.ID, SnapshotID: snapshot.ID}}, privateLayers: true, heads: map[uint]models.CatalogHead{r.row.ID: head}}
			if err := reader.Entries().First(&e, authority.EntryID).Error; err != nil {
				return err
			}
			rec := recognitions[0]
			if e.ProviderID != authority.ProviderID || !e.CreatedAt.Equal(authority.CreatedAt) || e.WorkKey != authority.WorkKey || e.RelativePath != authority.RelativePath || e.Size != authority.Size || !rec.ManualOverride || rec.TMDBID == nil || *rec.TMDBID != authority.TMDBID || rec.Title != authority.Title || rec.MetadataJSON != authority.MetadataJSON {
				return errors.New("version_authority_changed")
			}
		}
		return nil
	})
}

func (r *quarkObservationLiveRuntime) retirementVersionStep(ctx context.Context, from, to uint64, authority quarkRetirementAuthority, removing bool) error {
	if to == from+2 {
		if err := r.retirementVersionFacts(ctx, from+1, "base", authority, !removing, false); err != nil {
			return err
		}
	} else if to != from+1 {
		return errors.New("unexpected_semantic_version_count")
	}
	return r.retirementVersionFacts(ctx, to, "delta", authority, false, removing)
}

func (r *quarkObservationLiveRuntime) retirementUnchanged(ctx context.Context, before, after quarkObservationMutationSnapshot, authority quarkRetirementAuthority) error {
	if after.Head != before.Head {
		if after.Head != before.Head+1 {
			return errors.New("unexpected_noop_head")
		}
		if err := r.retirementVersionFacts(ctx, after.Head, "base", authority, false, false); err != nil {
			return err
		}
	}
	// Content revision, artifact generation, ID, bytes, and mtime must be exact.
	after.Head = before.Head
	if mutationDigest(before) != mutationDigest(after) {
		return errors.New("semantic_noop_changed")
	}
	return nil
}

func retirementMetadata(r *quarkObservationLiveRuntime) (func(), error) {
	// Only the metadata result is synthetic. Both manual writers, revisions,
	// authorization, publication, and artifact workers remain production code.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != "/movie/42" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"title":"Isolated retirement manual match","original_language":"zh","release_date":"2024-01-01","genres":[{"id":18}]}`))
	}))
	client, err := tmdb.NewForTest("isolated-metadata", server.URL, server.Client())
	if err != nil {
		server.Close()
		return nil, err
	}
	metadata := NewMetadataSettingsService(r.db, NewAuditService(r.db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "isolated-metadata"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) { return client, nil }
	r.library.SetMetadataSettingsService(metadata)
	return server.Close, nil
}

// The digest includes non-JSON timestamps and private canonical keys. It stays
// in the private acceptance DB; only boolean continuity results leave it.
func (r *quarkObservationLiveRuntime) retirementUserDigest(ctx context.Context, authority quarkRetirementAuthority, updated int64) (string, error) {
	var favorites []models.PlayerMediaFavorite
	var history []models.PlayerPlaybackHistory
	if err := r.db.WithContext(ctx).Where("user_id = ?", r.actor.User.ID).Limit(2).Find(&favorites).Error; err != nil || len(favorites) != 1 || favorites[0].LibraryID != r.row.ID || favorites[0].WorkKey != authority.WorkKey || favorites[0].CreatedAt.IsZero() {
		return "", errors.New("favorite_state")
	}
	if err := r.db.WithContext(ctx).Where("user_id = ?", r.actor.User.ID).Limit(2).Find(&history).Error; err != nil || len(history) != 1 || history[0].Deleted || history[0].SourceKind != "server" || history[0].HistoryIdentity == "" || history[0].ClientUpdatedAt != updated || history[0].Position != 1 || history[0].Duration == nil || *history[0].Duration != 2 || history[0].Revision == 0 || history[0].CreatedAt.IsZero() {
		return "", errors.New("history_state")
	}
	return mutationDigest([]any{favorites, favorites[0].UserID, favorites[0].WorkKey, history, history[0].UserID, history[0].CreatedAt, history[0].UpdatedAt}), nil
}

func (r *quarkObservationLiveRuntime) retirementUserVisible(ctx context.Context, state quarkRetirementState, visible bool) error {
	digest, err := r.retirementUserDigest(ctx, state.Authority, state.HistoryUpdatedAt)
	if err != nil || digest != state.UserDigest {
		return errors.New("user_state_changed")
	}
	favorites, err := NewPlayerMediaStateService(r.db, r.library).Favorites(r.actor)
	if err != nil {
		return err
	}
	history, err := NewPlayerHistoryService(r.db, r.library).List(r.actor, 1, 10, "server")
	want := 0
	if visible {
		want = 1
	}
	if err != nil || len(favorites) != want || len(history.List) != want || history.Total != int64(want) {
		return errors.New("public_user_visibility")
	}
	return nil
}

func (r *quarkObservationLiveRuntime) retirementCreateUserState(ctx context.Context, in QuarkObservationRetirementLiveInput, state *quarkRetirementState) error {
	// SetFavorite itself is idempotent, including existing timestamps.
	if ok, err := NewPlayerMediaStateService(r.db, r.library).SetFavorite(r.actor, playerMediaStateItemID(r.row.ID, state.Authority.WorkKey), true); err != nil || !ok {
		return errors.New("favorite_write")
	}
	var count int64
	if err := r.db.WithContext(ctx).Model(&models.PlayerPlaybackHistory{}).Where("user_id = ?", r.actor.User.ID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		token := playerHistoryEntryToken(r.row.ID, encodeCatalogToken(state.Authority.WorkKey), state.Authority.EntryID)
		identity := playerHistoryCanonicalIdentity(r.row.ID, encodeCatalogToken(state.Authority.WorkKey), "movie", nil, nil, 0)
		duration := 2.0
		change := PlayerHistoryChange{SyncKey: playerHistoryCanonicalSyncKey(identity), HistoryIdentity: identity, SourceKind: "server", SourceID: "isolated-retirement", SourceName: "Isolated retirement", SourceLocator: in.Base.BaseURL, LibraryID: strconv.FormatUint(uint64(r.row.ID), 10), ItemID: token, ItemToken: token, MediaIdentity: token, Title: quarkRetirementTitle, Position: 1, Duration: &duration, UpdatedAt: state.HistoryUpdatedAt}
		result, err := NewPlayerHistoryService(r.db, r.library).SyncContext(ctx, r.actor, 0, []PlayerHistoryChange{change})
		if err != nil || len(result.Rejected) != 0 {
			return errors.New("history_write")
		}
	}
	digest, err := r.retirementUserDigest(ctx, state.Authority, state.HistoryUpdatedAt)
	if err != nil {
		return err
	}
	if state.UserDigest != "" && digest != state.UserDigest {
		return errors.New("user_state_changed")
	}
	state.UserDigest = digest
	return r.retirementUserVisible(ctx, *state, true)
}

func (r *quarkObservationLiveRuntime) retirementRoundComplete(ctx context.Context, round quarkRetirementRound) (bool, error) {
	scope, err := r.retirementScope(ctx)
	if err != nil {
		return false, err
	}
	if scope.CoverageRound != round.Expected || scope.CoverageStartedAt.Before(round.BeforeStarted) {
		return false, errors.New("coverage_round_changed")
	}
	status, err := r.observer.Status(ctx, scope.ID)
	if err != nil || status.ErrorCode != "" {
		return false, errors.New("observation_error")
	}
	return status.LastCompleteAt != nil && !status.LastCompleteAt.Before(scope.CoverageStartedAt) && status.PendingDirectories == 0 && status.PendingPublications == 0 && status.PathRepairs == 0 && status.PendingConfirmations == 0, nil
}

func (r *quarkObservationLiveRuntime) retirementCoverageEvidence(ctx context.Context, in QuarkObservationRetirementLiveInput, round quarkRetirementRound) error {
	if err := r.retirementParent(ctx, in); err != nil {
		return err
	}
	scope, err := r.retirementScope(ctx)
	if err != nil {
		return err
	}
	var batches []models.ProviderObservationBatch
	if err = r.db.WithContext(ctx).Where("scope_id = ? AND coverage_round = ? AND state = ?", scope.ID, round.Expected, "complete").Limit(5).Find(&batches).Error; err != nil {
		return err
	}
	// Both actual directories, not just an empty global pending counter, must
	// have complete independently read pages in this exact source generation.
	seen := map[string]bool{}
	for _, batch := range batches {
		if batch.DirectoryID != in.Base.Root.ID && batch.DirectoryID != in.Predecessor.DirectoryTVID || seen[batch.DirectoryID] || batch.Fence != scope.Fence || batch.CompletedAt == nil || batch.Pages < 1 || batch.IncompleteReason != "" || batch.Revision != batch.PublishedRevision {
			return errors.New("coverage_evidence_changed")
		}
		seen[batch.DirectoryID] = true
	}
	if len(batches) != 2 || !seen[in.Base.Root.ID] || !seen[in.Predecessor.DirectoryTVID] {
		return errors.New("coverage_evidence_incomplete")
	}
	return nil
}

// Persist ScanNow intent before requesting the new coverage. Recovery resumes
// precisely that round; it neither restarts a completed round nor clears leases.
func (r *quarkObservationLiveRuntime) retirementRound(ctx context.Context, in QuarkObservationRetirementLiveInput, phase string, state *quarkRetirementState, save func(string) error) error {
	scope, err := r.retirementScope(ctx)
	if err != nil {
		return err
	}
	if state.Round.Stage != phase {
		if scope.LastCompleteAt == nil || scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			return errors.New("prior_coverage_incomplete")
		}
		pages, e := r.observationPages(ctx)
		if e != nil {
			return e
		}
		state.Round = quarkRetirementRound{Stage: phase, Before: scope.CoverageRound, Expected: scope.CoverageRound + 1, BeforeStarted: scope.CoverageStartedAt, PagesBefore: pages, RequestsBefore: in.Base.RequestCount()}
		if err = save(phase); err != nil {
			return err
		}
	}
	if scope.CoverageRound == state.Round.Before {
		if !scope.CoverageStartedAt.Equal(state.Round.BeforeStarted) || scope.LastCompleteAt == nil || scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			return errors.New("coverage_intent_changed")
		}
		if _, err = r.library.ScanNow(ctx, r.actor, r.row.ID); err != nil {
			return err
		}
	}
	if !state.Round.CompletedAt.IsZero() {
		complete, e := r.retirementRoundComplete(ctx, state.Round)
		if e != nil || !complete {
			return errors.New("completed_coverage_changed")
		}
		return r.retirementCoverageEvidence(ctx, in, state.Round)
	}
	for steps := 0; steps < 100; steps++ {
		complete, e := r.retirementRoundComplete(ctx, state.Round)
		if e != nil {
			return e
		}
		if complete {
			scope, err = r.retirementScope(ctx)
			if err != nil {
				return err
			}
			pages, e := r.observationPages(ctx)
			if e != nil {
				return e
			}
			state.Round.Pages, state.Round.Requests = pages-state.Round.PagesBefore, in.Base.RequestCount()-state.Round.RequestsBefore
			state.Round.StartedAt, state.Round.CompletedAt = scope.CoverageStartedAt, *scope.LastCompleteAt
			if state.Round.Pages < 2 || state.Round.Requests < 2 {
				return errors.New("coverage_without_reads")
			}
			if e = r.retirementCoverageEvidence(ctx, in, state.Round); e != nil {
				return e
			}
			return save(phase)
		}
		if err = r.observer.RunDue(ctx, 1); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return errors.New("coverage_step_budget")
}

func retirementWait(ctx context.Context, in QuarkObservationRetirementLiveInput, missingAt time.Time) error {
	if missingAt.IsZero() || missingAt.Before(in.StartedAt) || missingAt.After(time.Now()) {
		return errors.New("missing_clock_invalid")
	}
	due := missingAt.Add(quarkRetirementWait)
	if !due.Before(in.Deadline) {
		return errors.New("deadline_cannot_cover_confirmation")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := time.Until(due)
		if left <= 0 {
			return nil
		}
		if in.Progress != nil {
			in.Progress(QuarkObservationRetirementProgress{Stage: "wait_confirm", WaitSeconds: int64((left + time.Second - 1) / time.Second)})
		}
		if left > 30*time.Second {
			left = 30 * time.Second
		}
		timer := time.NewTimer(left)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (r *quarkObservationLiveRuntime) retirementRevoked(ctx context.Context, in QuarkObservationRetirementLiveInput, state quarkRetirementState, filePresent bool) error {
	head, err := r.retirementAbsent(ctx)
	if err != nil {
		return err
	}
	if head != state.DeletionHead {
		return errors.New("retirement_publication")
	}
	if err = r.retirementVersionStep(ctx, state.Baseline.Head, head, state.Authority, true); err != nil {
		return err
	}
	var artifact models.MediaArtifact
	err = r.db.WithContext(ctx).First(&artifact, state.Baseline.ArtifactID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) && !filePresent && state.Report.RevokedBeforeArtifacts && state.Report.RevokedDBReopen {
		// The real scoped cleanup removes its consumed manifest receipt after
		// deleting that exact owned file. Revocation was verified before drain.
	} else if err != nil || artifact.Active || !artifact.Managed || artifact.SourceIdentity != fmt.Sprintf("entry:%d", state.Baseline.EntryID) || artifact.ProviderItemID != in.Base.Sample.ID {
		return errors.New("retirement_manifest")
	}
	if err = r.retirementUserVisible(ctx, state, false); err != nil {
		return err
	}
	if !filePresent {
		return r.mutationOldRemoved(ctx, state.Baseline)
	}
	path := filepath.Join(r.row.STRMLocalRoot, filepath.FromSlash(strings.TrimPrefix(state.Baseline.ArtifactRelativePath, "/")))
	bytes, err := os.ReadFile(path)
	if err != nil || len(bytes) > 16<<10 || mutationDigestBytes(bytes) != state.Baseline.ArtifactHash {
		return errors.New("owned_strm_changed_before_cleanup")
	}
	proxy := r.artifacts.signedProxy
	before := in.Base.RequestCount()
	if _, err = proxy.VerifyArtifactURL(strings.TrimSpace(string(bytes))); ErrorCode(err) != CodeProxyTargetUnavailable {
		return errors.New("old_signature_not_revoked")
	}
	parsed, err := proxy.parseArtifactURL(strings.TrimSpace(string(bytes)))
	if err != nil {
		return errors.New("old_signature_invalid")
	}
	if _, err = proxy.Resolve(ctx, parsed.Opaque, parsed.KeyID, parsed.Expiry, parsed.Signature, "isolated-retirement"); ErrorCode(err) != CodeProxyTargetUnavailable {
		return errors.New("old_resolve_not_revoked")
	}
	if _, err = proxy.ResolveArtifact(ctx, parsed.Opaque, "isolated-retirement"); ErrorCode(err) != CodeProxyTargetUnavailable || in.Base.RequestCount() != before {
		return errors.New("old_emby_not_revoked_locally")
	}
	var followup models.CatalogScanFollowup
	if err = r.db.WithContext(ctx).First(&followup, "library_id = ?", r.row.ID).Error; err != nil || !followup.ArtifactPending || followup.ArtifactGeneration != state.DeletionGeneration {
		return errors.New("retirement_artifacts_already_drained")
	}
	return nil
}

func (r *quarkObservationLiveRuntime) retirementRestored(ctx context.Context, in QuarkObservationRetirementLiveInput, state quarkRetirementState) (quarkObservationMutationSnapshot, error) {
	var empty quarkObservationMutationSnapshot
	authority, err := r.retirementAuthority(ctx, in)
	if err != nil || mutationDigest(authority) != mutationDigest(state.Authority) {
		return empty, errors.New("restored_authority_changed")
	}
	if err = r.retirementUserVisible(ctx, state, true); err != nil {
		return empty, err
	}
	current, err := r.mutationSnapshot(ctx, in.Base, retirementRelative(in))
	if err != nil || current.EntryID != state.Baseline.EntryID || !current.CreatedAt.Equal(state.Baseline.CreatedAt) || current.Generation != state.Baseline.Generation+2 || current.ContentRevision != state.Baseline.ContentRevision+2 {
		return empty, errors.New("restored_publication_changed")
	}
	// Once restored, a later no-op compaction is validated separately against
	// the last restored snapshot, rather than being confused with a third edit.
	if state.Restored.EntryID == 0 {
		if err = r.retirementVersionStep(ctx, state.DeletionHead, current.Head, state.Authority, false); err != nil {
			return empty, err
		}
	} else if err = r.retirementUnchanged(ctx, state.Restored, current, state.Authority); err != nil {
		return empty, err
	}
	item, err := r.retirementItem(ctx, in)
	if err != nil || item.DownlistedAt != nil || item.MissingSince != nil || item.MissingObservations != 0 {
		return empty, errors.New("restored_observation_changed")
	}
	return current, nil
}

// RunQuarkObservationRetirementLiveValidation has no request to delete/recycle,
// no clock override, and no source-state loader. Only its private checkpoint is
// written directly; all actual catalog/user/artifact writes use real services.
func RunQuarkObservationRetirementLiveValidation(ctx context.Context, in QuarkObservationRetirementLiveInput) (report QuarkObservationRetirementLiveReport, returnedError error) {
	report = QuarkObservationRetirementLiveReport{Version: 1, Stage: "input", ManualScanNow: true, SyntheticMetadata: true}
	defer func() {
		if returnedError != nil {
			returnedError = fmt.Errorf("quark_observation_retirement_%s", report.Stage)
		}
	}()
	if err := retirementInput(in); err != nil {
		return report, err
	}
	startRequests := in.Base.RequestCount()
	defer func() {
		report.Requests = in.Base.RequestCount() - startRequests
		if report.Requests < 0 || report.Requests > 220 {
			returnedError = errors.New("request_budget")
		}
	}()
	lock, err := database.AcquireExclusiveRuntime(filepath.Join(in.Base.StateDir, "observation.db"))
	if err != nil {
		return report, err
	}
	defer lock.Close()
	report.Stage = "existing_state"
	r, err := quarkObservationLiveOpen(ctx, in.Base, false)
	if err != nil {
		return report, err
	}
	defer func() {
		if e := r.close(); returnedError == nil {
			returnedError = e
		}
	}()
	contract, original, err := r.retirementContract(ctx, in)
	if err != nil {
		return report, err
	}
	var cp quarkObservationRetirementCheckpoint
	if r.db.Migrator().HasTable(&cp) {
		err = r.db.First(&cp, 1).Error
	} else {
		err = gorm.ErrRecordNotFound
	}
	var state quarkRetirementState
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if !time.Now().Before(in.Deadline) {
			return report, errors.New("deadline_expired")
		}
		current, e := r.mutationSnapshot(ctx, in.Base, retirementRelative(in))
		if e != nil || mutationDigest(current) != mutationDigest(original) {
			return report, errors.New("predecessor_state_changed")
		}
		item, e := r.retirementItem(ctx, in)
		if e != nil || item.MissingSince != nil || item.DownlistedAt != nil || item.MissingObservations != 0 {
			return report, errors.New("predecessor_missing_state")
		}
		if err = r.retirementParent(ctx, in); err != nil {
			return report, err
		}
		if err = r.db.AutoMigrate(&cp); err != nil {
			return report, err
		}
		state = quarkRetirementState{Original: original, HistoryUpdatedAt: time.Now().UnixMilli(), Report: report}
		cp = quarkObservationRetirementCheckpoint{ID: 1, Contract: contract, Stage: "manual_match", StateJSON: string(mustMutationJSON(state))}
		if err = r.db.Create(&cp).Error; err != nil {
			return report, err
		}
	} else if err != nil || cp.Contract != contract || len(cp.StateJSON) > 256<<10 || json.Unmarshal([]byte(cp.StateJSON), &state) != nil || !retirementStageValid(cp.Stage) || mutationDigest(state.Original) != mutationDigest(original) {
		return report, errors.New("checkpoint_contract_changed")
	}
	save := func(stage string) error {
		cp.Stage, cp.StateJSON = stage, string(mustMutationJSON(state))
		if !retirementStageValid(stage) || len(cp.StateJSON) > 256<<10 {
			return errors.New("checkpoint_invalid")
		}
		return r.db.Save(&cp).Error
	}
	if cp.Stage == "completed" {
		report.Stage = "completed_reopen"
		current, e := r.retirementRestored(ctx, in, state)
		if e != nil || mutationDigest(current) != mutationDigest(state.Restored) {
			return report, errors.New("completed_state_changed")
		}
		report = state.Report
		report.CompletedDBReopen, report.Stage = true, "completed"
		return report, nil
	}
	if !time.Now().Before(in.Deadline) {
		return report, errors.New("deadline_expired")
	}
	ctx, cancel := context.WithDeadline(ctx, in.Deadline)
	defer cancel()
	if err = r.mutationAccessSnapshot(in.Base); err != nil {
		return report, err
	}
	for steps := 0; steps < 32; steps++ {
		report = state.Report
		report.Stage = cp.Stage
		if err = ctx.Err(); err != nil {
			return report, err
		}
		if in.Progress != nil {
			in.Progress(QuarkObservationRetirementProgress{Stage: cp.Stage})
		}
		// Configuration and signing identity stay pinned at every stage, including
		// after DB reopen. The previous completed checkpoint is immutable.
		if nowContract, _, e := r.retirementContract(ctx, in); e != nil || nowContract != contract {
			return report, errors.New("scope_contract_changed")
		}
		switch cp.Stage {
		case "manual_match":
			authority, e := r.retirementAuthority(ctx, in)
			if e != nil {
				return report, e
			}
			if !authority.Manual || authority.TMDBID != 42 {
				closeMetadata, e := retirementMetadata(r)
				if e != nil {
					return report, e
				}
				_, e = r.library.OverrideRecognition(ctx, r.actor, r.row.ID, encodeRecognitionToken(authority.RecognitionID), MediaRecognitionOverrideInput{TMDBID: 42, MediaType: "movie"}, RequestContext{})
				closeMetadata()
				if e != nil {
					return report, e
				}
			}
			err = save("manual_edit")
		case "manual_edit":
			authority, e := r.retirementAuthority(ctx, in)
			if e != nil || !authority.Manual || authority.TMDBID != 42 {
				return report, errors.New("manual_match_changed")
			}
			doc, e := r.library.CatalogMetadata(ctx, r.actor, r.row.ID, encodeCatalogToken(authority.WorkKey))
			if e != nil {
				return report, e
			}
			if doc.Editable.Title != quarkRetirementTitle || doc.Editable.Overview != quarkRetirementOverview {
				doc.Editable.Title, doc.Editable.Overview = quarkRetirementTitle, quarkRetirementOverview
				if _, e = r.library.UpdateCatalogMetadata(ctx, r.actor, r.row.ID, encodeCatalogToken(authority.WorkKey), MediaMetadataUpdateInput{Revision: doc.Revision, Editable: doc.Editable}, RequestContext{}); e != nil {
					return report, e
				}
			}
			err = save("manual_artifacts")
		case "manual_artifacts":
			if err = r.queue.RecoverExpiredLeases(); err != nil {
				return report, err
			}
			if err = r.drain(ctx); err != nil {
				return report, err
			}
			state.Baseline, err = r.mutationSnapshot(ctx, in.Base, retirementRelative(in))
			if err != nil || state.Baseline.EntryID != state.Original.EntryID || !state.Baseline.CreatedAt.Equal(state.Original.CreatedAt) || state.Baseline.Head != state.Original.Head+2 {
				return report, errors.New("manual_publication_changed")
			}
			state.Authority, err = r.retirementAuthority(ctx, in)
			if err != nil || !state.Authority.Manual || state.Authority.TMDBID != 42 || state.Authority.Title != quarkRetirementTitle {
				return report, errors.New("manual_authority_missing")
			}
			snapshot, e := r.snapshotAt(ctx, in.Base, retirementRelative(in), true)
			if e != nil {
				return report, e
			}
			lease, e := r.artifacts.signedProxy.inspectArtifactURL(strings.TrimSpace(string(snapshot.data)))
			if e != nil || !lease.ExpiresAt.After(in.Deadline) {
				return report, errors.New("signature_lifetime_insufficient")
			}
			err = save("user_state")
		case "user_state":
			if err = r.retirementCreateUserState(ctx, in, &state); err != nil {
				return report, err
			}
			err = save("move_out")
		case "move_out", "move_back":
			if err = in.Mutate(ctx, cp.Stage); err != nil {
				return report, err
			}
			next := "first_observe"
			if cp.Stage == "move_back" {
				next = "restore_observe"
			}
			err = save(next)
		case "first_observe":
			if err = r.retirementRound(ctx, in, cp.Stage, &state, save); err != nil {
				return report, err
			}
			item, e := r.retirementItem(ctx, in)
			if e != nil || item.MissingLastAt == nil || item.MissingSince == nil || item.MissingObservations != 1 || item.DownlistedAt != nil || item.MissingRound != state.Round.Expected || item.MissingBatchID == "" || item.MissingLastAt.Before(in.StartedAt) {
				return report, errors.New("first_missing_not_observed")
			}
			current, e := r.mutationSnapshot(ctx, in.Base, retirementRelative(in))
			if e != nil {
				return report, errors.New("first_missing_changed_media")
			}
			if e = r.retirementUnchanged(ctx, state.Baseline, current, state.Authority); e != nil {
				return report, e
			}
			state.Baseline = current
			if err = r.retirementUserVisible(ctx, state, true); err != nil {
				return report, err
			}
			state.FirstMissingAt, state.FirstMissingBatch, state.FirstMissingRound, state.FirstRound = *item.MissingLastAt, item.MissingBatchID, item.MissingRound, state.Round
			err = save("wait_confirm")
		case "wait_confirm":
			item, e := r.retirementItem(ctx, in)
			if e != nil || item.MissingLastAt == nil || !item.MissingLastAt.Equal(state.FirstMissingAt) || item.MissingRound != state.FirstMissingRound || item.MissingBatchID != state.FirstMissingBatch || item.MissingObservations != 1 || item.DownlistedAt != nil {
				return report, errors.New("waiting_missing_changed")
			}
			if err = retirementWait(ctx, in, state.FirstMissingAt); err != nil {
				return report, err
			}
			err = save("second_observe")
		case "second_observe":
			if time.Now().Before(state.FirstMissingAt.Add(quarkRetirementWait)) {
				return report, errors.New("confirmation_too_early")
			}
			if err = r.retirementRound(ctx, in, cp.Stage, &state, save); err != nil {
				return report, err
			}
			item, e := r.retirementItem(ctx, in)
			if e != nil || item.MissingLastAt == nil || item.DownlistedAt == nil || item.MissingObservations != 2 || item.MissingRound != state.Round.Expected || item.MissingRound <= state.FirstMissingRound || item.MissingBatchID == state.FirstMissingBatch || item.MissingLastAt.Sub(state.FirstMissingAt) < quarkRetirementWait || !state.Round.StartedAt.After(state.FirstRound.CompletedAt) {
				return report, errors.New("second_missing_not_confirmed")
			}
			state.Report.TwoCompleteCoverages, state.Report.MissingIntervalSatisfied = true, true
			state.Report.ObservedIntervalSeconds = int64(item.MissingLastAt.Sub(state.FirstMissingAt) / time.Second)
			state.DeletionHead, err = r.retirementAbsent(ctx)
			if err != nil {
				return report, err
			}
			var library models.MediaLibrary
			var followup models.CatalogScanFollowup
			if err = r.db.First(&library, r.row.ID).Error; err != nil {
				return report, err
			}
			if err = r.db.First(&followup, "library_id = ?", r.row.ID).Error; err != nil || !followup.ArtifactPending || followup.ArtifactGeneration != state.Baseline.Generation+1 || library.ContentRevision != state.Baseline.ContentRevision+1 {
				return report, errors.New("retirement_semantic_generation")
			}
			state.DeletionGeneration = followup.ArtifactGeneration
			err = save("revoked_verify")
		case "revoked_verify":
			if err = r.retirementRevoked(ctx, in, state, true); err != nil {
				return report, err
			}
			state.Report.RevokedBeforeArtifacts = true
			err = save("revoked_reopen")
		case "revoked_reopen":
			before := in.Base.RequestCount()
			if err = r.close(); err != nil {
				return report, err
			}
			r, err = quarkObservationLiveOpen(ctx, in.Base, false)
			if err != nil {
				return report, err
			}
			if err = r.retirementRevoked(ctx, in, state, true); err != nil || in.Base.RequestCount() != before {
				return report, errors.New("revoked_reopen_failed")
			}
			state.Report.RevokedDBReopen = true
			err = save("retire_artifacts")
		case "retire_artifacts":
			if err = r.queue.RecoverExpiredLeases(); err != nil {
				return report, err
			}
			if err = r.drain(ctx); err != nil {
				return report, err
			}
			if err = r.retirementRevoked(ctx, in, state, false); err != nil {
				return report, err
			}
			var run models.MediaArtifactRun
			if err = r.db.First(&run, "library_id = ? AND generation = ?", r.row.ID, state.DeletionGeneration).Error; err != nil || run.Status != models.MediaArtifactStatusCompleted {
				return report, errors.New("retirement_worker_incomplete")
			}
			state.DeletionRunID, state.Report.OwnedSTRMRemoved = run.ID, true
			err = save("move_back")
		case "restore_observe":
			if err = r.retirementRound(ctx, in, cp.Stage, &state, save); err != nil {
				return report, err
			}
			authority, e := r.retirementAuthority(ctx, in)
			if e != nil || mutationDigest(authority) != mutationDigest(state.Authority) {
				return report, errors.New("restoration_not_observed")
			}
			err = save("restore_artifacts")
		case "restore_artifacts":
			if err = r.queue.RecoverExpiredLeases(); err != nil {
				return report, err
			}
			if err = r.drain(ctx); err != nil {
				return report, err
			}
			state.Restored, err = r.retirementRestored(ctx, in, state)
			if err != nil {
				return report, err
			}
			state.Report.LatestManualMetadataPreserved, state.Report.NonemptyFavoritePreserved, state.Report.NonemptyHistoryPreserved = true, true, true
			err = save("late_cleanup")
		case "late_cleanup":
			before := in.Base.RequestCount()
			cleanup := NewSTRMManagementService(r.db, NewAuditService(r.db), r.queue, r.library, r.artifacts)
			result := cleanup.AutoCleanup(ctx, state.DeletionRunID)
			current, e := r.retirementRestored(ctx, in, state)
			if e != nil || result.Removed != 0 || mutationDigest(current) != mutationDigest(state.Restored) || in.Base.RequestCount() != before {
				return report, errors.New("late_cleanup_changed_restoration")
			}
			state.Report.LateCleanupSafe = true
			err = save("noop_observe")
		case "noop_observe":
			if err = r.retirementRound(ctx, in, cp.Stage, &state, save); err != nil {
				return report, err
			}
			state.Report.NoopPages, state.Report.NoopRequests = state.Round.Pages, state.Round.Requests
			err = save("noop_artifacts")
		case "noop_artifacts":
			if err = r.drain(ctx); err != nil {
				return report, err
			}
			current, e := r.retirementRestored(ctx, in, state)
			if e != nil {
				return report, errors.New("noop_changed_restoration")
			}
			if e = r.retirementUnchanged(ctx, state.Restored, current, state.Authority); e != nil {
				return report, e
			}
			state.Restored = current
			state.Report.NoopUnchanged = true
			err = save("completed_reopen")
		case "completed_reopen":
			before := in.Base.RequestCount()
			if err = r.close(); err != nil {
				return report, err
			}
			r, err = quarkObservationLiveOpen(ctx, in.Base, false)
			if err != nil {
				return report, err
			}
			current, e := r.retirementRestored(ctx, in, state)
			if e != nil || mutationDigest(current) != mutationDigest(state.Restored) || in.Base.RequestCount() != before {
				return report, errors.New("completed_reopen_failed")
			}
			state.Report.CompletedDBReopen, state.Report.Completed, state.Report.Stage = true, true, "completed"
			if err = save("completed"); err != nil {
				return report, err
			}
			return state.Report, nil
		default:
			return report, errors.New("checkpoint_stage_invalid")
		}
		if err != nil {
			return report, err
		}
	}
	return report, errors.New("stage_budget")
}
