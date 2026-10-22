//go:build quark_live

package services

// This follow-on harness is excluded from production. Only the owning handler
// can mutate its receipt-owned fixture; callbacks contribute no catalog facts.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

type QuarkObservationMutationLiveInput struct {
	Base          QuarkObservationLiveInput
	RunID         string
	RenamedName   string
	DirectoryName string
	// The handler freezes all targets and operation IDs in its separate journal.
	// Unknown/pending must stop, never resubmit under a new ID or another route.
	Mutate func(context.Context, string) error
}

type QuarkObservationMutationLiveReport struct {
	Version                  int    `json:"version"`
	Stage                    string `json:"stage"`
	ReusedDatabase           bool   `json:"reused_database"`
	RenameObserved           bool   `json:"rename_observed"`
	MoveObserved             bool   `json:"move_observed"`
	StableIdentity           bool   `json:"stable_identity"`
	OldOwnedArtifactsRemoved bool   `json:"old_owned_artifacts_removed"`
	NoopUnchanged            bool   `json:"noop_unchanged"`
	NoopPages                int64  `json:"noop_pages"`
	NoopRequests             int    `json:"noop_requests"`
	Requests                 int    `json:"requests"`
	ReadyNotifications       int    `json:"ready_notifications_this_process"`
	CompletedReopen          bool   `json:"completed_reopen"`
	ManualScanNow            bool   `json:"manual_scan_now"`
	AutomaticTimingTested    bool   `json:"automatic_timing_tested"`
	PlaybackTested           bool   `json:"playback_tested"`
	STRMCount                int    `json:"strm_count"`
}

// Private checkpoint only: no provider facts are inserted into catalog tables.
// One follow-on run per isolated DB prevents a new RunID from repeating writes.
type quarkObservationMutationCheckpoint struct {
	ID           uint `gorm:"primaryKey"`
	Contract     string
	Stage        string
	BaselineJSON string
	PreviousJSON string
	ReportJSON   string
}

func (quarkObservationMutationCheckpoint) TableName() string { return "quark_live_mutation_checkpoint" }

type quarkObservationMutationSnapshot struct {
	EntryID              uint
	CreatedAt            time.Time
	Head                 uint64
	RelativePath         string
	ArtifactID           uint
	ArtifactRelativePath string
	ArtifactHash         string
	ArtifactMtime        time.Time
	Generation           uint64
	ContentRevision      uint64
}

func mutationDigest(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func quarkMutationNameValid(name string) bool {
	return name != "" && len(name) < 100 && name != "." && name != ".." && strings.TrimSpace(name) == name && !strings.ContainsAny(name, "/\\:\x00\r\n")
}

func (r *quarkObservationLiveRuntime) mutationContract(ctx context.Context, in QuarkObservationMutationLiveInput) (string, error) {
	var library models.MediaLibrary
	var storage models.Storage
	var connection models.Connection
	var keys []models.ProxySigningKey
	var profile models.MediaClassificationProfile
	var head models.CatalogHead
	var scopes []models.ProviderObservationScope
	var count int64
	if err := r.db.WithContext(ctx).Model(&models.MediaLibrary{}).Count(&count).Error; err != nil || count != 1 {
		return "", errors.New("scope_count")
	}
	if err := r.db.WithContext(ctx).First(&library, r.row.ID).Error; err != nil {
		return "", err
	}
	if err := r.db.WithContext(ctx).First(&profile, library.ProfileID).Error; err != nil {
		return "", err
	}
	if err := r.db.WithContext(ctx).First(&storage, library.StorageID).Error; err != nil || storage.ConnectionID == nil {
		return "", errors.New("storage_scope")
	}
	if err := r.db.WithContext(ctx).First(&connection, *storage.ConnectionID).Error; err != nil {
		return "", err
	}
	if !library.Enabled || !library.Recursive || !library.STRMEnabled || library.MetadataArtifactsEnabled || library.UploadSidecars || library.CloudEmptyCleanupEnabled || library.IngestEnabled || library.ProviderRootID != in.Base.Root.ID || library.RelativeRoot != "/" || filepath.Clean(library.STRMLocalRoot) != filepath.Join(in.Base.StateDir, "projection") || !storage.Enabled || storage.Type != models.StorageTypeQuark || storage.RootPath != in.Base.Root.ID || storage.RootDisplayPath != in.Base.RootPath || !connection.Enabled || connection.Provider != models.ConnectionProviderQuark {
		return "", errors.New("scope_changed")
	}
	bundle, err := quark.ParseCredentials(in.Base.Credentials)
	if err != nil || bundle.TV.AccountID != connection.AccountID {
		return "", errors.New("identity_changed")
	}
	if err = r.db.WithContext(ctx).Order("id").Find(&keys).Error; err != nil || len(keys) != 1 {
		return "", errors.New("signing_identity_changed")
	}
	if err = r.db.WithContext(ctx).First(&head, "library_id = ?", library.ID).Error; err != nil {
		return "", err
	}
	if err = r.db.WithContext(ctx).Where("library_id = ?", library.ID).Find(&scopes).Error; err != nil || len(scopes) != 1 {
		return "", errors.New("observation_scope_changed")
	}
	scope := scopes[0]
	if !scope.Enabled || scope.Provider != "quark" || scope.Kind != "library" || scope.RootID != in.Base.Root.ID || scope.ConnectionID != connection.ID || scope.LastCompleteAt == nil {
		return "", errors.New("observation_scope_changed")
	}
	keyBytes, err := os.ReadFile(filepath.Join(in.Base.StateDir, "validation.key"))
	if err != nil {
		return "", err
	}
	return mutationDigest([]any{in.RunID, in.RenamedName, in.DirectoryName, in.Base.BaseURL, in.Base.Root.ID, in.Base.Sample.ID, in.Base.Sample.Size, in.Base.Sample.Name, library.ID, library.StorageID, library.ProfileID, library.ProfileRevision, catalogSourceFingerprint(library, storage), catalogConfigFingerprint(library, storage, profile), head.SourceEpoch, scope.ID, scope.Fence, scope.AccountKey, scope.RootPath, connection.ID, connection.AccountID, connection.Revision, keys, mutationDigest(keyBytes)}), nil
}

// Only identity-continuous access snapshots may replace the isolated ciphertext.
// Neither connection configuration revision nor provider proof is rewritten.
func (r *quarkObservationLiveRuntime) mutationAccessSnapshot(in QuarkObservationLiveInput) error {
	var storage models.Storage
	var connection models.Connection
	if err := r.db.First(&storage, r.row.StorageID).Error; err != nil || storage.ConnectionID == nil {
		return errors.New("storage_scope")
	}
	if err := r.db.First(&connection, *storage.ConnectionID).Error; err != nil {
		return err
	}
	store, err := credential.Open(filepath.Join(in.StateDir, "validation.key"), "")
	if err != nil {
		return err
	}
	oldRaw, err := store.Decrypt(connectionPurpose(connection.ID, connection.Provider), connection.CredentialCiphertext)
	if err != nil {
		return err
	}
	old, err := quark.ParseCredentials(json.RawMessage(oldRaw))
	if err != nil {
		return err
	}
	next, err := quark.ParseCredentials(in.Credentials)
	if err != nil {
		return err
	}
	same := func(a, b *quark.Authorization, renewedTV bool) bool {
		return a == nil && b == nil || a != nil && b != nil && a.AccountID == b.AccountID && a.DeviceID == b.DeviceID && (a.Generation == b.Generation || renewedTV && b.Generation > a.Generation) && a.IdentityVerified == b.IdentityVerified
	}
	if !same(old.TV, next.TV, true) || !same(old.Agent, next.Agent, false) || mutationDigest(old.Proofs) != mutationDigest(next.Proofs) {
		return errors.New("authorization_identity_changed")
	}
	sealed, err := store.Encrypt(connectionPurpose(connection.ID, connection.Provider), string(in.Credentials))
	if err != nil {
		return err
	}
	return r.db.Model(&connection).Update("credential_ciphertext", sealed).Error
}

func (r *quarkObservationLiveRuntime) mutationSnapshot(ctx context.Context, in QuarkObservationLiveInput, relative string) (quarkObservationMutationSnapshot, error) {
	var out quarkObservationMutationSnapshot
	s, err := r.snapshotAt(ctx, in, relative, true)
	if err != nil {
		return out, err
	}
	var library models.MediaLibrary
	var artifacts []models.MediaArtifact
	if err = r.db.WithContext(ctx).First(&library, r.row.ID).Error; err != nil {
		return out, err
	}
	if err = r.db.WithContext(ctx).Where("library_id = ? AND active = ? AND kind = ?", r.row.ID, true, models.MediaArtifactKindSTRM).Find(&artifacts).Error; err != nil {
		return out, err
	}
	if len(artifacts) != 1 {
		return out, errors.New("artifact_owner_count")
	}
	a := artifacts[0]
	verifier, err := r.artifacts.signedProxy.activeSigningVerifier()
	if err != nil {
		return out, err
	}
	if _, err = r.artifacts.signedProxy.inspectArtifactLeaseURL(strings.TrimSpace(string(s.data)), a, verifier); err != nil {
		return out, errors.New("signed_artifact_invalid")
	}
	path := filepath.Join(library.STRMLocalRoot, filepath.FromSlash(strings.TrimPrefix(a.RelativePath, "/")))
	if !a.Managed || a.SourceIdentity != fmt.Sprintf("entry:%d", s.row.ID) || a.ProviderItemID != in.Sample.ID || a.TargetKind != models.MediaArtifactTargetLocalProjection || filepath.Clean(path) != s.path || a.ContentFingerprint != mutationDigestBytes(s.data) || library.ArtifactAppliedGeneration != library.ArtifactGeneration {
		return out, errors.New("artifact_owner_mismatch")
	}
	out = quarkObservationMutationSnapshot{EntryID: s.row.ID, CreatedAt: s.row.CreatedAt, Head: s.head.Revision, RelativePath: relative, ArtifactID: a.ID, ArtifactRelativePath: a.RelativePath, ArtifactHash: mutationDigestBytes(s.data), ArtifactMtime: s.mtime, Generation: library.ArtifactGeneration, ContentRevision: library.ContentRevision}
	return out, nil
}

func mutationDigestBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func (r *quarkObservationLiveRuntime) mutationOldRemoved(ctx context.Context, previous quarkObservationMutationSnapshot) error {
	var old models.MediaArtifact
	err := r.db.WithContext(ctx).First(&old, previous.ArtifactID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && old.Active {
		return errors.New("old_artifact_active")
	}
	oldPath := filepath.Join(r.row.STRMLocalRoot, filepath.FromSlash(strings.TrimPrefix(previous.ArtifactRelativePath, "/")))
	if _, err = os.Lstat(oldPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("old_owned_artifact_remaining")
	}
	return nil
}

// RunQuarkObservationMutationLiveValidation never initializes a media DB. The
// handler owns independent cumulative 200 API / 220 total / 12 minute limits.
// This service uses real ScanNow rounds and a ten-minute invocation deadline;
// successful rounds do not establish natural 1/2-minute scheduling latency.
func RunQuarkObservationMutationLiveValidation(ctx context.Context, in QuarkObservationMutationLiveInput) (report QuarkObservationMutationLiveReport, returnedError error) {
	report = QuarkObservationMutationLiveReport{Version: 1, Stage: "input", ManualScanNow: true}
	defer func() {
		if returnedError != nil {
			returnedError = fmt.Errorf("quark_observation_mutation_%s", report.Stage)
		}
	}()
	if err := quarkObservationLiveInputModeCheck(in.Base, false); err != nil {
		return report, err
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{8,64}$`).MatchString(in.RunID) || !quarkMutationNameValid(in.RenamedName) || !strings.EqualFold(filepath.Ext(in.RenamedName), ".mp4") || in.RenamedName == in.Base.Sample.Name || !quarkMutationNameValid(in.DirectoryName) || in.Mutate == nil {
		return report, errors.New("invalid_mutation_contract")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	start := in.Base.RequestCount()
	defer func() {
		report.Requests = in.Base.RequestCount() - start
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
		report.ReadyNotifications = r.ready
		if e := r.close(); returnedError == nil {
			returnedError = e
		}
	}()
	contract, err := r.mutationContract(ctx, in)
	if err != nil {
		return report, err
	}
	report.ReusedDatabase = true
	if !r.db.Migrator().HasTable(&quarkObservationMutationCheckpoint{}) {
		if err = r.db.Migrator().CreateTable(&quarkObservationMutationCheckpoint{}); err != nil {
			return report, err
		}
	}
	var checkpoint quarkObservationMutationCheckpoint
	err = r.db.First(&checkpoint, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		baseline, e := r.mutationSnapshot(ctx, in.Base, "/"+in.Base.Sample.Name)
		if e != nil {
			return report, e
		}
		raw, _ := json.Marshal(baseline)
		checkpoint = quarkObservationMutationCheckpoint{ID: 1, Contract: contract, Stage: "rename_one", BaselineJSON: string(raw), PreviousJSON: string(raw)}
		if err = r.db.Create(&checkpoint).Error; err != nil {
			return report, err
		}
	} else if err != nil {
		return report, err
	}
	if checkpoint.Contract != contract {
		return report, errors.New("checkpoint_contract_changed")
	}
	var baseline, previous quarkObservationMutationSnapshot
	if json.Unmarshal([]byte(checkpoint.BaselineJSON), &baseline) != nil || json.Unmarshal([]byte(checkpoint.PreviousJSON), &previous) != nil || baseline.EntryID == 0 {
		return report, errors.New("checkpoint_invalid")
	}
	save := func(stage string, s quarkObservationMutationSnapshot) error {
		raw, _ := json.Marshal(s)
		checkpoint.Stage, checkpoint.PreviousJSON = stage, string(raw)
		return r.db.Save(&checkpoint).Error
	}
	finalRelative := "/" + in.DirectoryName + "/" + in.RenamedName
	if checkpoint.Stage == "completed" {
		report.Stage = "completed_reopen"
		current, e := r.mutationSnapshot(ctx, in.Base, finalRelative)
		if e != nil || mutationDigest(current) != mutationDigest(previous) {
			return report, errors.New("completed_state_changed")
		}
		if json.Unmarshal([]byte(checkpoint.ReportJSON), &report) != nil {
			return report, errors.New("report_invalid")
		}
		report.CompletedReopen = true
		report.Stage = "completed"
		return report, nil
	}
	if err = r.mutationAccessSnapshot(in.Base); err != nil {
		return report, err
	}
	for _, step := range []struct{ stage, relative, next string }{{"rename_one", "/" + in.RenamedName, "mkdir_move_one"}, {"mkdir_move_one", finalRelative, "noop"}} {
		if checkpoint.Stage != step.stage && checkpoint.Stage != step.stage+"_observe" && checkpoint.Stage != step.stage+"_artifacts" {
			continue
		}
		report.Stage = step.stage
		if currentContract, e := r.mutationContract(ctx, in); e != nil || currentContract != contract {
			return report, errors.New("scope_contract_changed")
		}
		if checkpoint.Stage == step.stage {
			// Intent already persisted. A retry calls only the same frozen actor stage.
			if err = in.Mutate(ctx, step.stage); err != nil {
				return report, err
			}
			if err = save(step.stage+"_observe", previous); err != nil {
				return report, err
			}
		}
		if checkpoint.Stage == step.stage+"_observe" {
			report.Stage = step.stage + "_scan"
			beforePages, e := r.observationPages(ctx)
			if e != nil {
				return report, e
			}
			beforeRequests := in.Base.RequestCount()
			if err = r.round(ctx); err != nil {
				return report, err
			}
			afterPages, e := r.observationPages(ctx)
			if e != nil || afterPages <= beforePages || in.Base.RequestCount() <= beforeRequests {
				return report, errors.New("mutation_not_observed")
			}
			// Do not leave the observation phase merely because a directory round
			// completed. Eventual old listings must be re-read on the next invocation.
			observed, e := r.snapshotAt(ctx, in.Base, step.relative, false)
			if e != nil || observed.row.ID != baseline.EntryID || !observed.row.CreatedAt.Equal(baseline.CreatedAt) || observed.head.Revision != previous.Head+1 {
				return report, errors.New("mutation_not_yet_visible")
			}
			if err = save(step.stage+"_artifacts", previous); err != nil {
				return report, err
			}
		}
		report.Stage = step.stage + "_artifacts"
		// A crashed worker keeps its real lease/physical owner. Only the ordinary
		// expiration path may recover it; no clock override or lease reset here.
		if err = r.queue.RecoverExpiredLeases(); err != nil {
			return report, err
		}
		if err = r.drain(ctx); err != nil {
			return report, err
		}
		report.Stage = step.stage + "_verify"
		current, e := r.mutationSnapshot(ctx, in.Base, step.relative)
		if e != nil {
			return report, e
		}
		if current.EntryID != baseline.EntryID || !current.CreatedAt.Equal(baseline.CreatedAt) || current.Head != previous.Head+1 || current.Generation != previous.Generation+1 || current.ArtifactID == previous.ArtifactID {
			return report, errors.New("mutation_identity_or_publication_changed")
		}
		if err = r.mutationOldRemoved(ctx, previous); err != nil {
			return report, err
		}
		if err = save(step.next, current); err != nil {
			return report, err
		}
		previous = current
	}
	if checkpoint.Stage != "noop" {
		return report, errors.New("checkpoint_stage_invalid")
	}
	report.RenameObserved, report.MoveObserved, report.StableIdentity, report.OldOwnedArtifactsRemoved = true, true, true, true
	report.Stage = "noop_observation"
	beforePages, err := r.observationPages(ctx)
	if err != nil {
		return report, err
	}
	beforeRequests, ready := in.Base.RequestCount(), r.ready
	if err = r.round(ctx); err != nil {
		return report, err
	}
	afterPages, err := r.observationPages(ctx)
	if err != nil {
		return report, err
	}
	report.NoopPages, report.NoopRequests = afterPages-beforePages, in.Base.RequestCount()-beforeRequests
	if report.NoopPages < 1 || report.NoopRequests < 1 {
		return report, errors.New("noop_not_observed")
	}
	if err = r.drain(ctx); err != nil {
		return report, err
	}
	current, err := r.mutationSnapshot(ctx, in.Base, finalRelative)
	if err != nil || !bytes.Equal(mustMutationJSON(previous), mustMutationJSON(current)) || r.ready != ready {
		return report, errors.New("noop_changed_state")
	}
	report.NoopUnchanged = true
	report.STRMCount = 1
	report.Stage = "completed"
	report.ReadyNotifications = r.ready
	report.Requests = in.Base.RequestCount() - start
	raw, _ := json.Marshal(report)
	checkpoint.ReportJSON = string(raw)
	if err = save("completed", current); err != nil {
		return report, err
	}
	return report, nil
}

func mustMutationJSON(value any) []byte { b, _ := json.Marshal(value); return b }
