package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrObservationFence       = errors.New("provider observation source changed")
	ErrObservationDeferred    = errors.New("provider observation deferred")
	ErrObservationInvalid     = errors.New("provider observation evidence invalid")
	ErrObservationUnavailable = errors.New("provider observation integration unavailable")
)

const (
	observationPageLimit         = 250
	observationSlicePages        = 5
	observationSliceDuration     = 15 * time.Second
	observationRequestInterval   = 2 * time.Second
	observationHourlyBudget      = 600
	observationMissingInterval   = 10 * time.Minute
	observationRecoveryRetention = 30 * 24 * time.Hour
)

// ObservationFact contains verified, credential-free provider metadata. IDs
// must already be qualified in the reader's adapter/account namespace.
type ObservationFact struct {
	ID, ParentID, Name     string
	IsDir                  bool
	Size                   int64
	ModifiedAt             time.Time
	ContentVersion, Digest string
}

type ObservationDirectoryPage struct {
	Items   []ObservationFact
	Offset  int64
	HasMore bool
	Total   *int64
	// Partial marks a provider-declared incomplete response. Positive facts may
	// still be used, but this traversal can never supply absence evidence.
	Partial bool
}

type ObservationDirectoryReader interface {
	ObserveDirectory(context.Context, models.ProviderObservationScope, string, int64, int) (ObservationDirectoryPage, error)
}

type ObservationScopeInput struct {
	LibraryID, ConnectionID                             uint
	AccountKey, Provider, Kind, RootID, RootPath, Fence string
	AutoIngest                                          bool
}

type ObservationChange struct {
	Fact                             ObservationFact
	RelativePath, PreviousPath, Kind string
	// Absence is observation evidence, never permission to delete cloud data.
	MissingReason string
	Revision      uint64
}

type ObservationPublication struct {
	Scope    models.ProviderObservationScope
	BatchID  string
	Revision uint64
	Changes  []ObservationChange
}

// PublishObservation must invoke acknowledge within the SAME database
// transaction as the catalog switch and artifact follow-up receipt. Network,
// recognition, file generation and media-server refresh stay outside it.
type ObservationPublisher interface {
	PublishObservation(context.Context, ObservationPublication, func(*gorm.DB) error) error
}

type ProviderObservationService struct {
	log       zerolog.Logger
	db        *gorm.DB
	now       func() time.Time
	runtimeID string
	reader    func(context.Context, models.ProviderObservationScope) (ObservationDirectoryReader, error)
	validate  func(*gorm.DB, models.ProviderObservationScope) error
	publisher ObservationPublisher
	progress  func(context.Context, models.ProviderObservationScope, ProviderObservationStatus) error
}

func NewProviderObservationService(db *gorm.DB) *ProviderObservationService {
	return &ProviderObservationService{db: db, now: func() time.Time { return time.Now().UTC() }, runtimeID: uuid.NewString(), log: zerolog.Nop()}
}

func (s *ProviderObservationService) SetLogger(log zerolog.Logger) { s.log = log }

func (s *ProviderObservationService) SetReaderResolver(f func(context.Context, models.ProviderObservationScope) (ObservationDirectoryReader, error)) {
	s.reader = f
}
func (s *ProviderObservationService) SetFenceValidator(f func(*gorm.DB, models.ProviderObservationScope) error) {
	s.validate = f
}
func (s *ProviderObservationService) SetPublisher(p ObservationPublisher) { s.publisher = p }
func (s *ProviderObservationService) SetProgressListener(f func(context.Context, models.ProviderObservationScope, ProviderObservationStatus) error) {
	s.progress = f
}

func (s *ProviderObservationService) BindScanRunTx(tx *gorm.DB, scopeID string, runID uint) error {
	if runID == 0 {
		return ErrObservationInvalid
	}
	var scope models.ProviderObservationScope
	if err := tx.First(&scope, "id = ? AND enabled = ?", scopeID, true).Error; err != nil {
		return err
	}
	var run models.MediaLibraryScanRun
	if err := tx.First(&run, "id = ? AND library_id = ?", runID, scope.LibraryID).Error; err != nil {
		return err
	}
	return tx.Model(&scope).Update("requested_scan_run_id", runID).Error
}

// EnsureScope does not replace old identity evidence. A changed root, identity
// or Profile creates a fresh scope and disables the old one in one transaction.
func (s *ProviderObservationService) EnsureScope(ctx context.Context, in ObservationScopeInput) (models.ProviderObservationScope, error) {
	var out models.ProviderObservationScope
	if in.LibraryID == 0 || in.ConnectionID == 0 || in.AccountKey == "" || in.RootID == "" || in.Fence == "" || in.Provider == "" || (in.Kind != "library" && in.Kind != "intake") || (in.Kind == "library" && in.AutoIngest) {
		return out, ErrObservationInvalid
	}
	clean, err := observationRootPath(in.RootPath)
	if err != nil {
		return out, err
	}
	now := s.now()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous models.ProviderObservationScope
		err := tx.Where("library_id = ? AND kind = ? AND enabled = ?", in.LibraryID, in.Kind, true).First(&previous).Error
		if err == nil && previous.Fence == in.Fence && previous.RootID == in.RootID && previous.AccountKey == in.AccountKey && previous.ConnectionID == in.ConnectionID && previous.RootPath == clean && previous.AutoIngest == in.AutoIngest {
			out = previous
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var scopes []models.ProviderObservationScope
		if err = tx.Where("account_key = ? AND enabled = ?", in.AccountKey, true).Find(&scopes).Error; err != nil {
			return err
		}
		for _, other := range scopes {
			if other.ID == previous.ID {
				continue
			}
			if (in.Kind == "intake" || other.Kind == "intake") && observationPathsOverlap(clean, other.RootPath) {
				return errors.New("provider intake and library roots overlap")
			}
		}
		if previous.ID != "" {
			if err = tx.Model(&models.ProviderObservationScope{}).Where("id = ?", previous.ID).Update("enabled", false).Error; err != nil {
				return err
			}
		}
		out = models.ProviderObservationScope{ID: uuid.NewString(), LibraryID: in.LibraryID, ConnectionID: in.ConnectionID, AccountKey: in.AccountKey, Provider: in.Provider, Kind: in.Kind, RootID: in.RootID, RootPath: clean, Fence: in.Fence, Enabled: true, AutoIngest: in.AutoIngest, CoverageRound: 1, CoverageStartedAt: now, NextCoverageAt: now.Add(24 * time.Hour), CreatedAt: now, UpdatedAt: now}
		if err = tx.Create(&out).Error; err != nil {
			return err
		}
		root := models.ProviderObservationItem{ScopeID: out.ID, ResourceID: in.RootID, IsDir: true, RelativePath: "/", QueueRound: 1, DueAt: now, StableSince: now, UpdatedAt: now}
		return tx.Create(&root).Error
	})
	return out, err
}

func observationRootPath(p string) (string, error) {
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n") {
		return "", ErrObservationInvalid
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return "", ErrObservationInvalid
		}
	}
	return path.Clean(p), nil
}
func observationPathsOverlap(a, b string) bool {
	return a == b || a == "/" || b == "/" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// RequestFullSync coalesces with an unfinished traversal instead of creating a
// second scanner. New directories discovered by pages join the same round.
func (s *ProviderObservationService) RequestFullSync(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var scope models.ProviderObservationScope
		if err := tx.First(&scope, "id = ? AND enabled = ?", id, true).Error; err != nil {
			return err
		}
		if scope.LastCompleteAt == nil || scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			return nil
		}
		now := s.now()
		scope.CoverageRound++
		scope.CoverageStartedAt = now
		scope.NextCoverageAt = now.Add(24 * time.Hour)
		scope.UpdatedAt = now
		if err := tx.Save(&scope).Error; err != nil {
			return err
		}
		return tx.Model(&models.ProviderObservationItem{}).Where("scope_id = ? AND batch_id = '' AND resource_id = ?", id, scope.RootID).Updates(map[string]any{"queue_round": scope.CoverageRound, "due_at": now}).Error
	})
}

func (s *ProviderObservationService) validScopeTx(tx *gorm.DB, scope models.ProviderObservationScope) error {
	var current models.ProviderObservationScope
	if err := tx.First(&current, "id = ?", scope.ID).Error; err != nil {
		return err
	}
	if !current.Enabled || current.Fence != scope.Fence || current.AccountKey != scope.AccountKey || current.RootID != scope.RootID {
		return ErrObservationFence
	}
	if s.validate == nil {
		return ErrObservationUnavailable
	}
	if err := observationMutationHoldTx(tx, scope); err != nil {
		return err
	}
	return s.validate(tx, scope)
}

// Read-only consumers use the same durable mutation holds as observation
// execution. A queue lease or a routine directory read is not such a hold.
func observationMutationHoldTx(tx *gorm.DB, scope models.ProviderObservationScope) error {
	if err := observationQuarkReorganizationHoldTx(tx, scope); err != nil {
		return err
	}
	if err := observationQuarkStructureHoldTx(tx, scope); err != nil {
		return err
	}
	return nil
}

// Native library observation owns its private source/configuration fence.
// Catalog read consumers consult this existing adapter boundary without
// reconstructing an account namespace or dispatching a provider request.
func observationLibrarySourceCurrentTx(tx *gorm.DB, scope models.ProviderObservationScope) error {
	input, err := quarkObservationScopeTx(tx, scope.LibraryID)
	if err != nil {
		return err
	}
	if scope.Kind != "library" || input.Provider != scope.Provider || input.Fence != scope.Fence || input.RootID != scope.RootID || input.AccountKey != scope.AccountKey || input.ConnectionID != scope.ConnectionID {
		return ErrObservationFence
	}
	return nil
}

// Run resumes durable queues, including incomplete traversals left by another
// process. A new runtime always restarts directory completeness at page zero.
func (s *ProviderObservationService) Run(ctx context.Context) {
	tick := time.NewTicker(observationRequestInterval)
	defer tick.Stop()
	var lastFailure time.Time
	for {
		if err := s.RunDue(ctx, 16); err != nil && ctx.Err() == nil && time.Since(lastFailure) >= time.Minute {
			operation := serverlog.OperationProviderObservation
			operation.Event(s.log.Warn()).Str("error_code", "observation_waiting_retry").Msg(operation.Message("部分目录巡检或目录发布暂未完成，已保存进度并等待重试"))
			lastFailure = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *ProviderObservationService) RunDue(ctx context.Context, limit int) error {
	if limit < 1 || limit > 100 || s.reader == nil || s.validate == nil {
		return ErrObservationUnavailable
	}
	var scopes []models.ProviderObservationScope
	if err := s.db.WithContext(ctx).Where("enabled = ?", true).Order("last_scheduled_at, id").Limit(limit).Find(&scopes).Error; err != nil {
		return err
	}
	var first error
	for _, scope := range scopes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if scope.NextRetryAt == nil || !scope.NextRetryAt.After(s.now()) {
			if !scope.NextCoverageAt.After(s.now()) {
				if err := s.RequestFullSync(ctx, scope.ID); err != nil && first == nil {
					first = err
				}
			}
			if _, err := s.RepairPaths(ctx, scope.ID, 250); err != nil && first == nil {
				first = err
			}
			reader, err := s.reader(ctx, scope)
			if err == nil {
				_, err = s.ObserveNext(ctx, scope.ID, reader)
			}
			if err != nil && !errors.Is(err, ErrObservationDeferred) && first == nil {
				first = err
			}
			if err != nil && !errors.Is(err, ErrObservationDeferred) {
				_ = s.recordSafeFailure(ctx, scope.ID, "observation_read_failed")
			}
			if s.publisher != nil {
				if _, err = s.PublishPending(ctx, scope.ID, 4); err != nil && !errors.Is(err, ErrObservationDeferred) {
					if first == nil {
						first = err
					}
					_ = s.recordSafeFailure(ctx, scope.ID, "observation_publication_failed")
				} else if err == nil {
					// A recovered publisher must not leave its old error visible forever.
					_ = s.db.WithContext(ctx).Model(&models.ProviderObservationScope{}).Where("id = ? AND last_error_code = ?", scope.ID, "observation_publication_failed").Updates(map[string]any{"last_error_code": "", "next_retry_at": nil}).Error
				}
			}

		}
		if s.progress != nil {
			var err error
			var current models.ProviderObservationScope
			if err = s.db.WithContext(ctx).First(&current, "id = ?", scope.ID).Error; err == nil {
				var status ProviderObservationStatus
				status, err = s.Status(ctx, scope.ID)
				if err == nil {
					err = s.progress(ctx, current, status)
				}
			}
			if err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (s *ProviderObservationService) recordSafeFailure(ctx context.Context, scopeID, code string) error {
	return s.db.WithContext(ctx).Model(&models.ProviderObservationScope{}).Where("id = ?", scopeID).Updates(map[string]any{"last_error_code": code, "next_retry_at": s.now().Add(time.Minute)}).Error
}

// ObserveNext executes at most five pages / fifteen seconds for one directory,
// then yields to other libraries. HTTP is never performed in a DB transaction.
func (s *ProviderObservationService) ObserveNext(ctx context.Context, scopeID string, reader ObservationDirectoryReader) (int, error) {
	if reader == nil {
		return 0, ErrObservationUnavailable
	}
	var scope models.ProviderObservationScope
	if err := s.db.WithContext(ctx).First(&scope, "id = ? AND enabled = ?", scopeID, true).Error; err != nil {
		return 0, err
	}
	if resumed, err := s.resumeConfirmation(ctx, scope); resumed || err != nil {
		return 0, err
	}
	now := s.now()
	directory, err := s.nextDirectory(ctx, scope, now)
	if err != nil {
		return 0, err
	}
	hot := observationIsHot(scope, directory, now)
	lease := uuid.NewString()
	if err := s.claimAccount(ctx, scope, hot, lease); err != nil {
		return 0, err
	}
	defer s.releaseAccount(context.WithoutCancel(ctx), scope.AccountKey, lease)
	bounded, cancel := context.WithTimeout(ctx, observationSliceDuration)
	defer cancel()
	batch, err := s.beginBatch(bounded, scope, directory)
	if err != nil {
		return 0, err
	}
	pages := 0
	for pages < observationSlicePages && bounded.Err() == nil {
		// Recheck immediately before dispatch as well as when accepting the
		// response: a durable reorganization may have started after beginBatch.
		if err = s.validScopeTx(s.db.WithContext(bounded), scope); err != nil {
			return pages, err
		}
		if err = s.reserveRequest(bounded, scope.AccountKey, lease, hot); err != nil {
			if errors.Is(err, ErrObservationDeferred) && pages > 0 {
				return pages, nil
			}
			return pages, err
		}
		page, readErr := reader.ObserveDirectory(bounded, scope, directory.ResourceID, batch.Offset, observationPageLimit)
		pages++
		if readErr != nil {
			op := serverlog.OperationProviderObservation
			op.Event(s.log.Warn()).Uint("library_id", scope.LibraryID).Str("batch_id", batch.ID).Str("directory", directory.RelativePath).Int64("offset", batch.Offset).Str("error_code", "observation_read_failed").Time("next_retry_at", s.now().Add(time.Minute)).Msg(op.Message("读取夸克目录失败，保留已确认内容；本次读取不用于判断删除"))
			var cooldown interface{ RetryAfter() time.Duration }
			if errors.As(readErr, &cooldown) {
				_ = s.Cooldown(context.WithoutCancel(ctx), scope.AccountKey, cooldown.RetryAfter())
			}
			_ = s.failBatch(context.WithoutCancel(ctx), scope, batch, "read_failed")
			return pages, readErr
		}
		if err = s.acceptPage(bounded, scope, directory, &batch, page); err != nil {
			return pages, err
		}
		op := serverlog.OperationProviderObservation
		op.Event(s.log.Info()).Uint("library_id", scope.LibraryID).Str("batch_id", batch.ID).Str("directory", directory.RelativePath).Int("page", batch.Pages).Int("page_items", len(page.Items)).Int64("observed_items", batch.SeenCount).Str("phase", batch.State).Msg(op.Message("已读取夸克目录分页，正在核对实际文件变化"))
		if batch.State != "reading" {
			return pages, nil
		}
		// The persisted next-request instant is shared with all adapter callers.
		// Do not hold the account across a sleep; yielding improves library fairness.
		if pages < observationSlicePages {
			return pages, nil
		}
	}
	return pages, bounded.Err()
}

func (s *ProviderObservationService) beginBatch(ctx context.Context, scope models.ProviderObservationScope, dir models.ProviderObservationItem) (models.ProviderObservationBatch, error) {
	var out models.ProviderObservationBatch
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.validScopeTx(tx, scope); err != nil {
			return err
		}
		if err := tx.First(&dir, "scope_id = ? AND batch_id = '' AND resource_id = ?", scope.ID, dir.ResourceID).Error; err != nil {
			return err
		}
		if dir.ActiveBatchID != "" {
			if err := tx.First(&out, "id = ?", dir.ActiveBatchID).Error; err != nil {
				return err
			}
			if out.State == "reading" && out.RuntimeID == s.runtimeID && out.Fence == scope.Fence {
				return nil
			}
			if out.State == "reading" {
				if err := tx.Model(&out).Updates(map[string]any{"state": "interrupted", "incomplete_reason": "restart_requires_page_zero"}).Error; err != nil {
					return err
				}
			}
		}
		now := s.now()
		out = models.ProviderObservationBatch{ID: uuid.NewString(), ScopeID: scope.ID, DirectoryID: dir.ResourceID, Fence: scope.Fence, CoverageRound: scope.CoverageRound, RuntimeID: s.runtimeID, State: "reading", StartedAt: now, UpdatedAt: now}
		if err := tx.Create(&out).Error; err != nil {
			return err
		}
		return tx.Model(&dir).Updates(map[string]any{"active_batch_id": out.ID}).Error
	})
	return out, err
}

func (s *ProviderObservationService) failBatch(ctx context.Context, scope models.ProviderObservationScope, batch models.ProviderObservationBatch, reason string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.ProviderObservationScope{}).Where("id = ?", scope.ID).Updates(map[string]any{"last_error_code": reason, "next_retry_at": s.now().Add(time.Minute)}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ProviderObservationBatch{}).Where("id = ? AND state = 'reading'", batch.ID).Updates(map[string]any{"state": "incomplete", "incomplete_reason": reason, "updated_at": s.now()}).Error; err != nil {
			return err
		}
		return tx.Model(&models.ProviderObservationItem{}).Where("scope_id = ? AND batch_id = '' AND resource_id = ? AND active_batch_id = ?", scope.ID, batch.DirectoryID, batch.ID).Updates(map[string]any{"active_batch_id": "", "due_at": s.now().Add(time.Minute)}).Error
	})
}

func observationFingerprint(f ObservationFact) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%t\x00%d\x00%d\x00%s\x00%s", f.ParentID, f.Name, f.IsDir, f.Size, f.ModifiedAt.UnixNano(), f.ContentVersion, f.Digest)
	return hex.EncodeToString(h.Sum(nil))
}
func observationFact(row models.ProviderObservationItem) ObservationFact {
	return ObservationFact{ID: row.ResourceID, ParentID: row.ParentID, Name: row.Name, IsDir: row.IsDir, Size: row.Size, ModifiedAt: row.ModifiedAt, ContentVersion: row.ContentVersion, Digest: row.Digest}
}
func observationCurrent(tx *gorm.DB, scopeID string) *gorm.DB {
	return tx.Where("scope_id = ? AND batch_id = ''", scopeID)
}
func validObservationFact(f ObservationFact, parent string) bool {
	return f.ID != "" && len(f.ID) <= 512 && f.ID != parent && f.ParentID == parent && len(f.ParentID) <= 512 && f.Name != "" && len(f.Name) <= 512 && f.Name != "." && f.Name != ".." && !strings.ContainsAny(f.Name, "/\\\x00\r\n") && !strings.ContainsAny(f.ID+f.ParentID+f.ContentVersion+f.Digest, "\x00\r\n") && len(f.ContentVersion) <= 512 && len(f.Digest) <= 256 && f.Size >= 0
}
func observationExcluded(name string, intake bool) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, ".omc-quark-verify-") || strings.HasPrefix(lower, "omc-quark-verify-") || strings.HasPrefix(lower, ".omc-verify-") || (intake && strings.HasPrefix(lower, "omc-"))
}

// ExcludeResource binds exclusion to an exact verified challenge receipt ID,
// supplementing reserved names. It does not delete any cloud file.
func (s *ProviderObservationService) ExcludeResource(ctx context.Context, scopeID, resourceID string) error {
	return observationCurrent(s.db.WithContext(ctx), scopeID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", resourceID).Update("excluded", true).Error
}

// Retain current identity/history anchors indefinitely. Only expendable fully
// acknowledged observation facts age out; recovery details survive >=30 days.
func (s *ProviderObservationService) PruneAcknowledged(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 100 {
		return 0, ErrObservationInvalid
	}
	var batches []models.ProviderObservationBatch
	if err := s.db.WithContext(ctx).Where("state NOT IN ? AND revision = published_revision AND updated_at < ?", []string{"reading", "confirming"}, s.now().Add(-observationRecoveryRetention)).Order("updated_at").Limit(limit).Find(&batches).Error; err != nil {
		return 0, err
	}
	var removed int64
	for _, b := range batches {
		err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// Deletion is bounded too; large retained batches may need several
			// maintenance passes, and their batch header survives until empty.
			result := tx.Where("batch_id = ? AND resource_id IN (SELECT resource_id FROM provider_observation_items WHERE batch_id = ? ORDER BY resource_id LIMIT 250)", b.ID, b.ID).Delete(&models.ProviderObservationItem{})
			if result.Error != nil {
				return result.Error
			}
			var left int64
			if err := tx.Model(&models.ProviderObservationItem{}).Where("batch_id = ?", b.ID).Count(&left).Error; err != nil {
				return err
			}
			if left > 0 {
				return nil
			}
			if err := tx.Delete(&models.ProviderObservationBatch{}, "id = ? AND revision = published_revision AND state NOT IN ?", b.ID, []string{"reading", "confirming"}).Error; err != nil {
				return err
			}
			removed++
			return nil
		})
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func observationUpsert(tx *gorm.DB, row *models.ProviderObservationItem) error {
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "scope_id"}, {Name: "batch_id"}, {Name: "resource_id"}}, UpdateAll: true}).Create(row).Error
}
