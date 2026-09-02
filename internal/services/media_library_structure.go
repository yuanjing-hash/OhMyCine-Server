package services

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

const JobTypeMediaLibraryRepair = "media_library_repair"

type StructureBoundary struct {
	Library models.MediaLibrary
	Storage models.Storage
}

type StructureProgress func(processed, total int) error

type MediaLibraryStructureBackend interface {
	StorageType() string
	Apply(context.Context, StructureBoundary, []StructurePlanItem, StructureProgress) error
}

type MediaLibraryStructureBackendRegistry struct {
	mu       sync.RWMutex
	backends map[string]MediaLibraryStructureBackend
}

func NewMediaLibraryStructureBackendRegistry(backends ...MediaLibraryStructureBackend) *MediaLibraryStructureBackendRegistry {
	registry := &MediaLibraryStructureBackendRegistry{backends: make(map[string]MediaLibraryStructureBackend, len(backends))}
	for _, backend := range backends {
		registry.Register(backend)
	}
	return registry
}

func (r *MediaLibraryStructureBackendRegistry) Register(backend MediaLibraryStructureBackend) {
	if r == nil || backend == nil || strings.TrimSpace(backend.StorageType()) == "" {
		return
	}
	r.mu.Lock()
	r.backends[backend.StorageType()] = backend
	r.mu.Unlock()
}

func (r *MediaLibraryStructureBackendRegistry) Get(storageType string) (MediaLibraryStructureBackend, error) {
	if r == nil {
		return nil, errors.New("structure backend registry is unavailable")
	}
	r.mu.RLock()
	backend := r.backends[storageType]
	r.mu.RUnlock()
	if backend == nil {
		return nil, errors.New("structure backend is unsupported")
	}
	return backend, nil
}

type MediaLibraryStructureService struct {
	db          *gorm.DB
	audit       *AuditService
	queue       *QueueService
	connections *ConnectionService
	log         zerolog.Logger
	planner     StructurePlanner
	backends    *MediaLibraryStructureBackendRegistry
	reconcile   func(uint)
	confirmKey  []byte
}

type MediaLibraryStructureDiagnostics struct {
	LibraryID    uint             `json:"library_id"`
	Status       string           `json:"status"`
	IssueCount   int              `json:"issue_count"`
	Unrecognized int              `json:"unrecognized"`
	CheckedAt    time.Time        `json:"checked_at"`
	Issues       []StructureIssue `json:"issues"`
	Revision     string           `json:"revision"`
}

type MediaLibraryStructurePreview struct {
	LibraryID         uint             `json:"library_id"`
	Revision          string           `json:"revision"`
	IssueCount        int              `json:"issue_count"`
	RepairableCount   int              `json:"repairable_count"`
	MoveCount         int              `json:"move_count"`
	Issues            []StructureIssue `json:"issues"`
	ConfirmationToken string           `json:"confirmation_token"`
	ExpiresAt         time.Time        `json:"expires_at"`
}

type mediaLibraryStructureClaim struct {
	ActorID         uint   `json:"actor_id"`
	LibraryID       uint   `json:"library_id"`
	WorkKey         string `json:"work_key"`
	Generation      uint64 `json:"generation"`
	RuleFingerprint string `json:"rule_fingerprint"`
	PlanHash        string `json:"plan_hash"`
	ExpiresAt       int64  `json:"expires_at"`
}

type mediaLibraryRepairJobPayload struct {
	RepairID string `json:"repair_id"`
}

func NewMediaLibraryStructureService(db *gorm.DB, audit *AuditService, queue *QueueService, connections *ConnectionService, log zerolog.Logger) *MediaLibraryStructureService {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("secure media library structure confirmation key unavailable")
	}
	service := &MediaLibraryStructureService{db: db, audit: audit, queue: queue, connections: connections, log: log, confirmKey: key}
	service.backends = NewMediaLibraryStructureBackendRegistry(
		localMediaLibraryStructureBackend{},
		pan115MediaLibraryStructureBackend{driver: func(connectionID uint) (cloudpkg.Driver, error) {
			if service.connections == nil {
				return nil, errors.New("provider connection is unavailable")
			}
			_, driver, err := service.connections.driver(connectionID)
			return driver, err
		}},
	)
	return service
}

func (s *MediaLibraryStructureService) SetReconcileNotifier(notify func(uint)) { s.reconcile = notify }

func structureDiagnosticRevision(library models.MediaLibrary) string {
	checkedAt := int64(0)
	if library.StructureCheckedAt != nil {
		checkedAt = library.StructureCheckedAt.UTC().UnixNano()
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%d\x00%s\x00%d\x00%s\x00%d", library.ID, library.BaselineGeneration, library.StructureStatus, library.StructureIssueCount, library.StructureErrorCode, checkedAt)))
	return fmt.Sprintf("%x", sum[:])
}

func structurePlanHash(plan StructurePlan) (string, error) {
	raw, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:]), nil
}

func (s *MediaLibraryStructureService) signStructureClaim(claim mediaLibraryStructureClaim) (string, error) {
	body, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.confirmKey)
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *MediaLibraryStructureService) verifyStructureClaim(token string) (mediaLibraryStructureClaim, error) {
	var claim mediaLibraryStructureClaim
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return claim, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", nil)
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(body) == 0 || len(body) > 4096 {
		return claim, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claim, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", err)
	}
	mac := hmac.New(sha256.New, s.confirmKey)
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(body, &claim) != nil {
		return claim, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", nil)
	}
	return claim, nil
}

func (s *MediaLibraryStructureService) Diagnose(ctx context.Context, libraryID uint, workKey string) (MediaLibraryStructureDiagnostics, error) {
	plan, library, err := s.buildPlan(ctx, libraryID, workKey)
	if err != nil {
		if strings.TrimSpace(workKey) == "" {
			now := time.Now().UTC()
			_ = s.db.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Updates(map[string]any{"structure_status": models.MediaLibraryStructureFailed, "structure_error_code": CodeMediaLibraryStructureDiagnosisFailed, "structure_checked_at": now}).Error
		}
		return MediaLibraryStructureDiagnostics{}, err
	}
	now := time.Now().UTC()
	status := models.MediaLibraryStructureHealthy
	if plan.IssueCount > 0 {
		status = models.MediaLibraryStructureIssues
	}
	if strings.TrimSpace(workKey) == "" {
		if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Updates(map[string]any{"structure_status": status, "structure_issue_count": plan.IssueCount, "structure_error_code": "", "structure_checked_at": now}).Error; err != nil {
			return MediaLibraryStructureDiagnostics{}, err
		}
		library.StructureStatus = status
		library.StructureIssueCount = plan.IssueCount
		library.StructureErrorCode = ""
		library.StructureCheckedAt = &now
	}
	return MediaLibraryStructureDiagnostics{LibraryID: libraryID, Status: status, IssueCount: plan.IssueCount, Unrecognized: plan.Unrecognized, CheckedAt: now, Issues: plan.Issues, Revision: structureDiagnosticRevision(library)}, nil
}

func (s *MediaLibraryStructureService) Diagnostics(ctx context.Context, actor Actor, libraryID uint) (MediaLibraryStructureDiagnostics, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureDiagnostics{}, appError(CodePermissionDenied, "无权查看媒体库结构", nil)
	}
	var library models.MediaLibrary
	if err := s.db.First(&library, libraryID).Error; err != nil {
		return MediaLibraryStructureDiagnostics{}, mediaLibraryNotFound(err)
	}
	checkedAt := time.Time{}
	if library.StructureCheckedAt != nil {
		checkedAt = *library.StructureCheckedAt
	}
	return MediaLibraryStructureDiagnostics{LibraryID: libraryID, Status: library.StructureStatus, IssueCount: library.StructureIssueCount, CheckedAt: checkedAt, Issues: []StructureIssue{}, Revision: structureDiagnosticRevision(library)}, nil
}

func (s *MediaLibraryStructureService) PreviewRepair(ctx context.Context, actor Actor, libraryID uint, workKey, revision string) (MediaLibraryStructurePreview, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructurePreview{}, appError(CodePermissionDenied, "无权修复媒体库结构", nil)
	}
	var current models.MediaLibrary
	if err := s.db.First(&current, libraryID).Error; err != nil {
		return MediaLibraryStructurePreview{}, mediaLibraryNotFound(err)
	}
	if strings.TrimSpace(workKey) == "" && (strings.TrimSpace(revision) == "" || revision != structureDiagnosticRevision(current)) {
		return MediaLibraryStructurePreview{}, appError(CodeConflict, "目录诊断结果已变化，请重新检查", nil)
	}
	plan, _, err := s.buildPlan(ctx, libraryID, workKey)
	if err != nil {
		return MediaLibraryStructurePreview{}, err
	}
	planHash, err := structurePlanHash(plan)
	if err != nil {
		return MediaLibraryStructurePreview{}, err
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	claim := mediaLibraryStructureClaim{ActorID: actor.User.ID, LibraryID: libraryID, WorkKey: strings.TrimSpace(workKey), Generation: plan.Generation, RuleFingerprint: plan.RuleFingerprint, PlanHash: planHash, ExpiresAt: expires.Unix()}
	token, err := s.signStructureClaim(claim)
	if err != nil {
		return MediaLibraryStructurePreview{}, err
	}
	return MediaLibraryStructurePreview{LibraryID: libraryID, Revision: revision, IssueCount: plan.IssueCount, RepairableCount: len(plan.Items), MoveCount: len(plan.Items), Issues: plan.Issues, ConfirmationToken: token, ExpiresAt: expires}, nil
}

func (s *MediaLibraryStructureService) EnqueueRepair(ctx context.Context, actor Actor, libraryID uint, workKey string, request RequestContext) (models.MediaLibraryStructureRepair, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return models.MediaLibraryStructureRepair{}, appError(CodePermissionDenied, "无权修复媒体库结构", nil)
	}
	workKey = strings.TrimSpace(workKey)
	plan, library, err := s.buildPlan(ctx, libraryID, workKey)
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	return s.enqueueRepairPlan(actor, library, workKey, plan, request)
}

func (s *MediaLibraryStructureService) EnqueueConfirmedRepair(ctx context.Context, actor Actor, libraryID uint, workKey, token string, request RequestContext) (models.MediaLibraryStructureRepair, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return models.MediaLibraryStructureRepair{}, appError(CodePermissionDenied, "无权修复媒体库结构", nil)
	}
	claim, err := s.verifyStructureClaim(token)
	if err != nil || claim.ActorID != actor.User.ID || claim.LibraryID != libraryID || claim.WorkKey != strings.TrimSpace(workKey) || claim.ExpiresAt < time.Now().UTC().Unix() {
		return models.MediaLibraryStructureRepair{}, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", err)
	}
	plan, library, err := s.buildPlan(ctx, libraryID, workKey)
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	planHash, err := structurePlanHash(plan)
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	if claim.Generation != plan.Generation || claim.RuleFingerprint != plan.RuleFingerprint || !hmac.Equal([]byte(claim.PlanHash), []byte(planHash)) {
		return models.MediaLibraryStructureRepair{}, appError(CodeConflict, "媒体库内容或分类规则已变化，请重新诊断和预览", nil)
	}
	return s.enqueueRepairPlan(actor, library, workKey, plan, request)
}

func (s *MediaLibraryStructureService) enqueueRepairPlan(actor Actor, library models.MediaLibrary, workKey string, plan StructurePlan, request RequestContext) (models.MediaLibraryStructureRepair, error) {
	libraryID := library.ID
	if workKey != "" && len(workKey) > 80 {
		return models.MediaLibraryStructureRepair{}, appError(CodeInvalidRequest, "作品标识无效", nil)
	}
	scope := models.MediaLibraryStructureScopeFull
	if workKey != "" {
		scope = models.MediaLibraryStructureScopeWork
	}
	var active models.MediaLibraryStructureRepair
	query := s.db.Where("library_id = ? AND scope = ? AND work_key = ? AND phase IN ?", libraryID, scope, workKey, []string{"queued", "executing", "reconciling"}).Order("created_at DESC").First(&active)
	if query.Error == nil {
		return active, nil
	}
	if !errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return models.MediaLibraryStructureRepair{}, query.Error
	}
	raw, err := json.Marshal(plan)
	if err != nil || len(raw) > 8*1024*1024 {
		return models.MediaLibraryStructureRepair{}, appError(CodeMediaLibraryStructureUnavailable, "媒体库修复计划过大", err)
	}
	now := time.Now().UTC()
	repair := models.MediaLibraryStructureRepair{ID: uuid.NewString(), OwnerID: actor.User.ID, LibraryID: libraryID, Scope: scope, WorkKey: workKey, RuleFingerprint: plan.RuleFingerprint, Generation: plan.Generation, PlanJSON: string(raw), StateJSON: `{}`, Phase: "queued", IssueCount: plan.IssueCount, TotalItems: len(plan.Items), CreatedAt: now, UpdatedAt: now}
	job, err := s.queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: JobTypeMediaLibraryRepair, DisplayName: "修复媒体库结构 · " + library.Name, Provider: "media_library", ResourceKey: "library:" + strconv.FormatUint(uint64(libraryID), 10), Payload: mediaLibraryRepairJobPayload{RepairID: repair.ID}}, func(tx *gorm.DB, job models.Job) error {
		repair.JobID = &job.ID
		if err := tx.Create(&repair).Error; err != nil {
			return err
		}
		if scope == models.MediaLibraryStructureScopeFull {
			if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Updates(map[string]any{"structure_status": models.MediaLibraryStructureRepairing, "structure_error_code": ""}).Error; err != nil {
				return err
			}
		}
		return s.audit.Record(tx, &actor.User.ID, "media_library.structure_repair.enqueue", "media_library", uintID(libraryID), "success", map[string]any{"scope": scope, "issue_count": plan.IssueCount, "item_count": len(plan.Items)}, request)
	})
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	repair.JobID = &job.ID
	return repair, nil
}

func (s *MediaLibraryStructureService) EnqueueWorkRepair(ctx context.Context, actor Actor, libraryID uint, workToken string, request RequestContext) (models.MediaLibraryStructureRepair, error) {
	workKey, err := decodeCatalogToken(workToken)
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	return s.EnqueueRepair(ctx, actor, libraryID, workKey, request)
}

func (s *MediaLibraryStructureService) Repairs(actor Actor, libraryID uint, limit int) ([]models.MediaLibraryStructureRepair, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return nil, appError(CodePermissionDenied, "无权查看媒体库修复记录", nil)
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var repairs []models.MediaLibraryStructureRepair
	if err := s.db.Where("library_id = ?", libraryID).Order("created_at DESC").Limit(limit).Find(&repairs).Error; err != nil {
		return nil, err
	}
	return repairs, nil
}

// EnsureWorkLayout repairs only an already indexed matching work before a new
// transfer is planned. It is intentionally synchronous: a failed repair must
// prevent Transfer from creating a second directory tree for the same work.
func (s *MediaLibraryStructureService) EnsureWorkLayout(ctx context.Context, ownerID, libraryID uint, tmdbID int64, mediaType string) error {
	if tmdbID <= 0 || (mediaType != "movie" && mediaType != "tv") {
		return nil
	}
	var match struct{ WorkKey string }
	err := s.db.Model(&models.MediaLibraryEntry{}).Select("work_key").Where("library_id = ? AND tmdb_id = ? AND media_type = ? AND match_status = ?", libraryID, tmdbID, mediaType, mediaRecognitionStatusMatched).Order("id").First(&match).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	plan, library, err := s.buildPlan(ctx, libraryID, match.WorkKey)
	if err != nil || len(plan.Items) == 0 {
		return err
	}
	var storage models.Storage
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	backend, err := s.backends.Get(storage.Type)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	repair := models.MediaLibraryStructureRepair{ID: uuid.NewString(), OwnerID: ownerID, LibraryID: libraryID, Scope: models.MediaLibraryStructureScopeWork, WorkKey: match.WorkKey, RuleFingerprint: plan.RuleFingerprint, Generation: plan.Generation, PlanJSON: string(raw), StateJSON: `{}`, Phase: "executing", IssueCount: plan.IssueCount, TotalItems: len(plan.Items), CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&repair).Error; err != nil {
		return err
	}
	if err := backend.Apply(ctx, StructureBoundary{Library: library, Storage: storage}, plan.Items, nil); err != nil {
		code := CodeMediaLibraryStructureApplyFailed
		if errors.Is(err, errStructureConflict) {
			code = CodeMediaLibraryStructureConflict
		}
		_ = s.db.Model(&repair).Updates(map[string]any{"phase": "failed", "last_error_code": code, "finished_at": time.Now().UTC(), "updated_at": time.Now().UTC()}).Error
		return appError(code, "已有作品目录修复失败", err)
	}
	finished := time.Now().UTC()
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := updateStructureCatalogPaths(tx, libraryID, plan.Items, nil); err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ? AND baseline_generation = ?", libraryID, plan.Generation).UpdateColumn("dirty_generation", gorm.Expr("dirty_generation + 1")).Error; err != nil {
			return err
		}
		if err := tx.Model(&repair).Updates(map[string]any{"phase": "completed", "processed_items": len(plan.Items), "finished_at": finished, "updated_at": finished}).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &ownerID, "media_library.structure_repair.auto", "media_library", uintID(libraryID), "success", map[string]any{"scope": "work", "item_count": len(plan.Items)}, RequestContext{})
	}); err != nil {
		return err
	}
	return nil
}

func updateStructureCatalogPaths(tx *gorm.DB, libraryID uint, items []StructurePlanItem, providerParents map[string]string) error {
	for _, item := range items {
		model := tx.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND relative_path IN ?", libraryID, []string{item.SourceRelative, "/" + item.SourceRelative})
		if item.Kind == "sidecar" {
			model = tx.Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ? AND relative_path IN ?", libraryID, []string{item.SourceRelative, "/" + item.SourceRelative})
		}
		if err := model.Update("relative_path", "/"+item.TargetRelative).Error; err != nil {
			return err
		}
		if parentID := strings.TrimSpace(providerParents[item.ProviderID]); item.AllowProviderRootSource && parentID != "" {
			if err := tx.Model(&models.MediaManagedItem{}).Where("library_id = ? AND provider_item_id = ? AND managed = ? AND active = ?", libraryID, item.ProviderID, true, true).Update("provider_parent_id", parentID).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *MediaLibraryStructureService) buildPlan(_ context.Context, libraryID uint, workKey string) (StructurePlan, models.MediaLibrary, error) {
	var library models.MediaLibrary
	if err := s.db.First(&library, libraryID).Error; err != nil {
		return StructurePlan{}, library, mediaLibraryNotFound(err)
	}
	var entries []models.MediaLibraryEntry
	if err := s.db.Where("library_id = ?", libraryID).Order("relative_path").Find(&entries).Error; err != nil {
		return StructurePlan{}, library, err
	}
	if strings.TrimSpace(workKey) != "" {
		found := false
		for _, entry := range entries {
			if entry.WorkKey == strings.TrimSpace(workKey) {
				found = true
				break
			}
		}
		if !found {
			return StructurePlan{}, library, appError(CodeNotFound, "媒体库中没有这个作品", nil)
		}
	}
	var assets []models.MediaLibrarySourceAsset
	if err := s.db.Where("library_id = ? AND active = ?", libraryID, true).Order("relative_path").Find(&assets).Error; err != nil {
		return StructurePlan{}, library, err
	}
	plan, err := s.planner.Build(library, entries, assets, workKey)
	if err != nil || strings.TrimSpace(workKey) != "" || strings.TrimSpace(library.ProviderRootID) == "" || strings.TrimSpace(library.ProviderRootID) == "0" {
		return plan, library, err
	}
	// This is intentionally limited to rows already owned by OhMyCine. The
	// old cid=0 error placed such rows in 115's provider root, outside normal
	// scanning; include them in the existing read-only diagnose/confirmed-repair
	// workflow without adopting any unrelated root files.
	var misplaced []models.MediaManagedItem
	if err := s.db.Where("library_id = ? AND managed = ? AND active = ? AND provider_parent_id = ? AND provider_item_id <> ''", libraryID, true, true, "0").Order("id").Find(&misplaced).Error; err != nil {
		return StructurePlan{}, library, err
	}
	for _, managed := range misplaced {
		target := safeStructurePath(managed.RelativePath)
		if target == "" || (managed.Kind != models.MediaManagedItemKindVideo && managed.Kind != models.MediaManagedItemKindSidecar) || managed.Size < 0 {
			continue
		}
		plan.Items = append(plan.Items, StructurePlanItem{Kind: managed.Kind, SourceRelative: "网盘根目录/" + pathpkg.Base(target), TargetRelative: target, ProviderID: managed.ProviderItemID, ParentProviderID: "0", AllowProviderRootSource: true, Size: managed.Size})
		plan.addIssue(StructureIssue{Code: "cloud_transfer_root_misplaced", Kind: managed.Kind, Title: "历史 115 入库文件", CurrentPath: "网盘根目录/" + pathpkg.Base(target), ExpectedPath: target, Repairable: true})
	}
	return plan, library, nil
}

type MediaLibraryRepairWorker struct{ service *MediaLibraryStructureService }

func NewMediaLibraryRepairWorker(service *MediaLibraryStructureService) *MediaLibraryRepairWorker {
	return &MediaLibraryRepairWorker{service: service}
}

func (w *MediaLibraryRepairWorker) Run(ctx context.Context, runtime JobRuntime, job ClaimedJob) WorkerResult {
	var payload mediaLibraryRepairJobPayload
	if json.Unmarshal([]byte(job.Job.PayloadJSON), &payload) != nil || payload.RepairID == "" {
		return WorkerResult{ErrorCode: CodeMediaLibraryStructureUnavailable, ErrorMessage: "媒体库修复任务参数无效"}
	}
	return w.service.runRepair(ctx, runtime, payload.RepairID)
}

func (s *MediaLibraryStructureService) runRepair(ctx context.Context, runtime JobRuntime, repairID string) WorkerResult {
	var repair models.MediaLibraryStructureRepair
	if err := s.db.First(&repair, "id = ?", repairID).Error; err != nil {
		return WorkerResult{ErrorCode: CodeMediaLibraryStructureUnavailable, ErrorMessage: "媒体库修复任务不存在"}
	}
	if repair.Phase == "completed" {
		return WorkerResult{}
	}
	var plan StructurePlan
	if json.Unmarshal([]byte(repair.PlanJSON), &plan) != nil || plan.Version != 1 || plan.LibraryID != repair.LibraryID {
		return s.failRepair(repair, CodeMediaLibraryStructureUnavailable, "媒体库修复计划无效")
	}
	var library models.MediaLibrary
	var storage models.Storage
	if err := s.db.First(&library, repair.LibraryID).Error; err != nil {
		return s.failRepair(repair, CodeMediaLibraryStructureUnavailable, "媒体库不存在")
	}
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		return s.failRepair(repair, CodeMediaLibraryStructureUnavailable, "媒体库数据源不可用")
	}
	if plan.RuleFingerprint != libraryRuleFingerprint(library) || plan.Generation != library.BaselineGeneration {
		return s.failRepair(repair, CodeMediaLibraryStructureBoundaryChanged, "媒体库已变化，请重新诊断后再修复")
	}
	backend, err := s.backends.Get(storage.Type)
	if err != nil {
		return s.failRepair(repair, CodeMediaLibraryStructureUnavailable, "当前数据源不支持结构修复")
	}
	now := time.Now().UTC()
	_ = s.db.Model(&repair).Updates(map[string]any{"phase": "executing", "updated_at": now, "last_error_code": ""}).Error
	if repair.Scope == models.MediaLibraryStructureScopeFull {
		_ = s.db.Model(&library).Updates(map[string]any{"structure_status": models.MediaLibraryStructureRepairing, "structure_error_code": ""}).Error
	}
	progress := func(processed, total int) error {
		processed64, total64 := int64(processed), int64(total)
		percent := float64(100)
		if total > 0 {
			percent = float64(processed) * 100 / float64(total)
		}
		if err := runtime.Heartbeat(&percent, &processed64, &total64, nil, nil); err != nil {
			return err
		}
		return s.db.Model(&repair).Updates(map[string]any{"processed_items": processed, "updated_at": time.Now().UTC()}).Error
	}
	if err := backend.Apply(ctx, StructureBoundary{Library: library, Storage: storage}, plan.Items, progress); err != nil {
		code := CodeMediaLibraryStructureApplyFailed
		switch {
		case errors.Is(err, errStructureConflict):
			code = CodeMediaLibraryStructureConflict
		case errors.Is(err, errStructureFileLocked):
			code = CodeMediaLibraryStructureFileLocked
		case errors.Is(err, errStructurePermissionDenied):
			code = CodeMediaLibraryStructurePermissionDenied
		}
		return s.failRepair(repair, code, "媒体库结构修复失败")
	}
	providerParents, err := s.repairedManagedProviderParents(ctx, library, storage, plan.Items)
	if err != nil {
		return s.failRepair(repair, CodeMediaLibraryStructureApplyFailed, "媒体库结构修复结果验证失败")
	}
	finished := time.Now().UTC()
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := updateStructureCatalogPaths(tx, repair.LibraryID, plan.Items, providerParents); err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", repair.LibraryID).Updates(map[string]any{"dirty_generation": gorm.Expr("dirty_generation + 1"), "structure_status": models.MediaLibraryStructurePending, "structure_error_code": "", "structure_issue_count": 0}).Error; err != nil {
			return err
		}
		if err := tx.Model(&repair).Updates(map[string]any{"phase": "completed", "processed_items": len(plan.Items), "last_error_code": "", "finished_at": finished, "updated_at": finished}).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &repair.OwnerID, "media_library.structure_repair.complete", "media_library", uintID(repair.LibraryID), "success", map[string]any{"scope": repair.Scope, "item_count": len(plan.Items)}, RequestContext{})
	})
	if err != nil {
		return s.failRepair(repair, CodeMediaLibraryStructureApplyFailed, "媒体库修复结果保存失败")
	}
	if s.reconcile != nil {
		s.reconcile(repair.LibraryID)
	}
	return WorkerResult{}
}

// repairedManagedProviderParents records the actual post-move parent for the
// bounded set of historical cid=0 items. This keeps future diagnostics from
// mistaking a successfully repaired item for one still at provider root.
func (s *MediaLibraryStructureService) repairedManagedProviderParents(ctx context.Context, library models.MediaLibrary, storage models.Storage, items []StructurePlanItem) (map[string]string, error) {
	parents := map[string]string{}
	needsVerification := false
	for _, item := range items {
		needsVerification = needsVerification || item.AllowProviderRootSource
	}
	if !needsVerification {
		return parents, nil
	}
	if storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil || s.connections == nil {
		return nil, errors.New("provider repair verification is unavailable")
	}
	_, driver, err := s.connections.driver(*storage.ConnectionID)
	if err != nil {
		return nil, err
	}
	rootID := library.ProviderRootID
	if rootID == "" {
		rootID = storage.RootPath
	}
	for _, item := range items {
		if !item.AllowProviderRootSource {
			continue
		}
		stat, err := driver.Stat(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), item.ProviderID)
		if err != nil || stat.IsDir || stat.Name != pathpkg.Base(item.TargetRelative) || (item.Size > 0 && stat.Size != item.Size) {
			return nil, errors.New("repaired provider item identity changed")
		}
		within, err := providerParentWithinRoot(ctx, driver, stat.ParentID, rootID)
		if err != nil || !within || stat.ParentID == "0" {
			return nil, errors.New("repaired provider item remains outside media-library root")
		}
		parents[item.ProviderID] = stat.ParentID
	}
	return parents, nil
}

func (s *MediaLibraryStructureService) failRepair(repair models.MediaLibraryStructureRepair, code, message string) WorkerResult {
	now := time.Now().UTC()
	_ = s.db.Model(&repair).Updates(map[string]any{"phase": "failed", "last_error_code": code, "finished_at": now, "updated_at": now}).Error
	if repair.Scope == models.MediaLibraryStructureScopeFull {
		_ = s.db.Model(&models.MediaLibrary{}).Where("id = ?", repair.LibraryID).Updates(map[string]any{"structure_status": models.MediaLibraryStructureFailed, "structure_error_code": code, "structure_checked_at": now}).Error
	}
	return WorkerResult{ErrorCode: code, ErrorMessage: message}
}

var (
	errStructureConflict         = errors.New("structure target conflict")
	errStructureFileLocked       = errors.New("structure file is locked")
	errStructurePermissionDenied = errors.New("structure path permission denied")
)

type localMediaLibraryStructureBackend struct{}

func (localMediaLibraryStructureBackend) StorageType() string { return models.StorageTypeLocal }

func (localMediaLibraryStructureBackend) Apply(ctx context.Context, boundary StructureBoundary, items []StructurePlanItem, progress StructureProgress) error {
	root, err := medialibrary.ResolveRoot(boundary.Storage.RootPath, boundary.Library.RelativeRoot)
	if err != nil {
		return err
	}
	oldDirectories := make(map[string]struct{}, len(items))
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := filepath.Join(root, filepath.FromSlash(item.SourceRelative))
		target := filepath.Join(root, filepath.FromSlash(item.TargetRelative))
		if ensureWithin(root, source) != nil || ensureWithin(root, target) != nil {
			return errors.New("structure path escapes library root")
		}
		oldDirectories[filepath.Dir(source)] = struct{}{}
		sourceInfo, sourceErr := os.Lstat(source)
		targetInfo, targetErr := os.Lstat(target)
		if errors.Is(sourceErr, os.ErrNotExist) && targetErr == nil && targetInfo.Mode().IsRegular() && (item.Size <= 0 || targetInfo.Size() == item.Size) {
			if progress != nil {
				if err := progress(index+1, len(items)); err != nil {
					return err
				}
			}
			continue
		}
		if sourceErr != nil || !sourceInfo.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(source, fs.FileInfoToDirEntry(sourceInfo)) || (item.Size > 0 && sourceInfo.Size() != item.Size) {
			return errors.New("structure source identity changed")
		}
		if targetErr == nil {
			return errStructureConflict
		}
		if !errors.Is(targetErr, os.ErrNotExist) {
			return targetErr
		}
		if err := ensureSafeDirectoryPath(root, filepath.Dir(target), true); err != nil {
			return err
		}
		if err := retryLocalStructureMutation(ctx, func() error { return os.Rename(source, target) }); err != nil {
			return err
		}
		if progress != nil {
			if err := progress(index+1, len(items)); err != nil {
				return err
			}
		}
	}
	return removeEmptyLocalStructureDirectories(root, oldDirectories)
}

func removeEmptyLocalStructureDirectories(root string, directories map[string]struct{}) error {
	list := make([]string, 0, len(directories)*2)
	seen := map[string]struct{}{}
	for directory := range directories {
		for current := directory; current != root; current = filepath.Dir(current) {
			if ensureWithin(root, current) != nil || current == filepath.Dir(current) {
				break
			}
			if _, exists := seen[current]; !exists {
				seen[current] = struct{}{}
				list = append(list, current)
			}
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return strings.Count(list[i], string(filepath.Separator)) > strings.Count(list[j], string(filepath.Separator))
	})
	for _, directory := range list {
		if err := retryLocalStructureMutation(context.Background(), func() error { return os.Remove(directory) }); err != nil && !errors.Is(err, os.ErrNotExist) && !isDirectoryNotEmpty(err) {
			return err
		}
	}
	return nil
}

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, fs.ErrExist) || strings.Contains(strings.ToLower(err.Error()), "not empty") || strings.Contains(err.Error(), "目录不是空的")
}

func retryLocalStructureMutation(ctx context.Context, mutate func() error) error {
	delays := []time.Duration{0, 60 * time.Millisecond, 180 * time.Millisecond, 450 * time.Millisecond}
	var last error
	for _, delay := range delays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		last = mutate()
		if last == nil || !isWindowsSharingViolation(last) {
			break
		}
	}
	if last == nil {
		return nil
	}
	if isWindowsSharingViolation(last) {
		return fmt.Errorf("%w: %v", errStructureFileLocked, last)
	}
	if errors.Is(last, fs.ErrPermission) {
		return fmt.Errorf("%w: %v", errStructurePermissionDenied, last)
	}
	return last
}

func isWindowsSharingViolation(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33)) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "being used by another process") || strings.Contains(message, "sharing violation") || strings.Contains(message, "lock violation") || strings.Contains(message, "正由另一进程使用")
}

type pan115MediaLibraryStructureBackend struct {
	driver func(uint) (cloudpkg.Driver, error)
}

func (pan115MediaLibraryStructureBackend) StorageType() string { return models.StorageTypePan115 }

func (b pan115MediaLibraryStructureBackend) Apply(ctx context.Context, boundary StructureBoundary, items []StructurePlanItem, progress StructureProgress) error {
	if boundary.Storage.ConnectionID == nil || b.driver == nil {
		return errors.New("provider connection is unavailable")
	}
	driver, err := b.driver(*boundary.Storage.ConnectionID)
	if err != nil {
		return err
	}
	mutations, ok := driver.(cloudpkg.MutationDriver)
	if !ok {
		return errors.New("provider mutation is unavailable")
	}
	rootID := boundary.Library.ProviderRootID
	if rootID == "" {
		rootID = boundary.Storage.RootPath
	}
	directoryCache := map[string]string{"": rootID, ".": rootID}
	oldParents := map[string]struct{}{}
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.ProviderID == "" {
			return errors.New("provider item identity is missing")
		}
		stat, err := driver.Stat(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), item.ProviderID)
		if err != nil || stat.IsDir || (item.Size > 0 && stat.Size != item.Size) {
			return errors.New("provider item identity changed")
		}
		within, err := providerParentWithinRoot(ctx, driver, stat.ParentID, rootID)
		if err != nil || !within {
			// Only a signed, internally generated historical repair plan may move
			// a managed item out of the 115 provider root. Keep the exception
			// exact: it never authorizes another external directory as a source.
			if !item.AllowProviderRootSource || stat.ParentID != "0" || rootID == "0" || stat.Name != pathpkg.Base(item.TargetRelative) {
				return errors.New("provider item escaped library root")
			}
		}
		targetDirectory := pathpkg.Dir(item.TargetRelative)
		if targetDirectory == "." {
			targetDirectory = ""
		}
		targetParent, err := ensureReorganizationCloudDirectory(ctx, driver, mutations, rootID, targetDirectory, directoryCache)
		if err != nil {
			return err
		}
		targetName := pathpkg.Base(item.TargetRelative)
		// A provider move may have committed immediately before the worker lost
		// its lease or the database checkpoint failed. Accept only the exact
		// planned destination on retry; any other in-library location remains a
		// fail-closed source change.
		if item.AllowProviderRootSource && within && (stat.ParentID != targetParent || stat.Name != targetName) {
			return errors.New("historical provider-root repair source changed")
		}
		conflictID, err := providerChildID(ctx, driver, targetParent, targetName)
		if err != nil {
			return err
		}
		if conflictID != "" && conflictID != item.ProviderID {
			return errStructureConflict
		}
		if stat.ParentID != targetParent || stat.Name != targetName {
			if stat.ParentID != "" {
				oldParents[stat.ParentID] = struct{}{}
			}
			if stat.ParentID != targetParent {
				if err := mutations.Move(ctx, item.ProviderID, targetParent); err != nil {
					return err
				}
			}
			if stat.Name != targetName {
				if err := mutations.Rename(ctx, item.ProviderID, targetName); err != nil {
					return err
				}
			}
		}
		if progress != nil {
			if err := progress(index+1, len(items)); err != nil {
				return err
			}
		}
	}
	protected := make(map[string]struct{}, len(directoryCache)+1)
	protected[rootID] = struct{}{}
	for _, id := range directoryCache {
		protected[id] = struct{}{}
	}
	return cleanupEmptyProviderStructureDirectories(ctx, driver, mutations, rootID, oldParents, protected)
}

func providerChildID(ctx context.Context, driver cloudpkg.Driver, parentID, name string) (string, error) {
	for offset := int64(0); ; {
		page, err := driver.List(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), parentID, cloudpkg.PageRequest{Offset: offset, Limit: 200})
		if err != nil {
			return "", err
		}
		for _, child := range page.Items {
			if strings.EqualFold(child.Name, name) {
				return child.ID, nil
			}
		}
		if !page.HasMore || len(page.Items) == 0 {
			return "", nil
		}
		offset += int64(len(page.Items))
	}
}

func providerParentWithinRoot(ctx context.Context, driver cloudpkg.Driver, parentID, rootID string) (bool, error) {
	for current, depth := parentID, 0; current != "" && depth < 64; depth++ {
		if current == rootID {
			return true, nil
		}
		item, err := driver.Stat(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), current)
		if err != nil {
			return false, err
		}
		if !item.IsDir || item.ParentID == current {
			return false, nil
		}
		current = item.ParentID
	}
	return false, nil
}

func cleanupEmptyProviderStructureDirectories(ctx context.Context, driver cloudpkg.Driver, mutations cloudpkg.MutationDriver, rootID string, initial, protected map[string]struct{}) error {
	depths := make(map[string]int, len(initial)*2)
	for id := range initial {
		chain := make([]string, 0, 8)
		chainSeen := map[string]struct{}{}
		current := id
		reachedRoot := false
		for depth := 0; current != "" && depth < 64; depth++ {
			if current == rootID {
				reachedRoot = true
				break
			}
			if _, exists := chainSeen[current]; exists {
				break
			}
			chainSeen[current] = struct{}{}
			stat, err := driver.Stat(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), current)
			if err != nil || !stat.IsDir {
				break
			}
			chain = append(chain, current)
			current = stat.ParentID
		}
		if !reachedRoot {
			continue
		}
		for index, candidate := range chain {
			depth := len(chain) - index
			if depth > depths[candidate] {
				depths[candidate] = depth
			}
		}
	}
	candidates := make([]string, 0, len(depths))
	for id := range depths {
		candidates = append(candidates, id)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if depths[candidates[i]] != depths[candidates[j]] {
			return depths[candidates[i]] > depths[candidates[j]]
		}
		return candidates[i] < candidates[j]
	})
	for _, id := range candidates {
		if _, keep := protected[id]; keep || id == rootID {
			continue
		}
		page, err := driver.List(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), id, cloudpkg.PageRequest{Limit: 1})
		if err != nil {
			return err
		}
		if len(page.Items) == 0 && !page.HasMore {
			if err := mutations.Recycle(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}
