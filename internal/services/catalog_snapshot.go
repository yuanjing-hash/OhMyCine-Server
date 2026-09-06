package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	CatalogFormat        = 1
	CatalogMaxDeltas     = 8
	CatalogMaxDeltaRows  = 10000
	CatalogMaxDeltaBytes = 16 << 20
	CatalogBatchRows     = 250
	CatalogBatchBytes    = 1 << 20
)

var (
	ErrCatalogFence   = errors.New("catalog snapshot fence changed")
	ErrCatalogBudget  = errors.New("catalog snapshot budget exceeded")
	ErrCatalogInvalid = errors.New("catalog snapshot invalid")
)

// CatalogSnapshotStore is the storage boundary, not a rollout switch. No
// production consumer is converted merely by constructing it or migrating SQL.
type CatalogSnapshotStore struct {
	writeDB, readDB *gorm.DB
	admission       *CatalogWriteAdmission
	spaceAvailable  func(context.Context) (uint64, error)
	identityBudget  catalogIdentityBatchBudget
}

func NewCatalogSnapshotStore(writeDB, readDB *gorm.DB) *CatalogSnapshotStore {
	return &CatalogSnapshotStore{writeDB: writeDB, readDB: readDB, admission: NewCatalogWriteAdmission()}
}

type CatalogCandidateInput struct {
	LibraryID                            uint
	Kind                                 string
	ExpectedRevision                     uint64
	SourceEpoch                          uint64
	SourceFingerprint, ConfigFingerprint string
	JobID                                *string
	JobLeaseHash                         string
	LeaseDuration                        time.Duration
}

type CatalogFactBatch struct {
	Entries      []models.CatalogEntryFact
	Recognitions []models.CatalogRecognitionFact
	SourceAssets []models.CatalogSourceAssetFact
}

func catalogTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func requireCatalogTransaction(tx *gorm.DB) error {
	if tx == nil || tx.Statement == nil {
		return ErrCatalogInvalid
	}
	if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return fmt.Errorf("catalog operation requires a pinned transaction")
	}
	return nil
}

// StartCatalogConversionTx installs the disabled/legacy-reading fence. The
// caller must drain every library writer and verify updater compatibility in
// guard, in this SAME transaction. There is deliberately no public endpoint or
// background startup caller until the complete consumer migration is verified.
func StartCatalogConversionTx(tx *gorm.DB, head models.CatalogHead, guard func(*gorm.DB) error) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if guard == nil || head.LibraryID == 0 || head.SourceEpoch == 0 || head.SourceFingerprint == "" || head.ConfigFingerprint == "" {
		return ErrCatalogInvalid
	}
	if err := assertCatalogConversionDrainedTx(tx, head.LibraryID); err != nil { return err }
	if err := guard(tx); err != nil {
		return err
	}
	head.Mode, head.Revision, head.UpdatedAt = "converting", 0, time.Now().UTC()
	return tx.Create(&head).Error
}

func (s *CatalogSnapshotStore) BeginCandidate(ctx context.Context, input CatalogCandidateInput) (models.CatalogSnapshot, string, error) {
	var candidate models.CatalogSnapshot
	if input.Kind != "base" && input.Kind != "delta" {
		return candidate, "", ErrCatalogInvalid
	}
	if input.LeaseDuration <= 0 || input.LeaseDuration > 15*time.Minute {
		return candidate, "", ErrCatalogInvalid
	}
	token := uuid.NewString() + uuid.NewString()
	growth, err := s.catalogCandidateGrowth(ctx, input)
	if err != nil {
		return candidate, "", err
	}
	err = s.writeCatalogGrowth(ctx, growth, func(tx *gorm.DB) error {
		if err := requireMediaLibraryNotRetiringTx(tx, input.LibraryID); err != nil {
			return err
		}
		if err := catalogPreparationBudgetTx(tx, input); err != nil {
			return err
		}
		var head models.CatalogHead
		if err := tx.First(&head, "library_id = ?", input.LibraryID).Error; err != nil {
			return err
		}
		if head.Revision != input.ExpectedRevision || head.SourceEpoch != input.SourceEpoch || head.SourceFingerprint != input.SourceFingerprint || head.ConfigFingerprint != input.ConfigFingerprint || (head.Mode != "converting" && head.Mode != "versioned") {
			return ErrCatalogFence
		}
		if input.Kind == "delta" && head.Mode != "versioned" {
			return ErrCatalogInvalid
		}
		now := time.Now().UTC()
		candidate = models.CatalogSnapshot{ID: uuid.NewString(), LibraryID: head.LibraryID, Kind: input.Kind, State: "building", ParentRevision: head.Revision, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, OwnerTokenHash: catalogTokenHash(token), JobID: input.JobID, JobLeaseHash: input.JobLeaseHash, LeaseExpiresAt: now.Add(input.LeaseDuration), CreatedAt: now, UpdatedAt: now}
		if err := catalogCheckJob(tx, candidate, now); err != nil {
			return err
		}
		return tx.Create(&candidate).Error
	})
	if err != nil {
		return models.CatalogSnapshot{}, "", err
	}
	return candidate, token, nil
}

func catalogCheckJob(tx *gorm.DB, snapshot models.CatalogSnapshot, now time.Time) error {
	if snapshot.JobID == nil {
		if snapshot.JobLeaseHash != "" {
			return ErrCatalogInvalid
		}
		return nil
	}
	if snapshot.JobLeaseHash == "" {
		return ErrCatalogFence
	}
	var count int64
	err := tx.Model(&models.Job{}).Where("id = ? AND lease_token_hash = ? AND lease_expires_at > ? AND status = ? AND cancellation_asked = ? AND interrupt_status = ?", *snapshot.JobID, snapshot.JobLeaseHash, now, "running", false, "").Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrCatalogFence
	}
	return nil
}

func catalogOwnedCandidate(tx *gorm.DB, id, token, state string) (models.CatalogSnapshot, error) {
	var row models.CatalogSnapshot
	if err := tx.First(&row, "id = ?", id).Error; err != nil {
		return row, err
	}
	if err := requireMediaLibraryNotRetiringTx(tx, row.LibraryID); err != nil {
		return row, err
	}
	now := time.Now().UTC()
	if token == "" || row.OwnerTokenHash != catalogTokenHash(token) || row.State != state || !row.LeaseExpiresAt.After(now) {
		return row, ErrCatalogFence
	}
	var head models.CatalogHead
	if err := tx.First(&head, "library_id = ?", row.LibraryID).Error; err != nil {
		return row, err
	}
	if head.SourceEpoch != row.SourceEpoch || head.SourceFingerprint != row.SourceFingerprint || head.ConfigFingerprint != row.ConfigFingerprint || (head.Mode != "converting" && head.Mode != "versioned") {
		return row, ErrCatalogFence
	}
	if head.Revision != row.ParentRevision {
		if head.Mode != "versioned" || head.Revision < row.ParentRevision {
			return row, ErrCatalogFence
		}
		prefix, err := catalogCompactionPrefix(tx, row)
		if err != nil {
			return row, err
		}
		if len(prefix) == 0 {
			return row, ErrCatalogFence
		}
	}
	return row, catalogCheckJob(tx, row, now)
}

// AppendBatch accepts one bounded immutable batch, not a full scan. Retrying a
// persisted row is a conflict: callers resume using their committed checkpoint.
// Parsing/encoding happens before taking the writer. Admission/fairness is owned
// by the scheduler and must wrap this method, not create 128 SQLite writers.
func (s *CatalogSnapshotStore) AppendBatch(ctx context.Context, id, token string, batch CatalogFactBatch) error {
	count := len(batch.Entries) + len(batch.Recognitions) + len(batch.SourceAssets)
	if count == 0 || count > CatalogBatchRows {
		return ErrCatalogBudget
	}
	// Private embedded fields have json:"-" for safe DTOs, so account the actual
	// GORM field values rather than the intentionally redacted JSON envelope.
	bytes := catalogBatchSize(batch)
	if bytes > CatalogBatchBytes {
		return ErrCatalogBudget
	}
	return s.writeCatalogGrowth(ctx, bytes, func(tx *gorm.DB) error {
		candidate, err := catalogOwnedCandidate(tx, id, token, "building")
		if err != nil {
			return err
		}
		if candidate.Kind == "delta" && (candidate.RowCount+int64(count) > CatalogMaxDeltaRows || candidate.ByteCount+bytes > CatalogMaxDeltaBytes) {
			return ErrCatalogBudget
		}
		identities := map[string][]uint{"entry": {}, "recognition": {}, "asset": {}}
		for i := range batch.Entries {
			row := &batch.Entries[i]
			if row.LibraryID != candidate.LibraryID || row.ID == 0 {
				return ErrCatalogInvalid
			}
			identities["entry"] = append(identities["entry"], row.ID)
			row.SnapshotID = id
		}
		for i := range batch.Recognitions {
			row := &batch.Recognitions[i]
			if row.LibraryID != candidate.LibraryID || row.ID == 0 {
				return ErrCatalogInvalid
			}
			identities["recognition"] = append(identities["recognition"], row.ID)
			row.SnapshotID = id
		}
		for i := range batch.SourceAssets {
			row := &batch.SourceAssets[i]
			if row.LibraryID != candidate.LibraryID || row.ID == 0 {
				return ErrCatalogInvalid
			}
			identities["asset"] = append(identities["asset"], row.ID)
			row.SnapshotID = id
		}
		for kind, ids := range identities {
			if len(ids) == 0 {
				continue
			}
			var found int64
			if err := tx.Model(&models.CatalogIdentity{}).Where("library_id=? AND source_epoch=? AND entity_kind=? AND anchor_id IN ?", candidate.LibraryID, candidate.SourceEpoch, kind, ids).Count(&found).Error; err != nil {
				return err
			}
			if found != int64(len(ids)) {
				return ErrCatalogInvalid
			}
		}
		if len(batch.Recognitions) > 0 {
			if err := tx.CreateInBatches(batch.Recognitions, 100).Error; err != nil {
				return err
			}
		}
		if len(batch.Entries) > 0 {
			if err := tx.CreateInBatches(batch.Entries, 100).Error; err != nil {
				return err
			}
		}
		if len(batch.SourceAssets) > 0 {
			if err := tx.CreateInBatches(batch.SourceAssets, 100).Error; err != nil {
				return err
			}
		}
		return tx.Model(&models.CatalogSnapshot{}).Where("id = ?", id).Updates(map[string]any{"row_count": candidate.RowCount + int64(count), "byte_count": candidate.ByteCount + bytes, "updated_at": time.Now().UTC()}).Error
	})
}

func catalogBatchSize(batch CatalogFactBatch) int64 {
	var bytes int64
	for _, f := range batch.Entries {
		r := f.MediaLibraryEntry
		bytes += 256 + int64(len(r.RelativePath)+len(r.ProviderID)+len(r.MediaType)+len(r.Title)+len(r.WorkKey)+len(r.SeriesTitle)+len(r.MatchStatus)+len(r.RecognitionErrorCode)+len(r.CategoryName))
		if r.MatchedRuleID != nil {
			bytes += int64(len(*r.MatchedRuleID))
		}
	}
	for _, f := range batch.Recognitions {
		r := f.MediaLibraryRecognition
		bytes += 256 + int64(len(r.SourceKey)+len(r.InputFingerprint)+len(r.Status)+len(r.ErrorCode)+len(r.MediaType)+len(r.Title)+len(r.CategoryName)+len(r.MetadataJSON)+len(f.WorkKey))
		if r.MatchedRuleID != nil {
			bytes += int64(len(*r.MatchedRuleID))
		}
	}
	for _, f := range batch.SourceAssets {
		r := f.MediaLibrarySourceAsset
		bytes += 192 + int64(len(r.ProviderID)+len(r.ParentProviderID)+len(r.RelativePath)+len(r.Name)+len(r.Extension)+len(r.HashHint))
	}
	return bytes
}

// PublishTx performs only bounded metadata operations. guard must revalidate
// actual library/Profile/job authority, compatibility floor and atomically write
// the compact content-change/notification marker; it MUST NOT fan out targets,
// enumerate files, rewrite generations or reconcile the full collection table.
// The caller owns the surrounding immediate transaction and its rollback.
func (s *CatalogSnapshotStore) PublishTx(tx *gorm.DB, id, token string, expectedRevision uint64, guard func(*gorm.DB) error) (uint64, error) {
	if err := requireCatalogTransaction(tx); err != nil {
		return 0, err
	}
	if guard == nil {
		return 0, ErrCatalogInvalid
	}
	floor, err := database.ReadCatalogFormat(tx.Statement.Context, tx)
	if err != nil {
		return 0, err
	}
	if floor > CatalogFormat {
		return 0, ErrCatalogInvalid
	}
	var existing models.CatalogSnapshot
	if err := tx.First(&existing, "id = ?", id).Error; err != nil {
		return 0, err
	}
	if existing.State == "published" && token != "" && existing.OwnerTokenHash == catalogTokenHash(token) {
		return existing.PublishedRevision, nil
	}
	c, err := catalogOwnedCandidate(tx, id, token, "ready")
	if err != nil {
		return 0, err
	}
	if prefix, err := catalogCompactionPrefix(tx, c); err != nil {
		return 0, err
	} else if len(prefix) > 0 {
		return 0, ErrCatalogInvalid
	}
	if err := validateCatalogCollectionsPrepared(tx, c); err != nil {
		return 0, err
	}
	if c.ParentRevision != expectedRevision {
		return 0, ErrCatalogFence
	}
	var layers []models.CatalogHeadLayer
	if err := tx.Where("library_id = ?", c.LibraryID).Order("rank").Find(&layers).Error; err != nil {
		return 0, err
	}
	if c.Kind == "delta" {
		if len(layers) == 0 || len(layers) > CatalogMaxDeltas {
			return 0, ErrCatalogBudget
		}
		var totals struct {
			Rows  int64
			Bytes int64
		}
		if err := tx.Table("catalog_snapshots s").Select("COALESCE(SUM(s.row_count),0) AS rows,COALESCE(SUM(s.byte_count),0) AS bytes").Joins("JOIN catalog_head_layers l ON l.snapshot_id=s.id").Where("l.library_id=? AND l.rank>0", c.LibraryID).Scan(&totals).Error; err != nil {
			return 0, err
		}
		if totals.Rows+c.RowCount > CatalogMaxDeltaRows || totals.Bytes+c.ByteCount > CatalogMaxDeltaBytes {
			return 0, ErrCatalogBudget
		}
	}
	if err := guard(tx); err != nil {
		return 0, err
	}
	revision := expectedRevision + 1
	result := tx.Model(&models.CatalogHead{}).Where("library_id=? AND revision=? AND source_epoch=? AND source_fingerprint=? AND config_fingerprint=?", c.LibraryID, expectedRevision, c.SourceEpoch, c.SourceFingerprint, c.ConfigFingerprint).Updates(map[string]any{"mode": "versioned", "revision": revision, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected != 1 {
		return 0, ErrCatalogFence
	}
	if c.Kind == "base" {
		if err := tx.Where("library_id=?", c.LibraryID).Delete(&models.CatalogHeadLayer{}).Error; err != nil {
			return 0, err
		}
		layers = nil
	}
	if err := tx.Create(&models.CatalogHeadLayer{LibraryID: c.LibraryID, Rank: len(layers), SnapshotID: c.ID}).Error; err != nil {
		return 0, err
	}
	if err := tx.Model(&models.CatalogSnapshot{}).Where("id=? AND state=?", id, "ready").Updates(map[string]any{"state": "published", "published_revision": revision, "updated_at": time.Now().UTC()}).Error; err != nil {
		return 0, err
	}
	if err := tx.Exec("UPDATE catalog_format_floor SET format=? WHERE id=1 AND format<?", CatalogFormat, CatalogFormat).Error; err != nil {
		return 0, err
	}
	return revision, nil
}
