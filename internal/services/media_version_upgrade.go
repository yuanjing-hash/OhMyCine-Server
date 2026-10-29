package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

const JobTypeMediaVersionUpgrade = "media_version_upgrade"
const CodeMediaUpgradeChanged = "media_upgrade_changed"
const CodeMediaUpgradeUnsupported = "media_upgrade_recycle_unsupported"
const CodeMediaUpgradeSelection = "media_upgrade_selection_mismatch"
const CodeMediaUpgradePending = "media_upgrade_recycle_pending"

// The acquisition and recycle phases have independent durable receipts. A
// failed cleanup never creates another download or removes the replacement.
type MediaVersionUpgradeService struct {
	db        *gorm.DB
	libraries *MediaLibraryService
	sites     *SiteService
	queue     *QueueService
	now       func() time.Time
}

func NewMediaVersionUpgradeService(db *gorm.DB, libraries *MediaLibraryService, sites *SiteService, queue *QueueService) *MediaVersionUpgradeService {
	return &MediaVersionUpgradeService{db: db, libraries: libraries, sites: sites, queue: queue, now: func() time.Time { return time.Now().UTC() }}
}

type MediaUpgradeSearch struct {
	TMDBID    int64  `json:"tmdb_id"`
	MediaType string `json:"media_type"`
	Title     string `json:"title"`
	Season    *int   `json:"season,omitempty"`
	Episode   *int   `json:"episode,omitempty"`
}
type MediaVersionUpgradeDTO struct {
	ID               string             `json:"id"`
	State            string             `json:"state"`
	Stage            string             `json:"stage"`
	LibraryID        uint               `json:"library_id"`
	EntryID          uint               `json:"entry_id"`
	WorkToken        string             `json:"work_token"`
	Title            string             `json:"title"`
	MediaType        string             `json:"media_type"`
	TMDBID           int64              `json:"tmdb_id"`
	Season           *int               `json:"season,omitempty"`
	Episode          *int               `json:"episode,omitempty"`
	Revision         uint64             `json:"revision"`
	ExpiresAt        time.Time          `json:"expires_at"`
	Search           MediaUpgradeSearch `json:"search"`
	RecycleSupported bool               `json:"recycle_supported"`
	ErrorCode        string             `json:"error_code,omitempty"`
	ErrorMessage     string             `json:"error_message,omitempty"`
	DownloadTaskID   string             `json:"download_task_id,omitempty"`
	TransferTaskID   string             `json:"transfer_task_id,omitempty"`
	NextAttemptAt    *time.Time         `json:"next_attempt_at,omitempty"`
}
type SubmitMediaVersionUpgradeInput struct {
	ContentSelectionToken string   `json:"content_selection_token,omitempty"`
	ResultToken           string   `json:"result_token"`
	DownloaderID          string   `json:"downloader_id"`
	PreviewToken          string   `json:"preview_token,omitempty"`
	SelectedEntryTokens   []string `json:"selected_entry_tokens,omitempty"`
	Priority              int      `json:"priority"`
	Revision              uint64   `json:"revision"`
}

type mediaUpgradeSnapshot struct {
	Version int                              `json:"version"`
	Fence   catalogDeletionFence             `json:"fence"`
	Entry   models.MediaLibraryEntry         `json:"entry"`
	Assets  []models.MediaLibrarySourceAsset `json:"assets"`
	Items   []mediaUpgradeRecycleItem        `json:"items"`
}
type mediaUpgradeRecycleItem struct {
	Key              string    `json:"key"`
	RelativePath     string    `json:"relative_path"`
	ProviderID       string    `json:"provider_id,omitempty"`
	ParentID         string    `json:"parent_id,omitempty"`
	Name             string    `json:"name"`
	SHA1             string    `json:"sha1,omitempty"`
	ProviderRevision string    `json:"provider_revision,omitempty"`
	FileIdentity     string    `json:"file_identity,omitempty"`
	Size             int64     `json:"size"`
	ModifiedAt       time.Time `json:"modified_at"`
}
type mediaUpgradeRecycleState struct {
	Version int               `json:"version"`
	Items   map[string]string `json:"items"`
}
type mediaUpgradeOutput struct {
	PhysicalIdentities map[uint]string `json:"physical_identities,omitempty"`
	Version            int             `json:"version"`
	EntryIDs           []uint          `json:"entry_ids"`
	EntryDigest        string          `json:"entry_digest"`
	Generation         uint64          `json:"generation"`
	BindingID          string          `json:"binding_id,omitempty"`
}

func mediaUpgradeAllowed(actor Actor, libraryID uint) bool {
	id := uintID(libraryID)
	return actor.CanIngestLibrary(id) && actor.CanResource(authz.PermissionMediaLibrariesMediaDelete, models.AuthorizationResourceMediaLibrary, id)
}
func mediaUpgradeChanged() error {
	return appError(CodeMediaUpgradeChanged, "所选版本、来源或权限已变化；保留原版本，请重新选择", nil)
}
func mediaUpgradeSelectionError() error {
	return appError(CodeMediaUpgradeSelection, "请选择同一电影或同一集的一个已核实新版本及其配套字幕", nil)
}
func decodeMediaUpgradeSnapshot(raw string) (mediaUpgradeSnapshot, error) {
	var snapshot mediaUpgradeSnapshot
	if len(raw) > 512*1024 || json.Unmarshal([]byte(raw), &snapshot) != nil || snapshot.Version != 1 || snapshot.Entry.ID == 0 || snapshot.Entry.TMDBID == nil || *snapshot.Entry.TMDBID <= 0 || snapshot.Entry.ProviderID == "" || snapshot.Entry.WorkKey == "" || len(snapshot.Items) == 0 || len(snapshot.Items) > 64 {
		return snapshot, mediaUpgradeChanged()
	}
	return snapshot, nil
}
func mediaUpgradeDTO(row models.MediaVersionUpgrade) (MediaVersionUpgradeDTO, error) {
	snapshot, err := decodeMediaUpgradeSnapshot(row.SnapshotJSON)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	entry := snapshot.Entry
	search := MediaUpgradeSearch{TMDBID: *entry.TMDBID, MediaType: entry.MediaType, Title: catalogDeletionTitle([]models.MediaLibraryEntry{entry}), Season: cloneInt(entry.Season), Episode: cloneInt(entry.Episode)}
	return MediaVersionUpgradeDTO{ID: row.ID, State: row.State, Stage: row.Stage, LibraryID: row.LibraryID, EntryID: row.EntryID, WorkToken: encodeCatalogToken(row.WorkKey), Title: search.Title, MediaType: search.MediaType, TMDBID: search.TMDBID, Season: search.Season, Episode: search.Episode, Revision: row.Revision, ExpiresAt: row.ExpiresAt, Search: search, RecycleSupported: true, ErrorCode: row.ErrorCode, ErrorMessage: mediaUpgradePublicError(row.ErrorCode), DownloadTaskID: row.DownloadTaskID, TransferTaskID: row.TransferTaskID, NextAttemptAt: row.NextAttemptAt}, nil
}
func mediaUpgradePublicError(code string) string {
	switch code {
	case "":
		return ""
	case CodeMediaUpgradeSeedingBusy:
		return "新版本已就绪；旧版本仍是下载器使用中的做种源文件，等待做种结束后再回收"
	case CodeMediaUpgradePending:
		return "新版本已就绪，旧版本回收尚未确认；只核对原回收，不会再次下载"
	case CodeMediaUpgradeSelection:
		return "新输出未核实为所选电影或剧集的替代版本，或内容及字幕已确认没有变化；旧版本保留"
	case CodeMediaUpgradeChanged:
		return "所选版本或来源已变化，旧版本保留；请重新选择"
	case "media_upgrade_replacement_failed":
		return "新版本未完成入库，旧版本保留；请查看对应下载或入库任务"
	case "media_upgrade_artifacts_pending":
		return "本次新版本的播放或元数据产物尚未就绪，旧版本保留"
	default:
		return "洗版尚未完成，旧版本清理未被当作成功"
	}
}
func (s *MediaVersionUpgradeService) Get(ctx context.Context, actor Actor, id string) (MediaVersionUpgradeDTO, error) {
	row, err := s.load(ctx, actor, id)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	return mediaUpgradeDTO(row)
}
func (s *MediaVersionUpgradeService) load(ctx context.Context, actor Actor, id string) (models.MediaVersionUpgrade, error) {
	var row models.MediaVersionUpgrade
	if err := s.db.WithContext(ctx).First(&row, "id=? AND owner_id=?", id, actor.User.ID).Error; err != nil {
		return row, appError(CodeNotFound, "洗版任务不存在", nil)
	}
	if !mediaUpgradeAllowed(actor, row.LibraryID) {
		return row, appError(CodePermissionDenied, "无权对这个媒体库洗版", nil)
	}
	return row, nil
}

// Prepare captures an exact version and only EXCLUSIVE subtitle companions.
// No download, rename, recycle or cloud write takes place here.
func (s *MediaVersionUpgradeService) Prepare(ctx context.Context, actor Actor, libraryID uint, workToken string, entryID uint) (MediaVersionUpgradeDTO, error) {
	if s.libraries == nil || !mediaUpgradeAllowed(actor, libraryID) {
		return MediaVersionUpgradeDTO{}, appError(CodePermissionDenied, "洗版需要媒体库读取、下载入库及源文件回收权限", nil)
	}
	workKey, err := decodeCatalogToken(workToken)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	var entry models.MediaLibraryEntry
	err = s.libraries.withCatalogRead(ctx, []uint{libraryID}, func(_ *gorm.DB, reader *CatalogReader) error {
		return reader.Entries().Where("library_id=? AND id=? AND work_key=?", libraryID, entryID, workKey).First(&entry).Error
	})
	if err != nil {
		return MediaVersionUpgradeDTO{}, appError(CodeNotFound, "所选媒体版本不存在", nil)
	}
	if entry.MatchStatus != mediaRecognitionStatusMatched || entry.TMDBID == nil || *entry.TMDBID <= 0 || (entry.MediaType != "movie" && entry.MediaType != "tv") || (entry.MediaType == "tv" && (entry.Season == nil || entry.Episode == nil || *entry.Season < 0 || *entry.Episode <= 0)) {
		return MediaVersionUpgradeDTO{}, appError(CodeMediaUpgradeSelection, "请先为这个文件确认电影或季集身份，再洗版", nil)
	}
	if entry.MediaType == "tv" {
		if err := validateUpgradeOldEpisode(entry.RelativePath, entry.Season, entry.Episode); err != nil {
			return MediaVersionUpgradeDTO{}, err
		}
	}
	var assets []models.MediaLibrarySourceAsset
	err = s.libraries.withCatalogRead(ctx, []uint{libraryID}, func(_ *gorm.DB, reader *CatalogReader) error {
		var candidates []models.MediaLibrarySourceAsset
		if err := reader.SourceAssets().Where("library_id=? AND active=? AND relative_path LIKE ?", libraryID, true, escapeLike(strings.TrimSuffix(path.Dir(entry.RelativePath), "/"))+"/%").Order("id").Limit(1001).Find(&candidates).Error; err != nil {
			return err
		}
		if len(candidates) > 1000 {
			return ErrCatalogBudget
		}
		for _, asset := range candidates {
			if !exclusiveUpgradeSubtitle(entry, asset) {
				continue
			}
			// Another video whose stem could own this subtitle makes it shared.
			var others []models.MediaLibraryEntry
			if err := reader.Entries().Where("library_id=? AND id<>? AND relative_path LIKE ?", libraryID, entry.ID, escapeLike(strings.TrimSuffix(path.Dir(entry.RelativePath), "/"))+"/%").Limit(1001).Find(&others).Error; err != nil {
				return err
			}
			if len(others) > 1000 {
				return ErrCatalogBudget
			}
			shared := false
			for _, other := range others {
				if exclusiveUpgradeSubtitle(other, asset) {
					shared = true
					break
				}
			}
			if !shared {
				assets = append(assets, asset)
			}
		}
		return nil
	})
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	if len(assets) > 63 {
		return MediaVersionUpgradeDTO{}, ErrCatalogBudget
	}
	paths := []string{entry.RelativePath}
	for _, asset := range assets {
		paths = append(paths, asset.RelativePath)
	}
	write, err := s.libraries.captureCatalogDeletion(ctx, libraryID, "", paths)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	if len(write.Entries) != 1 || catalogDeletionDigest(write.Entries) != catalogDeletionDigest([]models.MediaLibraryEntry{entry}) {
		return MediaVersionUpgradeDTO{}, mediaUpgradeChanged()
	}
	if !write.Library.Enabled || !write.Storage.Enabled {
		return MediaVersionUpgradeDTO{}, mediaUpgradeChanged()
	}
	if err := validateUpgradeArtifactConfiguration(write.Library, write.Storage); err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	snapshot := mediaUpgradeSnapshot{Version: 1, Fence: write.Fence, Entry: entry, Assets: assets}
	snapshot.Items, err = s.captureRecycleItems(ctx, write.Library, write.Storage, entry, assets)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	now := s.now()
	row := models.MediaVersionUpgrade{ID: uuid.NewString(), OwnerID: actor.User.ID, LibraryID: libraryID, EntryID: entryID, WorkKey: workKey, Revision: 1, State: "prepared", Stage: "choose_version", SnapshotJSON: string(raw), OutputJSON: "{}", RecycleJSON: `{"version":1,"items":{}}`, ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now, UpdatedAt: now}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := NewAuthorizationService(tx).Resolve(actor.User.ID)
		if err != nil || !mediaUpgradeAllowed(current, libraryID) {
			return mediaUpgradeChanged()
		}
		reader, err := PinCatalogTx(tx, []uint{libraryID})
		if err != nil {
			return err
		}
		if err := validateCatalogDeletionTx(tx, reader, write, true); err != nil {
			return err
		}
		var existing models.MediaVersionUpgrade
		find := tx.Where("library_id=? AND entry_id=? AND state IN ?", libraryID, entryID, []string{"prepared", "submitted", "replacement_ready", "recycle_pending"}).First(&existing).Error
		if find == nil {
			if existing.OwnerID != actor.User.ID {
				return appError(CodeConflict, "该版本已有洗版任务", nil)
			}
			if existing.State != "prepared" || existing.ExpiresAt.After(now) {
				row = existing
				return nil
			}
			if err := tx.Model(&existing).Updates(map[string]any{"state": "expired", "revision": gorm.Expr("revision+1"), "updated_at": now}).Error; err != nil {
				return err
			}
		} else if !errors.Is(find, gorm.ErrRecordNotFound) {
			return find
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	return mediaUpgradeDTO(row)
}

// A catalog row stores one coordinate, even when a physical leaf's name spans
// several episodes. This single-episode workflow cannot safely recycle such a
// file. Use the real leaf only; a parent package range is not file coverage.
func validateUpgradeOldEpisode(relative string, season, episode *int) error {
	base := path.Base(relative)
	if season == nil || episode == nil || !followEpisodeDeclarationIsUnambiguous(base, *season) {
		return appError(CodeMediaUpgradeSelection, "旧文件可能覆盖多集或无法确认仅含所选一集，暂不支持单集洗版；旧版本保留", nil)
	}
	if first, last, ok := explicitFollowEpisodeRange(base, *season); ok && (first != *episode || last != *episode) {
		return appError(CodeMediaUpgradeSelection, "旧文件覆盖多集，暂不支持只洗其中一集；旧版本保留", nil)
	}
	facts, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: base, SeasonHint: cloneInt(season), SourceKind: mediarecognition.SourceDownload})
	if err != nil || episode == nil || facts.Episodes.Count != 1 || facts.Episodes.EpisodeMin == nil || facts.Episodes.EpisodeMax == nil || *facts.Episodes.EpisodeMin != *episode || *facts.Episodes.EpisodeMax != *episode || facts.Episodes.SeasonMin != nil && !equalInt(facts.Episodes.SeasonMin, season) || facts.Episodes.SeasonMax != nil && !equalInt(facts.Episodes.SeasonMax, season) {
		return appError(CodeMediaUpgradeSelection, "旧文件可能覆盖多集或无法确认仅含所选一集，暂不支持单集洗版；旧版本保留", nil)
	}
	return nil
}
func exclusiveUpgradeSubtitle(entry models.MediaLibraryEntry, asset models.MediaLibrarySourceAsset) bool {
	if path.Dir(entry.RelativePath) != path.Dir(asset.RelativePath) {
		return false
	}
	switch strings.ToLower(path.Ext(asset.RelativePath)) {
	case ".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx":
	default:
		return false
	}
	videoStem := strings.ToLower(strings.TrimSuffix(path.Base(entry.RelativePath), path.Ext(entry.RelativePath)))
	assetStem := strings.ToLower(strings.TrimSuffix(path.Base(asset.RelativePath), path.Ext(asset.RelativePath)))
	return assetStem == videoStem || strings.HasPrefix(assetStem, videoStem+".")
}

func (s *MediaVersionUpgradeService) validateSnapshotTx(tx *gorm.DB, row models.MediaVersionUpgrade, snapshot mediaUpgradeSnapshot) error {
	current, err := NewAuthorizationService(tx).Resolve(row.OwnerID)
	if err != nil || !mediaUpgradeAllowed(current, row.LibraryID) {
		return mediaUpgradeChanged()
	}
	reader, err := PinCatalogTx(tx, []uint{row.LibraryID})
	if err != nil {
		return err
	}
	_, _, _, fence, err := catalogDeletionFenceTx(tx, reader, row.LibraryID)
	if err != nil {
		return err
	}
	// Content revision/head are deliberately not compared: the replacement and
	// unrelated valid publications may advance them. Source/config epochs may not.
	frozen := snapshot.Fence
	frozen.ContentRevision = fence.ContentRevision
	if frozen != fence {
		return mediaUpgradeChanged()
	}
	var entries []models.MediaLibraryEntry
	if err := reader.Entries().Where("library_id=? AND id=?", row.LibraryID, row.EntryID).Find(&entries).Error; err != nil {
		return err
	}
	if catalogDeletionDigest(entries) != catalogDeletionDigest([]models.MediaLibraryEntry{snapshot.Entry}) {
		return mediaUpgradeChanged()
	}
	if len(snapshot.Assets) > 0 {
		ids := make([]uint, 0, len(snapshot.Assets))
		for _, asset := range snapshot.Assets {
			ids = append(ids, asset.ID)
		}
		var assets []models.MediaLibrarySourceAsset
		if err := reader.SourceAssets().Where("library_id=? AND id IN ? AND active=?", row.LibraryID, ids, true).Order("id").Find(&assets).Error; err != nil {
			return err
		}
		if catalogDeletionAssetDigest(assets) != catalogDeletionAssetDigest(snapshot.Assets) {
			return mediaUpgradeChanged()
		}
	}
	return nil
}

func (s *MediaVersionUpgradeService) Submit(ctx context.Context, actor Actor, id string, input SubmitMediaVersionUpgradeInput, request RequestContext) (MediaVersionUpgradeDTO, error) {
	row, err := s.load(ctx, actor, id)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	if row.DownloadTaskID != "" {
		return mediaUpgradeDTO(row)
	} // lost HTTP reply is not another acquisition
	if row.State != "prepared" || row.Revision != input.Revision || !row.ExpiresAt.After(s.now()) {
		return MediaVersionUpgradeDTO{}, mediaUpgradeChanged()
	}
	snapshot, err := decodeMediaUpgradeSnapshot(row.SnapshotJSON)
	if err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	if s.sites == nil || s.queue == nil {
		return MediaVersionUpgradeDTO{}, appError(CodeQueueWorkerUnavailable, "洗版服务尚未就绪", nil)
	}
	entry := snapshot.Entry
	internal := SiteDownloadInput{ContentSelectionToken: input.ContentSelectionToken, ResultToken: input.ResultToken, DownloaderID: input.DownloaderID, PreviewToken: input.PreviewToken, SelectedEntryTokens: input.SelectedEntryTokens, MediaLibraryID: &row.LibraryID, Priority: input.Priority, Season: cloneInt(entry.Season), Episode: cloneInt(entry.Episode), PreserveExistingVersions: true, ReplacementIdentity: &DownloadRecognitionIdentity{TMDBID: *entry.TMDBID, MediaType: entry.MediaType, Source: mediaIdentitySourceDirectID, Status: mediaIdentityStatusVerified, Season: cloneInt(entry.Season), Episode: cloneInt(entry.Episode)}}
	internal.BeforePersist = func(tx *gorm.DB) error {
		var locked models.MediaVersionUpgrade
		if err := tx.First(&locked, "id=?", row.ID).Error; err != nil {
			return err
		}
		if locked.State != "prepared" || locked.Revision != input.Revision || locked.DownloadTaskID != "" || !locked.ExpiresAt.After(s.now()) {
			return mediaUpgradeChanged()
		}
		return s.validateSnapshotTx(tx, locked, snapshot)
	}
	internal.AfterPersist = func(tx *gorm.DB, download models.DownloadTask) error {
		if download.TargetLibraryID == nil || *download.TargetLibraryID != row.LibraryID || download.ConflictPolicy != models.MediaLibraryConflictRename {
			return mediaUpgradeChanged()
		}
		now := s.now()
		update := tx.Model(&models.MediaVersionUpgrade{}).Where("id=? AND state='prepared' AND revision=? AND download_task_id=''", row.ID, input.Revision).Updates(map[string]any{"state": "submitted", "stage": "download_import", "download_task_id": download.ID, "revision": gorm.Expr("revision+1"), "updated_at": now})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return mediaUpgradeChanged()
		}
		// Nested queue transaction is a savepoint on the existing acquisition tx.
		// Local queue wakeups/publication are suppressed until the outer transaction commits.
		transactionQueue := *s.queue
		transactionQueue.db = tx
		transactionQueue.events = nil
		transactionQueue.notify = nil
		_, err := transactionQueue.EnqueueWith(EnqueueJobInput{OwnerID: row.OwnerID, JobType: JobTypeMediaVersionUpgrade, DisplayName: "洗版：" + safeLabel(entry.Title, 220), ResourceKey: fmt.Sprintf("library:%d", row.LibraryID), Payload: map[string]string{"upgrade_id": row.ID}}, func(tx *gorm.DB, job models.Job) error {
			return tx.Model(&models.MediaVersionUpgrade{}).Where("id=?", row.ID).Update("job_id", job.ID).Error
		})
		return err
	}
	if _, err = s.sites.Download(ctx, actor, internal, request); err != nil {
		return MediaVersionUpgradeDTO{}, err
	}
	s.queue.wake()
	var committed models.MediaVersionUpgrade
	if s.db.First(&committed, "id=?", row.ID).Error == nil {
		var job models.Job
		if s.db.First(&job, "id=?", committed.JobID).Error == nil {
			s.queue.publish(job, "job.created")
		}
	}
	return s.Get(ctx, actor, row.ID)
}

// Keep unsupported capabilities out of admission. A read-only adapter is never
// assumed to support trash and no provider name chooses permanent deletion.
func (s *MediaVersionUpgradeService) recycleDriver(storage models.Storage) (cloudpkg.Driver, cloudpkg.MutationDriver, error) {
	if storage.ConnectionID == nil || s.libraries.connections == nil {
		return nil, nil, appError(CodeMediaUpgradeUnsupported, "该存储尚未提供可恢复回收能力，暂不能洗版", nil)
	}
	_, driver, err := s.libraries.connections.driver(*storage.ConnectionID)
	if err != nil {
		return nil, nil, err
	}
	mutations, ok := driver.(cloudpkg.MutationDriver)
	if !ok || !mutations.Capabilities().Recycle {
		return nil, nil, appError(CodeMediaUpgradeUnsupported, "该存储尚未提供可恢复回收能力，暂不能洗版", nil)
	}
	return driver, mutations, nil
}

// The existing cloud artifact producer requires a signed projection root. A
// metadata-only cloud configuration cannot promise NFO/images independently.
// Refuse before acquiring any replacement instead of silently dropping them.
func validateUpgradeArtifactConfiguration(library models.MediaLibrary, storage models.Storage) error {
	if storage.Type != models.StorageTypeLocal && library.MetadataArtifactsEnabled && (!library.STRMEnabled || !library.SignedProxyEnabled) {
		return appError(CodeMediaUpgradeUnsupported, "此云媒体库启用了元数据产物，但尚未启用签名 STRM 投影；请先完成产物配置再洗版", nil)
	}
	return nil
}
