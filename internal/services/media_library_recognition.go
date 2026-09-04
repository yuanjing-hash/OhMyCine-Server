package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

var mediaRecognitionGlobalGate = make(chan struct{}, 8)

type mediaLibraryRecognizedUnit struct {
	Unit     medialibrary.RecognitionUnit
	Result   MediaRecognitionResult
	CacheHit bool
	Manual   bool
}

// stabilizeExistingRecognitionUnits preserves a work's durable identity when
// grouping rules evolve or a provider adds/renames an episode. A legacy source
// key is inherited only when every matching existing file points to exactly
// one recognition and that recognition is claimed by exactly one new unit.
func (s *MediaLibraryService) stabilizeExistingRecognitionUnits(ctx context.Context, libraryID uint, units []medialibrary.RecognitionUnit) ([]medialibrary.RecognitionUnit, error) {
	if len(units) == 0 {
		return units, nil
	}
	var entries []models.MediaLibraryEntry
	if err := s.db.WithContext(ctx).Select("relative_path", "provider_id", "recognition_id").Where("library_id = ? AND recognition_id IS NOT NULL", libraryID).Find(&entries).Error; err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return units, nil
	}
	byPath := make(map[string]uint, len(entries))
	byProvider := make(map[string]uint, len(entries))
	recognitionIDs := make([]uint, 0, len(entries))
	seenRecognitionIDs := make(map[uint]struct{}, len(entries))
	for _, entry := range entries {
		if entry.RecognitionID == nil {
			continue
		}
		byPath[entry.RelativePath] = *entry.RecognitionID
		if entry.ProviderID != "" {
			byProvider[entry.ProviderID] = *entry.RecognitionID
		}
		if _, seen := seenRecognitionIDs[*entry.RecognitionID]; !seen {
			seenRecognitionIDs[*entry.RecognitionID] = struct{}{}
			recognitionIDs = append(recognitionIDs, *entry.RecognitionID)
		}
	}
	var records []models.MediaLibraryRecognition
	if err := s.db.WithContext(ctx).Where("library_id = ? AND id IN ?", libraryID, recognitionIDs).Find(&records).Error; err != nil {
		return nil, err
	}
	byID := make(map[uint]models.MediaLibraryRecognition, len(records))
	for _, record := range records {
		byID[record.ID] = record
	}
	candidates := make([]uint, len(units))
	claims := make(map[uint]int)
	for index := range units {
		ids := make(map[uint]struct{})
		for _, file := range units[index].Files {
			if id := byPath[file.RelativePath]; id != 0 {
				ids[id] = struct{}{}
			} else if file.ProviderID != "" {
				if id := byProvider[file.ProviderID]; id != 0 {
					ids[id] = struct{}{}
				}
			}
		}
		if len(ids) == 1 {
			for id := range ids {
				if _, exists := byID[id]; exists {
					candidates[index] = id
					claims[id]++
				}
			}
		}
	}
	result := append([]medialibrary.RecognitionUnit(nil), units...)
	for index, id := range candidates {
		if id == 0 || claims[id] != 1 {
			continue
		}
		result[index].SourceKey = byID[id].SourceKey
	}
	return result, nil
}

type recognitionMetadataEnvelope struct {
	Version        int                     `json:"version"`
	EngineVersion  string                  `json:"engine_version,omitempty"`
	Classification classification.Metadata `json:"classification"`
	Snapshot       tmdb.Snapshot           `json:"snapshot,omitempty"`
}

type mediaRecognitionRateGate struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func (g *mediaRecognitionRateGate) Wait(ctx context.Context) error {
	if g == nil || g.interval <= 0 {
		return nil
	}
	g.mu.Lock()
	now := time.Now()
	reserved := g.next
	if reserved.Before(now) {
		reserved = now
	}
	g.next = reserved.Add(g.interval)
	g.mu.Unlock()
	if wait := time.Until(reserved); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (s *MediaLibraryService) recognizeLibraryUnits(ctx context.Context, library models.MediaLibrary, profile models.MediaClassificationProfile, units []medialibrary.RecognitionUnit) ([]mediaLibraryRecognizedUnit, error) {
	rules, err := classification.DecodeStrict([]byte(profile.RulesJSON))
	if err != nil {
		return nil, err
	}
	organization, err := storedProfileOrganizationConfig(profile)
	if err != nil {
		return nil, err
	}
	processor, err := builtinProcessorForCodes(organization.BuiltinRecognitionPacks)
	if err != nil {
		return nil, err
	}
	var lookup mediaRecognitionLookup
	if s.metadata != nil {
		client, _, _, clientErr := s.metadata.clientWithCredentialInfo()
		if clientErr == nil {
			lookup = client
		}
	}
	var existing []models.MediaLibraryRecognition
	if err := s.db.WithContext(ctx).Where("library_id = ?", library.ID).Find(&existing).Error; err != nil {
		return nil, err
	}
	bySource := make(map[string]models.MediaLibraryRecognition, len(existing))
	for _, record := range existing {
		bySource[record.SourceKey] = record
	}

	results := make([]mediaLibraryRecognizedUnit, len(units))
	interval := time.Duration(0)
	if library.MetadataRatePerSecond > 0 {
		interval = time.Second / time.Duration(library.MetadataRatePerSecond)
	}
	rateGate := &mediaRecognitionRateGate{interval: interval}
	workerCount := library.MetadataConcurrency
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > len(units) {
		workerCount = len(units)
	}
	if workerCount == 0 {
		return results, nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	errorsCh := make(chan error, 1)
	var workers sync.WaitGroup
	process := func(index int) error {
		unit := units[index]
		if record, ok := bySource[unit.SourceKey]; ok && record.ManualOverride {
			result, decodeErr := recognitionResultFromStored(record, rules)
			if decodeErr != nil {
				return decodeErr
			}
			results[index] = mediaLibraryRecognizedUnit{Unit: unit, Result: result, Manual: true}
			return nil
		}
		cacheKey := mediaLibraryRecognitionCacheKey(unit, profile, library)
		if cached, ok := s.loadRecognitionCache(workerCtx, cacheKey); ok {
			results[index] = mediaLibraryRecognizedUnit{Unit: unit, Result: cached, CacheHit: true}
			return nil
		}
		if lookup != nil {
			if err := rateGate.Wait(workerCtx); err != nil {
				return err
			}
		}
		select {
		case mediaRecognitionGlobalGate <- struct{}{}:
		case <-workerCtx.Done():
			return workerCtx.Err()
		}
		evidenceFiles := unit.EvidenceFiles
		if len(evidenceFiles) == 0 {
			evidenceFiles = medialibrary.RecognitionEvidenceFiles(unit.Files)
		}
		files := make([]recognitionSourceFile, 0, len(evidenceFiles))
		for _, file := range evidenceFiles {
			files = append(files, recognitionSourceFile{RelativePath: file.RelativePath, Size: file.Size})
		}
		result := recognizeMedia(workerCtx, lookup, MediaRecognitionRequest{
			PackageName:      unit.PackageName,
			Files:            files,
			SourceKind:       mediarecognition.SourceLibraryScan,
			MediaTypeHint:    unit.MediaTypeHint,
			BuiltinPackCodes: organization.BuiltinRecognitionPacks,
			BuiltinProcessor: processor,
			RecognitionRules: organization.RecognitionRules,
			Classification:   rules,
			Language:         library.MetadataLanguage,
			Region:           library.MetadataRegion,
			AIAssist:         s.aiRecognition,
		})
		<-mediaRecognitionGlobalGate
		_ = s.storeRecognitionCache(workerCtx, cacheKey, result)
		results[index] = mediaLibraryRecognizedUnit{Unit: unit, Result: result}
		return nil
	}
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if err := process(index); err != nil {
					select {
					case errorsCh <- err:
						cancel()
					default:
					}
					return
				}
			}
		}()
	}
dispatch:
	for index := range units {
		select {
		case jobs <- index:
		case <-workerCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-errorsCh:
		return nil, err
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func recognitionResultFromStored(record models.MediaLibraryRecognition, rules classification.RulesV1) (MediaRecognitionResult, error) {
	result := MediaRecognitionResult{
		Status:        record.Status,
		ErrorCode:     record.ErrorCode,
		Title:         record.Title,
		MediaType:     record.MediaType,
		CategoryName:  record.CategoryName,
		MatchedRuleID: record.MatchedRuleID,
		TMDBID:        cloneInt64(record.TMDBID),
		ReleaseYear:   cloneInt(record.ReleaseYear),
		Confidence:    cloneFloat64(record.Confidence),
	}
	if record.MetadataJSON != "" && record.MetadataJSON != "{}" {
		metadata, snapshot, err := decodeRecognitionMetadata(record.MetadataJSON)
		if err != nil {
			return MediaRecognitionResult{}, fmt.Errorf("decode stored recognition metadata: %w", err)
		}
		result.Metadata, result.Snapshot = metadata, snapshot
		classified := classification.Classify(result.Metadata, rules)
		result.CategoryName, result.MatchedRuleID = classified.CategoryName, classified.MatchedRuleID
	}
	return result, nil
}

func marshalRecognitionMetadata(result MediaRecognitionResult) (string, error) {
	payload, err := json.Marshal(recognitionMetadataEnvelope{Version: 1, EngineVersion: mediarecognition.EngineVersion, Classification: result.Metadata, Snapshot: result.Snapshot})
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

// mediaLibraryRecognitionProjectionFresh prevents the fast catalog path from
// bypassing the versioned recognition cache. Legacy automatic rows and rows
// written by an older ranking contract are recomputed, while explicit manual
// overrides remain authoritative across engine upgrades.
func mediaLibraryRecognitionProjectionFresh(record models.MediaLibraryRecognition) bool {
	if record.ManualOverride {
		return true
	}
	if record.MetadataJSON == "" || record.MetadataJSON == "{}" {
		return false
	}
	var envelope recognitionMetadataEnvelope
	if err := json.Unmarshal([]byte(record.MetadataJSON), &envelope); err != nil {
		return false
	}
	return envelope.Version == 1 && envelope.EngineVersion == mediarecognition.EngineVersion
}

func decodeRecognitionMetadata(raw string) (classification.Metadata, tmdb.Snapshot, error) {
	var envelope recognitionMetadataEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return classification.Metadata{}, tmdb.Snapshot{}, err
	}
	if envelope.Version == 1 {
		return envelope.Classification, envelope.Snapshot, nil
	}
	// v25-v26 stored classification.Metadata directly. Keep those records
	// readable until the next successful recognition refresh replaces them.
	var legacy classification.Metadata
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return classification.Metadata{}, tmdb.Snapshot{}, err
	}
	return legacy, tmdb.Snapshot{}, nil
}

func mediaLibraryRecognitionCacheKey(unit medialibrary.RecognitionUnit, profile models.MediaClassificationProfile, library models.MediaLibrary) string {
	// v2 refreshes older derived cache entries so the additive v1 snapshot
	// receives the richer bounded detail fields without breaking persisted v1
	// envelopes or requiring a database migration.
	value := "recognition:" + mediarecognition.EngineVersion + ":snapshot-v3\x00" + unit.InputFingerprint + "\x00" + strconv.FormatUint(uint64(profile.ID), 10) + "\x00" + strconv.FormatUint(profile.Revision, 10) + "\x00" + library.MetadataLanguage + "\x00" + library.MetadataRegion
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *MediaLibraryService) loadRecognitionCache(ctx context.Context, key string) (MediaRecognitionResult, bool) {
	var cached models.MediaRecognitionCache
	if err := s.db.WithContext(ctx).Where("lookup_key = ? AND expires_at > ?", key, time.Now().UTC()).First(&cached).Error; err != nil {
		return MediaRecognitionResult{}, false
	}
	var result MediaRecognitionResult
	if err := json.Unmarshal([]byte(cached.ResultJSON), &result); err != nil {
		return MediaRecognitionResult{}, false
	}
	return result, true
}

func (s *MediaLibraryService) storeRecognitionCache(ctx context.Context, key string, result MediaRecognitionResult) error {
	// Credential/auth failures are configuration state, not durable lookup
	// answers. Avoid retaining them beyond the current scan.
	if result.ErrorCode == mediaRecognitionCredentialMissing || result.ErrorCode == "tmdb_auth_failed" {
		return nil
	}
	ttl := 30 * time.Minute
	if result.Status == mediaRecognitionStatusMatched {
		ttl = 30 * 24 * time.Hour
	} else if result.ErrorCode == "tmdb_network_unavailable" || result.ErrorCode == "tmdb_request_failed" {
		ttl = 5 * time.Minute
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := models.MediaRecognitionCache{LookupKey: key, Status: result.Status, ErrorCode: result.ErrorCode, ResultJSON: string(payload), ExpiresAt: now.Add(ttl), CreatedAt: now, UpdatedAt: now}
	return s.db.WithContext(ctx).Save(&record).Error
}

func recognitionWorkKey(result MediaRecognitionResult, sourceKey string) string {
	if result.Status == mediaRecognitionStatusMatched && result.TMDBID != nil {
		if result.MediaType == "tv" {
			return "series:tmdb:" + strconv.FormatInt(*result.TMDBID, 10)
		}
		return "movie:tmdb:" + strconv.FormatInt(*result.TMDBID, 10)
	}
	return "file:" + sourceKey
}
