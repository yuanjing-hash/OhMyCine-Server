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
	"regexp"
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
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	JobTypeMediaLibraryRepair             = "media_library_repair"
	JobTypeMediaLibraryStructureDiagnosis = "media_library_structure_diagnosis"
)

type StructureBoundary struct {
	Library models.MediaLibrary
	Storage models.Storage
}

type StructureProgress func(processed, total int) error

type MediaLibraryStructureBackend interface {
	StorageType() string
	ValidateRecycle(context.Context, StructureBoundary) error
	Recycle(context.Context, StructureBoundary, []StructureRecycleItem, StructureProgress) error
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
	LibraryID       uint                          `json:"library_id"`
	JobID           string                        `json:"job_id,omitempty"`
	ScanRunID       *uint                         `json:"scan_run_id,omitempty"`
	Generation      uint64                        `json:"generation"`
	ScanKind        string                        `json:"scan_kind"`
	Status          string                        `json:"status"`
	TotalItems      int                           `json:"total_items"`
	ProcessedItems  int                           `json:"processed_items"`
	IssueCount      int                           `json:"issue_count"`
	RepairableCount int                           `json:"repairable_count"`
	Unrecognized    int                           `json:"unrecognized"`
	Classifications StructureIssueClassifications `json:"classifications"`
	ErrorCode       string                        `json:"error_code"`
	StartedAt       *time.Time                    `json:"started_at,omitempty"`
	CheckedAt       *time.Time                    `json:"checked_at,omitempty"`
	Issues          []StructureIssue              `json:"issues"`
	Revision        string                        `json:"revision"`
}

type MediaLibraryStructureIssueMemberSummary struct {
	Token       string `json:"token"`
	SourcePath  string `json:"source_path"`
	Recommended bool   `json:"recommended"`
}

type MediaLibraryStructureIssueSummary struct {
	Token                  string                                    `json:"token"`
	Code                   string                                    `json:"code"`
	Kind                   string                                    `json:"kind"`
	State                  string                                    `json:"state"`
	Repairable             bool                                      `json:"repairable"`
	Title                  string                                    `json:"title,omitempty"`
	CurrentPath            string                                    `json:"current_path,omitempty"`
	ExpectedPath           string                                    `json:"expected_path,omitempty"`
	RecognitionToken       string                                    `json:"recognition_token,omitempty"`
	MediaType              string                                    `json:"media_type,omitempty"`
	ReleaseYear            *int                                      `json:"release_year,omitempty"`
	TMDBID                 *int64                                    `json:"tmdb_id,omitempty"`
	PosterPath             string                                    `json:"poster_path,omitempty"`
	ConflictSourceCount    int                                       `json:"conflict_source_count,omitempty"`
	RecommendedMemberToken string                                    `json:"recommended_member_token,omitempty"`
	Members                []MediaLibraryStructureIssueMemberSummary `json:"members"`
}

type MediaLibraryStructureIssuePage struct {
	List     []MediaLibraryStructureIssueSummary `json:"list"`
	Total    int64                               `json:"total"`
	Page     int                                 `json:"page"`
	PageSize int                                 `json:"page_size"`
}

type MediaLibraryStructureIssueQuery struct {
	Page       int
	PageSize   int
	Code       string
	Actionable bool
}

type mediaLibraryStructureDiagnosisJobPayload struct {
	LibraryID      uint   `json:"library_id"`
	ScanRunID      uint   `json:"scan_run_id"`
	Generation     uint64 `json:"generation"`
	ScanKind       string `json:"scan_kind"`
	Automatic      bool   `json:"automatic,omitempty"`
	SourceRevision uint64 `json:"source_revision,omitempty"`
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
	DraftID         string `json:"draft_id,omitempty"`
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

var structureDuplicateSuffixPattern = regexp.MustCompile(`(?i)(?:\s*[\(（\[]\d+[\)）\]]|\s*(?:副本|copy)(?:\s*\d+)?)$`)

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
	workKey = strings.TrimSpace(workKey)
	if workKey != "" {
		plan, library, err := s.buildPlan(ctx, libraryID, workKey)
		if err != nil {
			return MediaLibraryStructureDiagnostics{}, err
		}
		now := time.Now().UTC()
		status := models.MediaLibraryStructureHealthy
		if plan.IssueCount > 0 {
			status = models.MediaLibraryStructureIssues
		}
		return diagnosticsFromPlan(library, plan, status, nil, "manual", &now), nil
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return MediaLibraryStructureDiagnostics{}, mediaLibraryNotFound(err)
	}
	var latest models.MediaLibraryScanRun
	scanRunID := uint(0)
	scanKind := "manual"
	if err := s.db.WithContext(ctx).Where("library_id = ? AND generation = ? AND catalog_published_at IS NOT NULL", libraryID, library.BaselineGeneration).Order("id DESC").First(&latest).Error; err == nil {
		scanRunID, scanKind = latest.ID, latest.Kind
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return MediaLibraryStructureDiagnostics{}, err
	}
	if err := s.EnqueueDiagnosis(ctx, libraryID, scanRunID, library.BaselineGeneration, scanKind); err != nil {
		return MediaLibraryStructureDiagnostics{}, err
	}
	return s.diagnosticsForLibrary(ctx, library)
}

func (s *MediaLibraryStructureService) Diagnostics(ctx context.Context, actor Actor, libraryID uint) (MediaLibraryStructureDiagnostics, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureDiagnostics{}, appError(CodePermissionDenied, "无权查看媒体库结构", nil)
	}
	var library models.MediaLibrary
	if err := s.db.First(&library, libraryID).Error; err != nil {
		return MediaLibraryStructureDiagnostics{}, mediaLibraryNotFound(err)
	}
	return s.diagnosticsForLibrary(ctx, library)
}

func (s *MediaLibraryStructureService) StructureIssues(ctx context.Context, actor Actor, libraryID uint, query MediaLibraryStructureIssueQuery) (MediaLibraryStructureIssuePage, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureIssuePage{}, appError(CodePermissionDenied, "无权查看媒体库结构", nil)
	}
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 {
		query.PageSize = 50
	}
	if query.PageSize > 200 {
		query.PageSize = 200
	}
	query.Code = safeLabel(strings.TrimSpace(query.Code), 64)
	db := s.db.WithContext(ctx).Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ?", libraryID)
	if query.Code != "" && query.Code != "all" {
		db = db.Where("code = ?", query.Code)
	}
	if query.Actionable {
		db = db.Where("code <> ?", "missing_season_episode")
	}
	var total int64
	if err := db.Count(&total).Error; err != nil {
		return MediaLibraryStructureIssuePage{}, err
	}
	var rows []models.MediaLibraryStructureIssue
	if err := db.Order("code,id").Offset((query.Page - 1) * query.PageSize).Limit(query.PageSize).Find(&rows).Error; err != nil {
		return MediaLibraryStructureIssuePage{}, err
	}
	issueIDs := make([]uint, 0, len(rows))
	recognitionIDs := make([]uint, 0, len(rows))
	for _, row := range rows {
		issueIDs = append(issueIDs, row.ID)
		if row.RecognitionID != nil {
			recognitionIDs = append(recognitionIDs, *row.RecognitionID)
		}
	}
	membersByIssue, err := s.loadStructureIssueMembers(ctx, issueIDs)
	if err != nil {
		return MediaLibraryStructureIssuePage{}, err
	}
	recognitionsByID := make(map[uint]models.MediaLibraryRecognition, len(recognitionIDs))
	if len(recognitionIDs) > 0 {
		var recognitions []models.MediaLibraryRecognition
		if err := s.db.WithContext(ctx).Where("library_id = ? AND id IN ?", libraryID, recognitionIDs).Find(&recognitions).Error; err != nil {
			return MediaLibraryStructureIssuePage{}, err
		}
		for _, recognition := range recognitions {
			recognitionsByID[recognition.ID] = recognition
		}
	}
	result := MediaLibraryStructureIssuePage{List: make([]MediaLibraryStructureIssueSummary, 0, len(rows)), Total: total, Page: query.Page, PageSize: query.PageSize}
	for _, row := range rows {
		members := membersByIssue[row.ID]
		item := MediaLibraryStructureIssueSummary{Token: row.Token, Code: row.Code, Kind: row.Kind, State: row.State, Repairable: row.Repairable, Title: safeMediaDisplayName(row.Title), CurrentPath: safeStructurePath(row.CurrentPath), ExpectedPath: safeStructurePath(row.ExpectedPath), ConflictSourceCount: row.ConflictSourceCount, RecommendedMemberToken: row.RecommendedMemberToken, Members: make([]MediaLibraryStructureIssueMemberSummary, 0, len(members))}
		if row.RecognitionID != nil {
			item.RecognitionToken = encodeRecognitionToken(*row.RecognitionID)
			if recognition, exists := recognitionsByID[*row.RecognitionID]; exists {
				item.MediaType, item.ReleaseYear, item.TMDBID = recognition.MediaType, cloneInt(recognition.ReleaseYear), cloneInt64(recognition.TMDBID)
				if _, snapshot, decodeErr := decodeRecognitionMetadata(recognition.MetadataJSON); decodeErr == nil {
					item.PosterPath = snapshot.PosterPath
				}
			}
		}
		for _, member := range members {
			sourcePath := safeStructurePath(member.SourcePath)
			if sourcePath == "" {
				if member.Token == item.RecommendedMemberToken {
					item.RecommendedMemberToken = ""
				}
				continue
			}
			item.Members = append(item.Members, MediaLibraryStructureIssueMemberSummary{Token: member.Token, SourcePath: sourcePath, Recommended: member.Recommended})
		}
		item.ConflictSourceCount = len(item.Members)
		result.List = append(result.List, item)
	}
	return result, nil
}

func (s *MediaLibraryStructureService) loadStructureIssueMembers(ctx context.Context, issueIDs []uint) (map[uint][]models.MediaLibraryStructureIssueMember, error) {
	const batchSize = 400
	result := make(map[uint][]models.MediaLibraryStructureIssueMember, len(issueIDs))
	for start := 0; start < len(issueIDs); start += batchSize {
		end := min(start+batchSize, len(issueIDs))
		var members []models.MediaLibraryStructureIssueMember
		if err := s.db.WithContext(ctx).Where("issue_id IN ?", issueIDs[start:end]).Order("issue_id,id").Find(&members).Error; err != nil {
			return nil, err
		}
		for _, member := range members {
			result[member.IssueID] = append(result[member.IssueID], member)
		}
	}
	return result, nil
}

// RefreshRecognitionProjection updates only the diagnosis rows owned by one
// recognition. It deliberately performs no provider or filesystem operation.
func (s *MediaLibraryStructureService) RefreshRecognitionProjection(ctx context.Context, libraryID, recognitionID uint) error {
	if recognitionID == 0 {
		return nil
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return mediaLibraryNotFound(err)
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&diagnosis).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if diagnosis.Generation != library.BaselineGeneration {
		return nil
	}
	var affectedEntries []models.MediaLibraryEntry
	if err := s.db.WithContext(ctx).Where("library_id = ? AND recognition_id = ?", libraryID, recognitionID).Order("relative_path").Find(&affectedEntries).Error; err != nil {
		return err
	}
	if len(affectedEntries) == 0 {
		return nil
	}
	// Rebuild against all source facts so a newly confirmed identity cannot be
	// projected as a simple move when another work or sidecar already claims the
	// same target. This remains a targeted database projection: no diagnosis job
	// is enqueued and no filesystem/provider mutation is performed.
	entries, err := s.loadStructureEntries(ctx, libraryID)
	if err != nil {
		return err
	}
	assets, err := s.loadStructureAssets(ctx, libraryID)
	if err != nil {
		return err
	}
	plan, err := s.planner.BuildContext(ctx, library, entries, assets, "", nil)
	if err != nil {
		return err
	}
	affectedPaths := make(map[string]struct{}, len(affectedEntries))
	affectedWorks := make(map[string]struct{}, len(affectedEntries))
	for _, entry := range affectedEntries {
		if value := safeStructurePath(entry.RelativePath); value != "" {
			affectedPaths[value] = struct{}{}
		}
		if entry.WorkKey != "" {
			affectedWorks[entry.WorkKey] = struct{}{}
		}
	}
	projectedIssues := make([]StructureIssue, 0, len(affectedEntries))
	for _, issue := range plan.AllIssues {
		_, affected := affectedWorks[issue.WorkKey]
		if issue.RecognitionID == recognitionID {
			affected = true
		}
		if _, exists := affectedPaths[safeStructurePath(issue.CurrentPath)]; exists {
			affected = true
		}
		for _, source := range issue.AllConflictSources {
			if _, exists := affectedPaths[safeStructurePath(source)]; exists {
				affected = true
				break
			}
		}
		if affected {
			projectedIssues = append(projectedIssues, issue)
		}
	}
	if len(projectedIssues) == 0 {
		current := safeStructurePath(affectedEntries[0].RelativePath)
		projectedIssues = []StructureIssue{{Code: "manual_identity_resolved", Kind: "video", WorkKey: affectedEntries[0].WorkKey, Title: affectedEntries[0].Title, CurrentPath: current, ExpectedPath: current, RecognitionID: recognitionID}}
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Conflict groups can contain several recognition identities. Remove any
		// group that references one of this recognition's safe source paths so a
		// stale pre-override conflict is never shown as current truth.
		paths := make([]string, 0, len(affectedEntries))
		for _, entry := range affectedEntries {
			if value := safeStructurePath(entry.RelativePath); value != "" {
				paths = append(paths, value)
			}
		}
		query := tx.Where("library_id = ? AND recognition_id = ?", libraryID, recognitionID)
		if len(paths) > 0 {
			query = tx.Where("library_id = ? AND (recognition_id = ? OR id IN (SELECT issue_id FROM media_library_structure_issue_members WHERE source_path IN ?))", libraryID, recognitionID, paths)
		}
		if err := query.Delete(&models.MediaLibraryStructureIssue{}).Error; err != nil {
			return err
		}
		return insertStructureIssuesTx(tx, libraryID, diagnosis.JobID, diagnosis.Generation, projectedIssues, now, "manual_identity_resolved")
	})
}

func diagnosticsFromPlan(library models.MediaLibrary, plan StructurePlan, status string, scanRunID *uint, scanKind string, checkedAt *time.Time) MediaLibraryStructureDiagnostics {
	return MediaLibraryStructureDiagnostics{
		LibraryID: library.ID, ScanRunID: scanRunID, Generation: plan.Generation, ScanKind: scanKind,
		Status: status, TotalItems: plan.CheckedItems, ProcessedItems: plan.CheckedItems, IssueCount: plan.IssueCount,
		RepairableCount: len(plan.Items), Unrecognized: plan.Unrecognized, Classifications: plan.Classifications,
		CheckedAt: checkedAt, Issues: plan.Issues, Revision: structureDiagnosticRevision(library),
	}
}

func (s *MediaLibraryStructureService) diagnosticsForLibrary(ctx context.Context, library models.MediaLibrary) (MediaLibraryStructureDiagnostics, error) {
	var diagnosis models.MediaLibraryStructureDiagnosis
	err := s.db.WithContext(ctx).First(&diagnosis, "library_id = ?", library.ID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return MediaLibraryStructureDiagnostics{LibraryID: library.ID, Generation: library.BaselineGeneration, Status: library.StructureStatus, IssueCount: library.StructureIssueCount, ErrorCode: library.StructureErrorCode, CheckedAt: library.StructureCheckedAt, Issues: []StructureIssue{}, Revision: structureDiagnosticRevision(library)}, nil
	}
	if err != nil {
		return MediaLibraryStructureDiagnostics{}, err
	}
	issues := make([]StructureIssue, 0)
	if json.Unmarshal([]byte(diagnosis.IssuesJSON), &issues) != nil || len(issues) > maxStructureIssueSamples {
		issues = []StructureIssue{}
	}
	for index := range issues {
		issues[index].Title = safeMediaDisplayName(issues[index].Title)
		issues[index].CurrentPath = safeStructurePath(issues[index].CurrentPath)
		issues[index].ExpectedPath = safeStructurePath(issues[index].ExpectedPath)
		issues[index].ConflictSources = sanitizeStructureConflictSources(issues[index].ConflictSources)
		if issues[index].ConflictSourceCount < len(issues[index].ConflictSources) {
			issues[index].ConflictSourceCount = len(issues[index].ConflictSources)
		}
	}
	classifications := StructureIssueClassifications{
		Unrecognized: diagnosis.UnrecognizedCount, MissingEpisode: diagnosis.MissingEpisodeCount,
		InvalidPath: diagnosis.InvalidPathCount, TemplateError: diagnosis.TemplateErrorCount,
		DuplicateTarget: diagnosis.DuplicateTargetCount, SidecarConflict: diagnosis.SidecarConflictCount,
	}
	return MediaLibraryStructureDiagnostics{
		LibraryID: diagnosis.LibraryID, JobID: diagnosis.JobID, ScanRunID: cloneOptionalUint(diagnosis.ScanRunID), Generation: diagnosis.Generation,
		ScanKind: diagnosis.ScanKind, Status: diagnosis.Status, TotalItems: diagnosis.TotalItems, ProcessedItems: diagnosis.ProcessedItems,
		IssueCount: diagnosis.IssueCount, RepairableCount: diagnosis.RepairableCount, Unrecognized: diagnosis.UnrecognizedCount,
		Classifications: classifications, ErrorCode: diagnosis.LastErrorCode, StartedAt: diagnosis.StartedAt,
		CheckedAt: diagnosis.FinishedAt, Issues: issues, Revision: structureDiagnosticRevision(library),
	}, nil
}

func (s *MediaLibraryStructureService) EnqueueDiagnosis(ctx context.Context, libraryID, scanRunID uint, generation uint64, scanKind string) error {
	return s.enqueueDiagnosis(ctx, libraryID, scanRunID, generation, scanKind, false, 0)
}

// EnqueueAutomaticDiagnosis is the only scan-driven entry point. A source
// revision receives one automatic diagnosis after its complete recognition
// projection converges; routine scans and manual overrides cannot reset it.
func (s *MediaLibraryStructureService) EnqueueAutomaticDiagnosis(ctx context.Context, libraryID, scanRunID uint, generation uint64, scanKind string) error {
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return mediaLibraryNotFound(err)
	}
	var autoState models.MediaLibraryStructureAutoState
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&autoState).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		autoState = models.MediaLibraryStructureAutoState{LibraryID: libraryID, SourceRevision: 1, Status: "pending", UpdatedAt: time.Now().UTC()}
		if err := s.db.WithContext(ctx).Create(&autoState).Error; err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if autoState.SourceRevision == 0 || autoState.DiagnosedRevision >= autoState.SourceRevision {
		return nil
	}
	if autoState.Status == "queued" || autoState.Status == "running" {
		var diagnosis models.MediaLibraryStructureDiagnosis
		if err := s.db.WithContext(ctx).Where("library_id = ? AND automatic = ? AND source_revision = ?", libraryID, true, autoState.SourceRevision).First(&diagnosis).Error; err == nil && diagnosis.Generation == generation {
			return nil
		}
	}
	if scanRunID == 0 {
		return nil
	}
	var run models.MediaLibraryScanRun
	if err := s.db.WithContext(ctx).First(&run, scanRunID).Error; err != nil {
		return err
	}
	if run.LibraryID != libraryID || run.Generation != generation || run.Partial || run.CatalogPublishedAt == nil || run.Status != "success" || run.RecognitionCompleted < run.RecognitionTotal {
		return nil
	}
	return s.enqueueDiagnosis(ctx, libraryID, scanRunID, generation, scanKind, true, autoState.SourceRevision)
}

func (s *MediaLibraryStructureService) enqueueDiagnosis(ctx context.Context, libraryID, scanRunID uint, generation uint64, scanKind string, automatic bool, sourceRevision uint64) error {
	if s.queue == nil {
		return errors.New("media library structure diagnosis queue is unavailable")
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return mediaLibraryNotFound(err)
	}
	if generation == 0 {
		generation = library.BaselineGeneration
	}
	if library.BaselineGeneration != generation {
		return appError(CodeConflict, "媒体库目录代际已经变化", nil)
	}
	if automatic {
		var autoState models.MediaLibraryStructureAutoState
		if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&autoState).Error; err != nil {
			return err
		}
		if sourceRevision == 0 || autoState.SourceRevision != sourceRevision || autoState.DiagnosedRevision >= sourceRevision {
			return nil
		}
	}
	scanKind = safeLabel(scanKind, 24)
	var scanRunIDPtr *uint
	if scanRunID != 0 {
		var run models.MediaLibraryScanRun
		if err := s.db.WithContext(ctx).First(&run, scanRunID).Error; err != nil || run.LibraryID != libraryID || run.Generation != generation || run.CatalogPublishedAt == nil {
			return appError(CodeConflict, "媒体库扫描关联已经变化", err)
		}
		scanRunIDPtr = &scanRunID
		if scanKind == "" {
			scanKind = run.Kind
		}
	}
	payload := mediaLibraryStructureDiagnosisJobPayload{LibraryID: libraryID, ScanRunID: scanRunID, Generation: generation, ScanKind: scanKind, Automatic: automatic, SourceRevision: sourceRevision}
	now := time.Now().UTC()
	job, err := s.queue.EnqueueLatestWith(EnqueueJobInput{
		System: true, JobType: JobTypeMediaLibraryStructureDiagnosis, Priority: 15,
		DisplayName: "目录结构诊断 · " + safeMediaDisplayName(library.Name), Provider: "media_library",
		ResourceKey: "structure-diagnosis-library:" + strconv.FormatUint(uint64(libraryID), 10), CoalescingKey: "latest_generation", Payload: payload,
	}, func(tx *gorm.DB, job models.Job) error {
		diagnosis := models.MediaLibraryStructureDiagnosis{
			LibraryID: libraryID, JobID: job.ID, ScanRunID: scanRunIDPtr, Generation: generation, ScanKind: scanKind,
			Automatic: automatic, SourceRevision: sourceRevision, Status: models.MediaLibraryStructureQueued, IssuesJSON: "[]", CreatedAt: now, UpdatedAt: now,
		}
		columns := []string{"job_id", "scan_run_id", "generation", "scan_kind", "automatic", "source_revision", "status", "total_items", "processed_items", "issue_count", "repairable_count", "unrecognized_count", "missing_episode_count", "invalid_path_count", "template_error_count", "duplicate_target_count", "sidecar_conflict_count", "issues_json", "last_error_code", "started_at", "finished_at", "updated_at"}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "library_id"}}, DoUpdates: clause.AssignmentColumns(columns)}).Create(&diagnosis).Error; err != nil {
			return err
		}
		updates := map[string]any{
			"structure_status": models.MediaLibraryStructureQueued, "structure_issue_count": 0, "structure_error_code": "", "structure_checked_at": nil,
		}
		where := tx.Model(&models.MediaLibrary{}).Where("id = ? AND baseline_generation = ?", libraryID, generation)
		if automatic {
			stateUpdate := tx.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ? AND source_revision = ? AND diagnosed_revision < ?", libraryID, sourceRevision, sourceRevision).Updates(map[string]any{"status": "queued", "updated_at": now})
			if stateUpdate.Error != nil {
				return stateUpdate.Error
			}
			if stateUpdate.RowsAffected != 1 {
				return appError(CodeConflict, "媒体库自动诊断来源版本已经变化", nil)
			}
		}
		updated := where.Updates(updates)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return appError(CodeConflict, "媒体库目录代际已经变化", nil)
		}
		return nil
	})
	if err != nil {
		failedAt := time.Now().UTC()
		updates := map[string]any{"structure_status": models.MediaLibraryStructureFailed, "structure_error_code": CodeMediaLibraryStructureDiagnosisFailed, "structure_checked_at": failedAt}
		_ = s.db.Model(&models.MediaLibrary{}).Where("id = ? AND baseline_generation = ?", libraryID, generation).Updates(updates).Error
		if automatic {
			_ = s.db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ? AND source_revision = ?", libraryID, sourceRevision).Updates(map[string]any{"status": "failed", "updated_at": failedAt}).Error
		}
		return err
	}
	event := serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Info()).Uint("library_id", libraryID).Uint64("generation", generation).Str("scan_kind", scanKind).Str("phase", "queued").Str("action", "diagnosis_queued").Str("job_id", job.ID)
	if scanRunID != 0 {
		event = event.Uint("scan_run_id", scanRunID)
	}
	event.Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("目录结构诊断已进入持久任务队列"))
	return nil
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

func (s *MediaLibraryStructureService) buildPlan(ctx context.Context, libraryID uint, workKey string) (StructurePlan, models.MediaLibrary, error) {
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
	plan, err := s.planner.BuildContext(ctx, library, entries, assets, workKey, nil)
	if err != nil || strings.TrimSpace(workKey) != "" {
		return plan, library, err
	}
	plan, err = s.includeHistoricalProviderRootItems(ctx, library, plan)
	return plan, library, err
}

func (s *MediaLibraryStructureService) includeHistoricalProviderRootItems(ctx context.Context, library models.MediaLibrary, plan StructurePlan) (StructurePlan, error) {
	if strings.TrimSpace(library.ProviderRootID) == "" || strings.TrimSpace(library.ProviderRootID) == "0" {
		return plan, nil
	}
	// This is intentionally limited to rows already owned by OhMyCine. The
	// old cid=0 error placed such rows in 115's provider root, outside normal
	// scanning; include them in the existing read-only diagnose/confirmed-repair
	// workflow without adopting any unrelated root files.
	var misplaced []models.MediaManagedItem
	if err := s.db.WithContext(ctx).Where("library_id = ? AND managed = ? AND active = ? AND provider_parent_id = ? AND provider_item_id <> ''", library.ID, true, true, "0").Order("id").Find(&misplaced).Error; err != nil {
		return StructurePlan{}, err
	}
	candidates := make([]structurePlanCandidate, 0, len(plan.Items)+len(misplaced))
	for index, item := range plan.Items {
		candidates = append(candidates, structurePlanCandidate{
			index: index, kind: item.Kind, workKey: item.WorkKey,
			title: item.Title, recognitionID: item.RecognitionID, source: item.SourceRelative, target: item.TargetRelative,
			providerID: item.ProviderID, parentProviderID: item.ParentProviderID, size: item.Size,
		})
	}
	// Movement issues are regenerated below so historical items participate in
	// the same all-members target-conflict isolation as catalog items.
	plan.Items = nil
	plan.IssueCount -= len(candidates)
	if plan.IssueCount < 0 {
		plan.IssueCount = 0
	}
	nonMovementIssues := plan.Issues[:0]
	for _, issue := range plan.Issues {
		if !issue.Repairable {
			nonMovementIssues = append(nonMovementIssues, issue)
		}
	}
	plan.Issues = nonMovementIssues
	allNonMovementIssues := plan.AllIssues[:0]
	for _, issue := range plan.AllIssues {
		if !issue.Repairable {
			allNonMovementIssues = append(allNonMovementIssues, issue)
		}
	}
	plan.AllIssues = allNonMovementIssues
	plan.rebuildIssueSampleCounts()
	for _, managed := range misplaced {
		target := safeStructurePath(managed.RelativePath)
		if target == "" || (managed.Kind != models.MediaManagedItemKindVideo && managed.Kind != models.MediaManagedItemKindSidecar) || managed.Size < 0 {
			continue
		}
		plan.CheckedItems++
		candidates = append(candidates, structurePlanCandidate{
			index: len(candidates), kind: managed.Kind, title: "历史 115 入库文件",
			source: "网盘根目录/" + pathpkg.Base(target), target: target,
			providerID: managed.ProviderItemID, parentProviderID: "0", size: managed.Size,
			allowRootSource: true, moveIssueCode: "cloud_transfer_root_misplaced",
		})
	}
	appendStructureCandidates(&plan, candidates)
	sort.Slice(plan.Items, func(i, j int) bool {
		leftDepth := strings.Count(plan.Items[i].SourceRelative, "/")
		rightDepth := strings.Count(plan.Items[j].SourceRelative, "/")
		if leftDepth != rightDepth {
			return leftDepth > rightDepth
		}
		return plan.Items[i].SourceRelative < plan.Items[j].SourceRelative
	})
	return plan, nil
}

type MediaLibraryStructureDiagnosisWorker struct{ service *MediaLibraryStructureService }

func NewMediaLibraryStructureDiagnosisWorker(service *MediaLibraryStructureService) *MediaLibraryStructureDiagnosisWorker {
	return &MediaLibraryStructureDiagnosisWorker{service: service}
}

func (w *MediaLibraryStructureDiagnosisWorker) Run(ctx context.Context, runtime JobRuntime, job ClaimedJob) WorkerResult {
	if w == nil || w.service == nil {
		return WorkerResult{ErrorCode: CodeMediaLibraryStructureDiagnosisFailed, ErrorMessage: "目录结构诊断服务不可用"}
	}
	var payload mediaLibraryStructureDiagnosisJobPayload
	if json.Unmarshal([]byte(job.Job.PayloadJSON), &payload) != nil || payload.LibraryID == 0 || payload.Generation == 0 {
		return WorkerResult{ErrorCode: CodeMediaLibraryStructureDiagnosisFailed, ErrorMessage: "目录结构诊断任务参数无效"}
	}
	if err := w.service.runDiagnosis(ctx, runtime, job.Job.ID, job.Job.StartedGeneration, payload); err != nil {
		w.service.failDiagnosis(payload, job.Job.ID, job.Job.StartedGeneration)
		return WorkerResult{ErrorCode: CodeMediaLibraryStructureDiagnosisFailed, ErrorMessage: "目录结构诊断系统失败"}
	}
	return WorkerResult{}
}

func (s *MediaLibraryStructureService) runDiagnosis(ctx context.Context, runtime JobRuntime, jobID string, jobGeneration uint64, payload mediaLibraryStructureDiagnosisJobPayload) error {
	started := time.Now().UTC()
	currentJob, err := s.currentStructureDiagnosisJob(ctx, jobID, jobGeneration)
	if err != nil {
		return err
	}
	if !currentJob {
		return nil
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := s.db.WithContext(ctx).First(&diagnosis, "library_id = ?", payload.LibraryID).Error; err != nil {
		return err
	}
	if diagnosis.JobID != jobID || diagnosis.Generation != payload.Generation || optionalUintValue(diagnosis.ScanRunID) != payload.ScanRunID || diagnosis.ScanKind != payload.ScanKind {
		return nil
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, payload.LibraryID).Error; err != nil {
		return err
	}
	if library.BaselineGeneration != payload.Generation {
		return nil
	}
	if payload.ScanRunID != 0 {
		var run models.MediaLibraryScanRun
		if err := s.db.WithContext(ctx).First(&run, payload.ScanRunID).Error; err != nil {
			return err
		}
		if run.LibraryID != payload.LibraryID || run.Generation != payload.Generation || run.Kind != payload.ScanKind || run.CatalogPublishedAt == nil {
			return nil
		}
	}
	begin := s.db.WithContext(ctx).Model(&models.MediaLibraryStructureDiagnosis{}).
		Where("library_id = ? AND job_id = ? AND generation = ? AND scan_kind = ?", payload.LibraryID, jobID, payload.Generation, payload.ScanKind).
		Updates(map[string]any{"status": models.MediaLibraryStructureRunning, "started_at": started, "finished_at": nil, "last_error_code": "", "updated_at": started})
	if begin.Error != nil {
		return begin.Error
	}
	if begin.RowsAffected != 1 {
		return nil
	}
	runningUpdates := map[string]any{"structure_status": models.MediaLibraryStructureRunning, "structure_error_code": ""}
	if err := s.db.WithContext(ctx).Model(&models.MediaLibrary{}).Where("id = ? AND baseline_generation = ?", payload.LibraryID, payload.Generation).
		Updates(runningUpdates).Error; err != nil {
		return err
	}
	if payload.Automatic {
		if err := s.db.WithContext(ctx).Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ? AND source_revision = ?", payload.LibraryID, payload.SourceRevision).Updates(map[string]any{"status": "running", "updated_at": started}).Error; err != nil {
			return err
		}
	}
	structureDiagnosisLogEvent(serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Info()), payload, "running").
		Int("worker_count", StructurePlanningWorkers).Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("开始目录结构诊断"))

	entries, err := s.loadStructureEntries(ctx, payload.LibraryID)
	if err != nil {
		return err
	}
	assets, err := s.loadStructureAssets(ctx, payload.LibraryID)
	if err != nil {
		return err
	}
	total := len(entries) + len(assets)
	if err := s.db.WithContext(ctx).Model(&models.MediaLibraryStructureDiagnosis{}).
		Where("library_id = ? AND job_id = ? AND generation = ?", payload.LibraryID, jobID, payload.Generation).
		Updates(map[string]any{"total_items": total, "processed_items": 0, "updated_at": time.Now().UTC()}).Error; err != nil {
		return err
	}
	planCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	progressStep := max(256, total/20)
	lastPersisted := 0
	var progressErr error
	progress := func(processed, total int) {
		if progressErr != nil || (processed != total && processed-lastPersisted < progressStep) {
			return
		}
		processed64, total64 := int64(processed), int64(total)
		percent := float64(100)
		if total > 0 {
			percent = float64(processed) * 100 / float64(total)
		}
		if err := runtime.Heartbeat(&percent, &processed64, &total64, nil, nil); err != nil {
			progressErr = err
			cancel()
			return
		}
		result := s.db.WithContext(planCtx).Model(&models.MediaLibraryStructureDiagnosis{}).
			Where("library_id = ? AND job_id = ? AND generation = ? AND status = ?", payload.LibraryID, jobID, payload.Generation, models.MediaLibraryStructureRunning).
			Updates(map[string]any{"processed_items": processed, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			progressErr = result.Error
			cancel()
			return
		}
		lastPersisted = processed
		structureDiagnosisLogEvent(serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Info()), payload, "running").
			Int("processed", processed).Int("total", total).Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("目录结构诊断进度"))
	}
	plan, err := s.planner.BuildContext(planCtx, library, entries, assets, "", progress)
	if err != nil {
		if progressErr != nil {
			return progressErr
		}
		return err
	}
	if progressErr != nil {
		return progressErr
	}
	plan, err = s.includeHistoricalProviderRootItems(planCtx, library, plan)
	if err != nil {
		return err
	}
	total = plan.CheckedItems
	var current models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&current, payload.LibraryID).Error; err != nil {
		return err
	}
	if current.BaselineGeneration != payload.Generation || libraryRuleFingerprint(current) != plan.RuleFingerprint {
		return nil
	}
	issuesJSON, err := json.Marshal(plan.Issues)
	if err != nil {
		return err
	}
	status := models.MediaLibraryStructureHealthy
	if plan.IssueCount > 0 {
		status = models.MediaLibraryStructureIssues
	}
	finished := time.Now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := structureDiagnosisJobCurrentTx(tx, jobID, jobGeneration)
		if err != nil {
			return err
		}
		if !current {
			return nil
		}
		updated := tx.Model(&models.MediaLibraryStructureDiagnosis{}).
			Where("library_id = ? AND job_id = ? AND generation = ? AND scan_kind = ?", payload.LibraryID, jobID, payload.Generation, payload.ScanKind).
			Updates(map[string]any{
				"status": status, "processed_items": total, "issue_count": plan.IssueCount, "repairable_count": len(plan.Items),
				"unrecognized_count": plan.Classifications.Unrecognized, "missing_episode_count": plan.Classifications.MissingEpisode,
				"invalid_path_count": plan.Classifications.InvalidPath, "template_error_count": plan.Classifications.TemplateError,
				"duplicate_target_count": plan.Classifications.DuplicateTarget, "sidecar_conflict_count": plan.Classifications.SidecarConflict,
				"issues_json": string(issuesJSON), "last_error_code": "", "finished_at": finished, "updated_at": finished,
			})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return nil
		}
		if err := persistStructureIssuesTx(tx, payload.LibraryID, jobID, payload.Generation, plan, finished); err != nil {
			return err
		}
		libraryUpdates := map[string]any{"structure_status": status, "structure_issue_count": plan.IssueCount, "structure_error_code": "", "structure_checked_at": finished}
		where := tx.Model(&models.MediaLibrary{}).Where("id = ? AND baseline_generation = ?", payload.LibraryID, payload.Generation)
		if payload.Automatic {
			stateUpdate := tx.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ? AND source_revision = ?", payload.LibraryID, payload.SourceRevision).Updates(map[string]any{"diagnosed_revision": payload.SourceRevision, "status": "completed", "updated_at": finished})
			if stateUpdate.Error != nil {
				return stateUpdate.Error
			}
			if stateUpdate.RowsAffected != 1 {
				return nil
			}
		}
		return where.Updates(libraryUpdates).Error
	})
	if err != nil {
		return err
	}
	for _, issue := range plan.Issues {
		structureDiagnosisLogEvent(serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Debug()), payload, "completed").
			Str("issue_code", issue.Code).Str("media_kind", issue.Kind).Str("media_display_name", safeMediaDisplayName(issue.Title)).
			Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("目录结构诊断问题样本"))
	}
	structureDiagnosisLogEvent(serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Info()), payload, "completed").
		Str("status", status).Int("total", total).Int("issue_count", plan.IssueCount).Int("repairable_count", len(plan.Items)).
		Int("unrecognized", plan.Classifications.Unrecognized).Int("missing_season_episode", plan.Classifications.MissingEpisode).
		Int("invalid_path", plan.Classifications.InvalidPath).Int("template_unavailable", plan.Classifications.TemplateError).
		Int("duplicate_target", plan.Classifications.DuplicateTarget).Int("sidecar_target_conflict", plan.Classifications.SidecarConflict).
		Int64("duration_ms", time.Since(started).Milliseconds()).Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("目录结构诊断完成，未移动任何文件"))
	return nil
}

func persistStructureIssuesTx(tx *gorm.DB, libraryID uint, jobID string, generation uint64, plan StructurePlan, now time.Time) error {
	if err := tx.Where("library_id = ?", libraryID).Delete(&models.MediaLibraryStructureIssue{}).Error; err != nil {
		return err
	}
	return insertStructureIssuesTx(tx, libraryID, jobID, generation, plan.AllIssues, now, "")
}

func insertStructureIssuesTx(tx *gorm.DB, libraryID uint, jobID string, generation uint64, issues []StructureIssue, now time.Time, forcedState string) error {
	type pendingGroup struct {
		issue   StructureIssue
		sources []string
	}
	groups := make(map[string]*pendingGroup, len(issues))
	order := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Code == "missing_season_episode" {
			continue
		}
		key := issue.Code + "\x00" + issue.Kind + "\x00" + issue.CurrentPath
		if issue.ConflictSourceCount > 1 {
			key = issue.Code + "\x00" + issue.ExpectedPath
		}
		group := groups[key]
		if group == nil {
			copyIssue := issue
			group = &pendingGroup{issue: copyIssue}
			groups[key] = group
			order = append(order, key)
		} else if group.issue.Kind != issue.Kind {
			group.issue.Kind = "mixed"
		}
		sources := issue.AllConflictSources
		if len(sources) == 0 && issue.CurrentPath != "" {
			sources = []string{issue.CurrentPath}
		}
		for _, source := range sources {
			source = safeStructurePath(source)
			if source == "" {
				continue
			}
			found := false
			for _, existing := range group.sources {
				if existing == source {
					found = true
					break
				}
			}
			if !found {
				group.sources = append(group.sources, source)
			}
		}
	}
	sort.Strings(order)
	for _, key := range order {
		group := groups[key]
		sort.Slice(group.sources, func(i, j int) bool { return strings.ToLower(group.sources[i]) < strings.ToLower(group.sources[j]) })
		state := "needs_attention"
		if group.issue.Repairable {
			state = "pending_repair"
		}
		if group.issue.Code == "media_unrecognized" {
			state = "unrecognized"
		}
		if forcedState != "" {
			state = forcedState
		}
		row := models.MediaLibraryStructureIssue{Token: uuid.NewString(), LibraryID: libraryID, DiagnosisJobID: jobID, Generation: generation, Code: safeLabel(group.issue.Code, 64), Kind: safeLabel(group.issue.Kind, 16), State: state, Repairable: group.issue.Repairable, Title: safeMediaDisplayName(group.issue.Title), CurrentPath: safeStructurePath(group.issue.CurrentPath), ExpectedPath: safeStructurePath(group.issue.ExpectedPath), ConflictSourceCount: len(group.sources), CreatedAt: now, UpdatedAt: now}
		if group.issue.RecognitionID != 0 {
			row.RecognitionID = &group.issue.RecognitionID
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		recommendedIndex := -1
		for index, source := range group.sources {
			base := strings.TrimSuffix(pathpkg.Base(source), pathpkg.Ext(source))
			if !structureDuplicateSuffixPattern.MatchString(strings.TrimSpace(base)) {
				if recommendedIndex != -1 {
					recommendedIndex = -2
					break
				}
				recommendedIndex = index
			}
		}
		for index, source := range group.sources {
			member := models.MediaLibraryStructureIssueMember{IssueID: row.ID, Token: uuid.NewString(), SourcePath: source, Recommended: recommendedIndex == index, CreatedAt: now}
			if err := tx.Create(&member).Error; err != nil {
				return err
			}
			if member.Recommended {
				row.RecommendedMemberToken = member.Token
			}
		}
		if row.RecommendedMemberToken != "" {
			if err := tx.Model(&models.MediaLibraryStructureIssue{}).Where("id = ?", row.ID).Update("recommended_member_token", row.RecommendedMemberToken).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *MediaLibraryStructureService) loadStructureEntries(ctx context.Context, libraryID uint) ([]models.MediaLibraryEntry, error) {
	entries := make([]models.MediaLibraryEntry, 0, 4096)
	lastID := uint(0)
	for {
		batch := make([]models.MediaLibraryEntry, 0, 2000)
		if err := s.db.WithContext(ctx).Where("library_id = ? AND id > ?", libraryID, lastID).Order("id").Limit(2000).Find(&batch).Error; err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return entries, nil
		}
		entries = append(entries, batch...)
		lastID = batch[len(batch)-1].ID
	}
}

func (s *MediaLibraryStructureService) loadStructureAssets(ctx context.Context, libraryID uint) ([]models.MediaLibrarySourceAsset, error) {
	assets := make([]models.MediaLibrarySourceAsset, 0, 4096)
	lastID := uint(0)
	for {
		batch := make([]models.MediaLibrarySourceAsset, 0, 2000)
		if err := s.db.WithContext(ctx).Where("library_id = ? AND active = ? AND id > ?", libraryID, true, lastID).Order("id").Limit(2000).Find(&batch).Error; err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return assets, nil
		}
		assets = append(assets, batch...)
		lastID = batch[len(batch)-1].ID
	}
}

func (s *MediaLibraryStructureService) failDiagnosis(payload mediaLibraryStructureDiagnosisJobPayload, jobID string, jobGeneration uint64) {
	failedAt := time.Now().UTC()
	marked := false
	_ = s.db.Transaction(func(tx *gorm.DB) error {
		current, err := structureDiagnosisJobCurrentTx(tx, jobID, jobGeneration)
		if err != nil || !current {
			return err
		}
		result := tx.Model(&models.MediaLibraryStructureDiagnosis{}).
			Where("library_id = ? AND job_id = ? AND generation = ? AND scan_kind = ?", payload.LibraryID, jobID, payload.Generation, payload.ScanKind).
			Updates(map[string]any{"status": models.MediaLibraryStructureFailed, "last_error_code": CodeMediaLibraryStructureDiagnosisFailed, "finished_at": failedAt, "updated_at": failedAt})
		if result.Error != nil || result.RowsAffected != 1 {
			return result.Error
		}
		updates := map[string]any{"structure_status": models.MediaLibraryStructureFailed, "structure_error_code": CodeMediaLibraryStructureDiagnosisFailed, "structure_checked_at": failedAt}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ? AND baseline_generation = ?", payload.LibraryID, payload.Generation).
			Updates(updates).Error; err != nil {
			return err
		}
		if payload.Automatic {
			if err := tx.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id = ? AND source_revision = ?", payload.LibraryID, payload.SourceRevision).Updates(map[string]any{"status": "failed", "updated_at": failedAt}).Error; err != nil {
				return err
			}
		}
		marked = true
		return nil
	})
	if !marked {
		return
	}
	structureDiagnosisLogEvent(serverlog.OperationMediaLibraryStructureDiagnosis.Event(s.log.Error()), payload, "failed").
		Str("error_code", CodeMediaLibraryStructureDiagnosisFailed).Msg(serverlog.OperationMediaLibraryStructureDiagnosis.Message("目录结构诊断系统失败"))
}

func (s *MediaLibraryStructureService) currentStructureDiagnosisJob(ctx context.Context, jobID string, generation uint64) (bool, error) {
	return structureDiagnosisJobCurrentTx(s.db.WithContext(ctx), jobID, generation)
}

func structureDiagnosisJobCurrentTx(tx *gorm.DB, jobID string, generation uint64) (bool, error) {
	var job models.Job
	if err := tx.Select("id", "generation").First(&job, "id = ?", jobID).Error; err != nil {
		return false, err
	}
	return job.Generation == generation, nil
}

func structureDiagnosisLogEvent(event *zerolog.Event, payload mediaLibraryStructureDiagnosisJobPayload, phase string) *zerolog.Event {
	event = event.Uint("library_id", payload.LibraryID).Uint64("generation", payload.Generation).Str("scan_kind", payload.ScanKind).Str("phase", phase)
	if payload.ScanRunID != 0 {
		event = event.Uint("scan_run_id", payload.ScanRunID)
	}
	return event
}

func optionalUintValue(value *uint) uint {
	if value == nil {
		return 0
	}
	return *value
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
	if plan.SelectionBound {
		var diagnosis models.MediaLibraryStructureDiagnosis
		var autoState models.MediaLibraryStructureAutoState
		if err := s.db.Where("library_id = ?", repair.LibraryID).First(&diagnosis).Error; err != nil || diagnosis.JobID != plan.DiagnosisJobID || diagnosis.Generation != plan.Generation {
			return s.failRepair(repair, CodeMediaLibraryStructureBoundaryChanged, "目录诊断结果已变化，请重新预览")
		}
		if err := s.db.Where("library_id = ?", repair.LibraryID).First(&autoState).Error; err != nil || autoState.SourceRevision != plan.SourceRevision {
			return s.failRepair(repair, CodeMediaLibraryStructureBoundaryChanged, "媒体库来源已变化，请重新预览")
		}
		selectedIssueTokens := append(append([]string(nil), plan.ResolvedIssues...), plan.SkippedIssues...)
		if len(selectedIssueTokens) > 0 {
			var current int64
			if err := s.db.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND token IN ?", repair.LibraryID, plan.DiagnosisJobID, plan.Generation, selectedIssueTokens).Count(&current).Error; err != nil || current != int64(len(selectedIssueTokens)) {
				return s.failRepair(repair, CodeMediaLibraryStructureBoundaryChanged, "目录问题选择已变化，请重新预览")
			}
		}
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
	totalMutations := len(plan.RecycleItems) + len(plan.Items)
	progressAt := func(offset int) StructureProgress {
		return func(processed, _ int) error {
			processed += offset
			processed64, total64 := int64(processed), int64(totalMutations)
			percent := float64(100)
			if totalMutations > 0 {
				percent = float64(processed) * 100 / float64(totalMutations)
			}
			if err := runtime.Heartbeat(&percent, &processed64, &total64, nil, nil); err != nil {
				return err
			}
			return s.db.Model(&repair).Updates(map[string]any{"processed_items": processed, "updated_at": time.Now().UTC()}).Error
		}
	}
	boundary := StructureBoundary{Library: library, Storage: storage}
	if err := backend.Recycle(ctx, boundary, plan.RecycleItems, progressAt(0)); err != nil {
		code := CodeMediaLibraryStructureApplyFailed
		switch {
		case errors.Is(err, errStructureConflict):
			code = CodeMediaLibraryStructureConflict
		case errors.Is(err, errStructureFileLocked):
			code = CodeMediaLibraryStructureFileLocked
		case errors.Is(err, errStructurePermissionDenied):
			code = CodeMediaLibraryStructurePermissionDenied
		}
		return s.failRepair(repair, code, "媒体库冲突来源回收失败")
	}
	if err := backend.Apply(ctx, boundary, plan.Items, progressAt(len(plan.RecycleItems))); err != nil {
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
		if err := removeStructureCatalogItems(tx, repair.LibraryID, plan.RecycleItems); err != nil {
			return err
		}
		if err := updateStructureCatalogPaths(tx, repair.LibraryID, plan.Items, providerParents); err != nil {
			return err
		}
		if len(plan.ResolvedIssues) > 0 {
			if err := tx.Where("library_id = ? AND diagnosis_job_id = ? AND token IN ?", repair.LibraryID, plan.DiagnosisJobID, plan.ResolvedIssues).Delete(&models.MediaLibraryStructureIssue{}).Error; err != nil {
				return err
			}
		}
		if len(plan.SkippedIssues) > 0 {
			if err := tx.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ? AND diagnosis_job_id = ? AND token IN ?", repair.LibraryID, plan.DiagnosisJobID, plan.SkippedIssues).Updates(map[string]any{"state": "skipped", "updated_at": finished}).Error; err != nil {
				return err
			}
		}
		var remaining int64
		if err := tx.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ?", repair.LibraryID).Count(&remaining).Error; err != nil {
			return err
		}
		status := models.MediaLibraryStructureHealthy
		if remaining > 0 {
			status = models.MediaLibraryStructureIssues
		}
		libraryUpdates := map[string]any{"structure_status": status, "structure_error_code": "", "structure_issue_count": remaining, "structure_checked_at": finished}
		if totalMutations > 0 {
			libraryUpdates["dirty_generation"] = gorm.Expr("dirty_generation + 1")
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", repair.LibraryID).Updates(libraryUpdates).Error; err != nil {
			return err
		}
		if err := tx.Model(&repair).Updates(map[string]any{"phase": "completed", "processed_items": totalMutations, "last_error_code": "", "finished_at": finished, "updated_at": finished}).Error; err != nil {
			return err
		}
		return s.audit.Record(tx, &repair.OwnerID, "media_library.structure_repair.complete", "media_library", uintID(repair.LibraryID), "success", map[string]any{"scope": repair.Scope, "move_count": len(plan.Items), "recycle_count": len(plan.RecycleItems)}, RequestContext{})
	})
	if err != nil {
		return s.failRepair(repair, CodeMediaLibraryStructureApplyFailed, "媒体库修复结果保存失败")
	}
	if s.reconcile != nil {
		s.reconcile(repair.LibraryID)
	}
	return WorkerResult{}
}

func removeStructureCatalogItems(tx *gorm.DB, libraryID uint, items []StructureRecycleItem) error {
	for _, item := range items {
		paths := []string{item.SourceRelative, "/" + item.SourceRelative}
		if item.Kind == "sidecar" {
			if err := tx.Where("library_id = ? AND relative_path IN ?", libraryID, paths).Delete(&models.MediaLibrarySourceAsset{}).Error; err != nil {
				return err
			}
		} else if err := tx.Where("library_id = ? AND relative_path IN ?", libraryID, paths).Delete(&models.MediaLibraryEntry{}).Error; err != nil {
			return err
		}
		if item.ProviderID != "" {
			if err := tx.Model(&models.MediaManagedItem{}).Where("library_id = ? AND provider_item_id = ?", libraryID, item.ProviderID).Updates(map[string]any{"active": false, "updated_at": time.Now().UTC()}).Error; err != nil {
				return err
			}
		}
	}
	return nil
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

func (localMediaLibraryStructureBackend) ValidateRecycle(_ context.Context, boundary StructureBoundary) error {
	_, err := medialibrary.ResolveRoot(boundary.Storage.RootPath, boundary.Library.RelativeRoot)
	return err
}

func (localMediaLibraryStructureBackend) Recycle(ctx context.Context, boundary StructureBoundary, items []StructureRecycleItem, progress StructureProgress) error {
	if len(items) == 0 {
		return nil
	}
	root, err := medialibrary.ResolveRoot(boundary.Storage.RootPath, boundary.Library.RelativeRoot)
	if err != nil {
		return err
	}
	oldDirectories := make(map[string]struct{}, len(items))
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		sourceRelative := safeStructurePath(item.SourceRelative)
		recycleRelative := safeStructurePath(item.RecycleRelative)
		if sourceRelative == "" || recycleRelative == "" || !strings.HasPrefix(strings.ToLower(recycleRelative), ".ohmycine-recycle/") {
			return errors.New("invalid local recycle target")
		}
		source := filepath.Join(root, filepath.FromSlash(sourceRelative))
		target := filepath.Join(root, filepath.FromSlash(recycleRelative))
		if ensureWithin(root, source) != nil || ensureWithin(root, target) != nil {
			return errors.New("recycle path escapes library root")
		}
		if err := ensureSafeDirectoryPath(root, filepath.Dir(source), false); err != nil {
			return errors.New("local recycle source directory is unsafe")
		}
		oldDirectories[filepath.Dir(source)] = struct{}{}
		sourceInfo, sourceErr := os.Lstat(source)
		targetInfo, targetErr := os.Lstat(target)
		if targetErr == nil {
			if err := ensureSafeDirectoryPath(root, filepath.Dir(target), false); err != nil {
				return errors.New("local recycle target directory is unsafe")
			}
		}
		if errors.Is(sourceErr, os.ErrNotExist) && targetErr == nil && targetInfo.Mode().IsRegular() && (item.Size <= 0 || targetInfo.Size() == item.Size) && (item.ModifiedAtUnixNano == 0 || targetInfo.ModTime().UTC().UnixNano() == item.ModifiedAtUnixNano) {
			if progress != nil {
				if err := progress(index+1, len(items)); err != nil {
					return err
				}
			}
			continue
		}
		if sourceErr != nil || !sourceInfo.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(source, fs.FileInfoToDirEntry(sourceInfo)) || (item.Size > 0 && sourceInfo.Size() != item.Size) || (item.ModifiedAtUnixNano != 0 && sourceInfo.ModTime().UTC().UnixNano() != item.ModifiedAtUnixNano) {
			return errors.New("local recycle source identity changed")
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
		if err := ensureSafeDirectoryPath(root, filepath.Dir(source), false); err != nil {
			return errors.New("structure source directory is unsafe")
		}
		oldDirectories[filepath.Dir(source)] = struct{}{}
		sourceInfo, sourceErr := os.Lstat(source)
		targetInfo, targetErr := os.Lstat(target)
		if targetErr == nil {
			if err := ensureSafeDirectoryPath(root, filepath.Dir(target), false); err != nil {
				return errors.New("structure target directory is unsafe")
			}
		}
		if errors.Is(sourceErr, os.ErrNotExist) && targetErr == nil && targetInfo.Mode().IsRegular() && (item.Size <= 0 || targetInfo.Size() == item.Size) && (item.ModifiedAtUnixNano == 0 || targetInfo.ModTime().UTC().UnixNano() == item.ModifiedAtUnixNano) {
			if progress != nil {
				if err := progress(index+1, len(items)); err != nil {
					return err
				}
			}
			continue
		}
		if sourceErr != nil || !sourceInfo.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(source, fs.FileInfoToDirEntry(sourceInfo)) || (item.Size > 0 && sourceInfo.Size() != item.Size) || (item.ModifiedAtUnixNano != 0 && sourceInfo.ModTime().UTC().UnixNano() != item.ModifiedAtUnixNano) {
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

func (b pan115MediaLibraryStructureBackend) ValidateRecycle(_ context.Context, boundary StructureBoundary) error {
	if boundary.Storage.ConnectionID == nil || b.driver == nil {
		return errors.New("provider connection is unavailable")
	}
	driver, err := b.driver(*boundary.Storage.ConnectionID)
	if err != nil {
		return err
	}
	mutations, ok := driver.(cloudpkg.MutationDriver)
	if !ok || !mutations.Capabilities().Recycle {
		return errors.New("provider recoverable recycle is unavailable")
	}
	return nil
}

func (b pan115MediaLibraryStructureBackend) Recycle(ctx context.Context, boundary StructureBoundary, items []StructureRecycleItem, progress StructureProgress) error {
	if len(items) == 0 {
		return nil
	}
	if err := b.ValidateRecycle(ctx, boundary); err != nil {
		return err
	}
	driver, err := b.driver(*boundary.Storage.ConnectionID)
	if err != nil {
		return err
	}
	mutations, ok := driver.(cloudpkg.MutationDriver)
	if !ok || !mutations.Capabilities().Recycle {
		return errors.New("provider recoverable recycle is unavailable")
	}
	rootID := boundary.Library.ProviderRootID
	if rootID == "" {
		rootID = boundary.Storage.RootPath
	}
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.TrimSpace(item.ProviderID) == "" {
			return errors.New("provider recycle identity is missing")
		}
		stat, err := driver.Stat(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), item.ProviderID)
		if err != nil {
			if code, _ := cloudpkg.ErrorInfo(err); code == cloudpkg.CodeNotFound {
				if progress != nil {
					if err := progress(index+1, len(items)); err != nil {
						return err
					}
				}
				continue
			}
			return err
		}
		within, err := providerParentWithinRoot(ctx, driver, stat.ParentID, rootID)
		if err != nil || !within || stat.IsDir || stat.Name != pathpkg.Base(item.SourceRelative) || (item.Size > 0 && stat.Size != item.Size) {
			return errors.New("provider recycle source identity changed")
		}
		if err := mutations.Recycle(ctx, item.ProviderID); err != nil {
			return err
		}
		if progress != nil {
			if err := progress(index+1, len(items)); err != nil {
				return err
			}
		}
	}
	return nil
}

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
		item, err := driver.Stat(ctx, current)
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
