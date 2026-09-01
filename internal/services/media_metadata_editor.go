package services

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MediaMetadataEditable struct {
	Title               string         `json:"title"`
	OriginalTitle       string         `json:"original_title"`
	ReleaseDate         string         `json:"release_date"`
	Overview            string         `json:"overview"`
	Tagline             string         `json:"tagline"`
	Status              string         `json:"status"`
	VoteAverage         float64        `json:"vote_average"`
	VoteCount           int            `json:"vote_count"`
	RuntimeMinutes      int            `json:"runtime_minutes"`
	SeasonCount         int            `json:"season_count"`
	EpisodeCount        int            `json:"episode_count"`
	Genres              []tmdb.Genre   `json:"genres"`
	ProductionCountries []string       `json:"production_countries"`
	OriginCountries     []string       `json:"origin_countries"`
	OriginalLanguage    string         `json:"original_language"`
	SpokenLanguages     []string       `json:"spoken_languages"`
	Studios             []tmdb.Company `json:"studios"`
	Directors           []tmdb.Person  `json:"directors"`
	Writers             []tmdb.Person  `json:"writers"`
	Cast                []tmdb.Person  `json:"cast"`
	PosterPath          string         `json:"poster_path"`
	BackdropPath        string         `json:"backdrop_path"`
}

type MediaMetadataImageOption struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

type MediaMetadataDocument struct {
	LibraryID       uint                       `json:"library_id"`
	WorkID          string                     `json:"work_id"`
	Revision        int64                      `json:"revision"`
	TMDBID          int64                      `json:"tmdb_id"`
	MediaType       string                     `json:"media_type"`
	ManualOverride  bool                       `json:"manual_override"`
	Editable        MediaMetadataEditable      `json:"editable"`
	PosterOptions   []MediaMetadataImageOption `json:"poster_options"`
	BackdropOptions []MediaMetadataImageOption `json:"backdrop_options"`
}

type MediaMetadataUpdateInput struct {
	Revision int64                 `json:"revision"`
	Editable MediaMetadataEditable `json:"editable"`
}

// MediaMetadataEditor is a pure policy object. It cannot alter TMDB identity,
// media type, provider paths or files; it only validates an editable snapshot.
type MediaMetadataEditor struct{}

func (MediaMetadataEditor) Apply(current tmdb.Snapshot, input MediaMetadataEditable) (tmdb.Snapshot, error) {
	if current.TMDBID <= 0 || (current.MediaType != "movie" && current.MediaType != "tv") {
		return tmdb.Snapshot{}, errors.New("metadata identity is invalid")
	}
	var err error
	input.Title, err = boundedRequiredText(input.Title, 512)
	if err != nil {
		return tmdb.Snapshot{}, err
	}
	for value, limit := range map[*string]int{&input.OriginalTitle: 512, &input.Overview: 32768, &input.Tagline: 2048, &input.Status: 128} {
		*value, err = boundedOptionalText(*value, limit)
		if err != nil {
			return tmdb.Snapshot{}, err
		}
	}
	input.ReleaseDate = strings.TrimSpace(input.ReleaseDate)
	if input.ReleaseDate != "" {
		parsed, parseErr := time.Parse("2006-01-02", input.ReleaseDate)
		if parseErr != nil || parsed.Year() < 1888 || parsed.Year() > 2200 {
			return tmdb.Snapshot{}, errors.New("release date is invalid")
		}
	}
	if input.VoteAverage < 0 || input.VoteAverage > 10 || input.VoteCount < 0 || input.RuntimeMinutes < 0 || input.RuntimeMinutes > 100000 || input.SeasonCount < 0 || input.SeasonCount > 10000 || input.EpisodeCount < 0 || input.EpisodeCount > 1000000 {
		return tmdb.Snapshot{}, errors.New("numeric metadata is out of range")
	}
	input.OriginalLanguage, err = boundedCode(input.OriginalLanguage)
	if err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.ProductionCountries, err = normalizeMetadataStrings(input.ProductionCountries, 64, 32, true); err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.OriginCountries, err = normalizeMetadataStrings(input.OriginCountries, 64, 32, true); err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.SpokenLanguages, err = normalizeMetadataStrings(input.SpokenLanguages, 64, 32, true); err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.Genres, err = normalizeMetadataGenres(input.Genres); err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.Studios, err = normalizeMetadataCompanies(input.Studios); err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.Directors, err = normalizeMetadataPeople(input.Directors, 128); err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.Writers, err = normalizeMetadataPeople(input.Writers, 128); err != nil {
		return tmdb.Snapshot{}, err
	}
	if input.Cast, err = normalizeMetadataPeople(input.Cast, 256); err != nil {
		return tmdb.Snapshot{}, err
	}
	if !metadataImageAllowed(input.PosterPath, append([]string{current.PosterPath}, current.PosterPaths...)) || !metadataImageAllowed(input.BackdropPath, append([]string{current.BackdropPath}, current.BackdropPaths...)) {
		return tmdb.Snapshot{}, errors.New("image identity is not in the verified snapshot")
	}
	current.Title, current.OriginalTitle, current.ReleaseDate = input.Title, input.OriginalTitle, input.ReleaseDate
	current.Overview, current.Tagline, current.Status = input.Overview, input.Tagline, input.Status
	current.VoteAverage, current.VoteCount, current.RuntimeMinutes = input.VoteAverage, input.VoteCount, input.RuntimeMinutes
	current.SeasonCount, current.EpisodeCount = input.SeasonCount, input.EpisodeCount
	current.Genres, current.ProductionCountries, current.OriginCountries = input.Genres, input.ProductionCountries, input.OriginCountries
	current.OriginalLanguage, current.SpokenLanguages, current.Studios = input.OriginalLanguage, input.SpokenLanguages, input.Studios
	current.Directors, current.Writers, current.Cast = input.Directors, input.Writers, input.Cast
	current.PosterPath, current.BackdropPath = input.PosterPath, input.BackdropPath
	return current, nil
}

func boundedRequiredText(value string, limit int) (string, error) {
	value, err := boundedOptionalText(value, limit)
	if err != nil || value == "" {
		return "", errors.New("title is required")
	}
	return value, nil
}

func boundedOptionalText(value string, limit int) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > limit {
		return "", errors.New("metadata text is too long")
	}
	return value, nil
}

func boundedCode(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if utf8.RuneCountInString(value) > 16 {
		return "", errors.New("metadata code is too long")
	}
	return value, nil
}

func normalizeMetadataStrings(values []string, maxItems, maxLength int, lower bool) ([]string, error) {
	if len(values) > maxItems {
		return nil, errors.New("metadata list is too large")
	}
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if lower {
			value = strings.ToLower(value)
		}
		if value == "" || utf8.RuneCountInString(value) > maxLength {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func normalizeMetadataGenres(values []tmdb.Genre) ([]tmdb.Genre, error) {
	if len(values) > 64 {
		return nil, errors.New("genre list is too large")
	}
	result := make([]tmdb.Genre, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		name, err := boundedOptionalText(value.Name, 128)
		if err != nil {
			return nil, err
		}
		if name == "" && value.ID <= 0 {
			continue
		}
		key := strings.ToLower(name) + ":" + strconv.Itoa(value.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, tmdb.Genre{ID: max(0, value.ID), Name: name})
	}
	return result, nil
}

func normalizeMetadataCompanies(values []tmdb.Company) ([]tmdb.Company, error) {
	if len(values) > 128 {
		return nil, errors.New("studio list is too large")
	}
	result := make([]tmdb.Company, 0, len(values))
	for _, value := range values {
		name, err := boundedOptionalText(value.Name, 256)
		if err != nil {
			return nil, err
		}
		if name != "" {
			result = append(result, tmdb.Company{TMDBID: max(int64(0), value.TMDBID), Name: name})
		}
	}
	return result, nil
}

func normalizeMetadataPeople(values []tmdb.Person, maxItems int) ([]tmdb.Person, error) {
	if len(values) > maxItems {
		return nil, errors.New("people list is too large")
	}
	result := make([]tmdb.Person, 0, len(values))
	for _, value := range values {
		name, err := boundedOptionalText(value.Name, 256)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}
		character, err := boundedOptionalText(value.Character, 256)
		if err != nil {
			return nil, err
		}
		job, err := boundedOptionalText(value.Job, 128)
		if err != nil {
			return nil, err
		}
		result = append(result, tmdb.Person{TMDBID: max(int64(0), value.TMDBID), Name: name, Character: character, Job: job, ProfilePath: safeTMDBImagePath(value.ProfilePath)})
	}
	return result, nil
}

func metadataImageAllowed(value string, options []string) bool {
	raw := strings.TrimSpace(value)
	value = safeTMDBImagePath(raw)
	if raw == "" {
		return true
	}
	if value == "" {
		return false
	}
	for _, option := range options {
		if value == safeTMDBImagePath(option) {
			return true
		}
	}
	return false
}

func editableFromSnapshot(snapshot tmdb.Snapshot) MediaMetadataEditable {
	return MediaMetadataEditable{Title: snapshot.Title, OriginalTitle: snapshot.OriginalTitle, ReleaseDate: snapshot.ReleaseDate, Overview: snapshot.Overview, Tagline: snapshot.Tagline, Status: snapshot.Status, VoteAverage: snapshot.VoteAverage, VoteCount: snapshot.VoteCount, RuntimeMinutes: snapshot.RuntimeMinutes, SeasonCount: snapshot.SeasonCount, EpisodeCount: snapshot.EpisodeCount, Genres: snapshot.Genres, ProductionCountries: snapshot.ProductionCountries, OriginCountries: snapshot.OriginCountries, OriginalLanguage: snapshot.OriginalLanguage, SpokenLanguages: snapshot.SpokenLanguages, Studios: snapshot.Studios, Directors: snapshot.Directors, Writers: snapshot.Writers, Cast: snapshot.Cast, PosterPath: snapshot.PosterPath, BackdropPath: snapshot.BackdropPath}
}

func (s *MediaLibraryService) CatalogMetadata(ctx context.Context, actor Actor, libraryID uint, workToken string) (MediaMetadataDocument, error) {
	if err := s.ensureMediaLibraryReadable(actor, libraryID); err != nil {
		return MediaMetadataDocument{}, err
	}
	records, err := s.catalogMetadataRecognitions(libraryID, workToken)
	if err != nil {
		return MediaMetadataDocument{}, err
	}
	record := records[len(records)-1]
	_, snapshot, err := decodeRecognitionMetadata(record.MetadataJSON)
	if err != nil || snapshot.TMDBID <= 0 {
		return MediaMetadataDocument{}, appError(CodeConflict, "当前作品没有可编辑的元数据快照", err)
	}
	snapshot = s.enrichMetadataImageOptions(ctx, libraryID, snapshot)
	return s.metadataDocument(libraryID, workToken, record, snapshot), nil
}

func (s *MediaLibraryService) UpdateCatalogMetadata(ctx context.Context, actor Actor, libraryID uint, workToken string, input MediaMetadataUpdateInput, request RequestContext) (MediaMetadataDocument, error) {
	if !actor.Can(authz.PermissionMediaLibrariesScan) {
		return MediaMetadataDocument{}, appError(CodePermissionDenied, "无权编辑媒体库元数据", nil)
	}
	records, err := s.catalogMetadataRecognitions(libraryID, workToken)
	if err != nil {
		return MediaMetadataDocument{}, err
	}
	latest := records[len(records)-1]
	if input.Revision <= 0 || latest.UpdatedAt.UnixNano() != input.Revision {
		return MediaMetadataDocument{}, appError(CodeConflict, "元数据已变化，请重新加载后再保存", nil)
	}
	_, imageAuthority, _ := decodeRecognitionMetadata(latest.MetadataJSON)
	imageAuthority = s.enrichMetadataImageOptions(ctx, libraryID, imageAuthority)
	var saved tmdb.Snapshot
	updates := make([]catalogMetadataResult, 0, len(records))
	for _, record := range records {
		var profile models.MediaClassificationProfile
		if err := s.db.First(&profile, record.ProfileID).Error; err != nil {
			return MediaMetadataDocument{}, err
		}
		rules, err := classification.DecodeStrict([]byte(profile.RulesJSON))
		if err != nil {
			return MediaMetadataDocument{}, err
		}
		result, err := recognitionResultFromStored(record, rules)
		if err != nil {
			return MediaMetadataDocument{}, err
		}
		result.Snapshot.PosterPaths = append([]string(nil), imageAuthority.PosterPaths...)
		result.Snapshot.BackdropPaths = append([]string(nil), imageAuthority.BackdropPaths...)
		edited, err := (MediaMetadataEditor{}).Apply(result.Snapshot, input.Editable)
		if err != nil {
			return MediaMetadataDocument{}, appError(CodeInvalidRequest, "元数据字段无效", err)
		}
		result.Snapshot, result.Title = edited, edited.Title
		result.ReleaseYear = nil
		if len(edited.ReleaseDate) >= 4 {
			year, parseErr := time.Parse("2006-01-02", edited.ReleaseDate)
			if parseErr == nil {
				value := year.Year()
				result.ReleaseYear = &value
			}
		}
		genreIDs := make([]int, 0, len(edited.Genres))
		for _, genre := range edited.Genres {
			if genre.ID > 0 {
				genreIDs = append(genreIDs, genre.ID)
			}
		}
		result.Metadata = classification.Metadata{MediaType: classification.MediaType(edited.MediaType), GenreIDs: genreIDs, OriginalLanguage: edited.OriginalLanguage, ProductionCountries: edited.ProductionCountries, OriginCountries: edited.OriginCountries, ReleaseYear: result.ReleaseYear}
		classified := classification.Classify(result.Metadata, rules)
		result.CategoryName, result.MatchedRuleID = classified.CategoryName, classified.MatchedRuleID
		updates = append(updates, catalogMetadataResult{Record: record, Profile: profile, Result: result})
		saved = edited
	}
	if err := s.persistCatalogMetadataResults(updates); err != nil {
		return MediaMetadataDocument{}, err
	}
	_ = s.audit.Record(s.db, &actor.User.ID, "media_library.metadata.update", "media_library", uintID(libraryID), "success", map[string]any{"recognition_count": len(records)}, request)
	if err := s.db.First(&latest, latest.ID).Error; err != nil {
		return MediaMetadataDocument{}, err
	}
	return s.metadataDocument(libraryID, workToken, latest, saved), nil
}

type catalogMetadataResult struct {
	Record  models.MediaLibraryRecognition
	Profile models.MediaClassificationProfile
	Result  MediaRecognitionResult
}

func (s *MediaLibraryService) persistCatalogMetadataResults(updates []catalogMetadataResult) error {
	if len(updates) == 0 {
		return appError(CodeInvalidRequest, "元数据更新为空", nil)
	}
	libraryID := updates[0].Record.LibraryID
	lock := s.scanLock(libraryID)
	lock.Lock()
	defer lock.Unlock()
	now := time.Now().UTC()
	var committedChange models.MediaLibraryChange
	var artifactGeneration uint64
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var library models.MediaLibrary
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&library, libraryID).Error; err != nil {
			return err
		}
		var storage models.Storage
		if err := tx.First(&storage, library.StorageID).Error; err != nil {
			return err
		}
		generation := library.DirtyGeneration
		for _, update := range updates {
			if update.Record.LibraryID != libraryID || update.Profile.ID != library.ProfileID || update.Profile.Revision != library.ProfileRevision {
				return appError(CodeConflict, "媒体库识别配置已变化，请重试", nil)
			}
			generation = max(generation, update.Record.LastGeneration)
		}
		requiresArtifacts := mediaLibraryRequiresArtifacts(storage.Type, library, s.artifacts != nil)
		if requiresArtifacts {
			artifactGeneration = max(generation, library.ArtifactGeneration) + 1
			generation = artifactGeneration
		}
		for _, update := range updates {
			metadataJSON, err := marshalRecognitionMetadata(update.Result)
			if err != nil {
				return err
			}
			result := update.Result
			recognitionUpdates := map[string]any{"profile_id": update.Profile.ID, "profile_revision": update.Profile.Revision, "status": result.Status, "error_code": result.ErrorCode, "media_type": result.MediaType, "title": result.Title, "release_year": result.ReleaseYear, "tmdb_id": result.TMDBID, "confidence": result.Confidence, "category_name": result.CategoryName, "matched_rule_id": result.MatchedRuleID, "metadata_json": metadataJSON, "manual_override": true, "last_generation": generation, "updated_at": now}
			changed := tx.Model(&models.MediaLibraryRecognition{}).Where("id = ? AND library_id = ? AND updated_at = ?", update.Record.ID, libraryID, update.Record.UpdatedAt).Updates(recognitionUpdates)
			if changed.Error != nil {
				return changed.Error
			}
			if changed.RowsAffected != 1 {
				return appError(CodeConflict, "元数据已变化，请重新加载后再保存", nil)
			}
			entryUpdates := map[string]any{"media_type": result.MediaType, "title": result.Title, "series_title": "", "work_key": recognitionWorkKey(result, update.Record.SourceKey), "match_status": result.Status, "tmdb_id": result.TMDBID, "release_year": result.ReleaseYear, "match_confidence": result.Confidence, "recognition_error_code": result.ErrorCode, "category_name": result.CategoryName, "matched_rule_id": result.MatchedRuleID, "updated_at": now}
			if result.MediaType == "tv" {
				entryUpdates["series_title"] = result.Title
			}
			if err := tx.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND recognition_id = ?", libraryID, update.Record.ID).Updates(entryUpdates).Error; err != nil {
				return err
			}
		}
		if artifactGeneration > 0 {
			if err := tx.Model(&models.MediaLibraryRecognition{}).Where("library_id = ?", libraryID).Updates(map[string]any{"last_generation": artifactGeneration, "updated_at": now}).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ?", libraryID).Updates(map[string]any{"generation": artifactGeneration, "updated_at": now}).Error; err != nil {
				return err
			}
			if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Updates(map[string]any{"dirty_generation": artifactGeneration, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		if s.changes != nil {
			change, err := s.changes.RecordTx(tx, libraryID, generation, models.MediaLibraryChangeMetadata, artifactGeneration == 0)
			if err != nil {
				return err
			}
			committedChange = change
		}
		return nil
	})
	if err != nil {
		return err
	}
	if artifactGeneration > 0 {
		return s.artifacts.ScheduleGeneration(libraryID, artifactGeneration)
	}
	if committedChange.State == models.MediaLibraryChangeReady && s.changes != nil {
		s.changes.NotifyCommitted(committedChange.LibraryID, committedChange.Revision)
	}
	return nil
}

func (s *MediaLibraryService) enrichMetadataImageOptions(ctx context.Context, libraryID uint, snapshot tmdb.Snapshot) tmdb.Snapshot {
	if s.metadata == nil || snapshot.TMDBID <= 0 || (snapshot.MediaType != "movie" && snapshot.MediaType != "tv") {
		return snapshot
	}
	var library models.MediaLibrary
	if err := s.db.Select("metadata_language").First(&library, libraryID).Error; err != nil {
		return snapshot
	}
	client, _, _, err := s.metadata.clientWithCredentialInfo()
	if err != nil {
		return snapshot
	}
	match, err := client.GetByID(ctx, snapshot.MediaType, snapshot.TMDBID, library.MetadataLanguage)
	if err != nil || match.ID != snapshot.TMDBID || match.MediaType != snapshot.MediaType {
		return snapshot
	}
	verified := match.Snapshot
	if len(verified.PosterPaths) > 0 {
		snapshot.PosterPaths = verified.PosterPaths
	}
	if len(verified.BackdropPaths) > 0 {
		snapshot.BackdropPaths = verified.BackdropPaths
	}
	return snapshot
}

func (s *MediaLibraryService) catalogMetadataRecognitions(libraryID uint, workToken string) ([]models.MediaLibraryRecognition, error) {
	workKey, err := decodeCatalogToken(workToken)
	if err != nil {
		return nil, err
	}
	var ids []uint
	if err := s.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND work_key = ? AND recognition_id IS NOT NULL", libraryID, workKey).Distinct().Order("recognition_id").Pluck("recognition_id", &ids).Error; err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, appError(CodeConflict, "当前作品没有可编辑的识别元数据", nil)
	}
	var records []models.MediaLibraryRecognition
	if err := s.db.Where("library_id = ? AND id IN ?", libraryID, ids).Order("id").Find(&records).Error; err != nil {
		return nil, err
	}
	if len(records) != len(ids) {
		return nil, appError(CodeConflict, "作品识别记录已变化", nil)
	}
	return records, nil
}

func (s *MediaLibraryService) metadataDocument(libraryID uint, workToken string, record models.MediaLibraryRecognition, snapshot tmdb.Snapshot) MediaMetadataDocument {
	posters := metadataImageOptions(snapshot.PosterPath, snapshot.PosterPaths, func(path string) string { return s.catalogImageURL(path, "w500") })
	backdrops := metadataImageOptions(snapshot.BackdropPath, snapshot.BackdropPaths, func(path string) string { return s.catalogImageURL(path, "w1280") })
	return MediaMetadataDocument{LibraryID: libraryID, WorkID: workToken, Revision: record.UpdatedAt.UnixNano(), TMDBID: snapshot.TMDBID, MediaType: snapshot.MediaType, ManualOverride: record.ManualOverride, Editable: editableFromSnapshot(snapshot), PosterOptions: posters, BackdropOptions: backdrops}
}

func metadataImageOptions(primary string, values []string, resolve func(string) string) []MediaMetadataImageOption {
	values = append([]string{primary}, values...)
	seen := map[string]struct{}{}
	result := make([]MediaMetadataImageOption, 0, len(values))
	for _, value := range values {
		value = safeTMDBImagePath(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, MediaMetadataImageOption{Path: value, URL: resolve(value)})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Path == primary })
	return result
}
