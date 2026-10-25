package services

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

const maxCoverageSeasons = 200

type MediaCoverageService struct {
	catalogStore *CatalogSnapshotStore
	db           *gorm.DB
	metadata     *MetadataSettingsService
	now          func() time.Time
}

type MediaCoverageLibrary struct {
	ID               uint       `json:"id"`
	Name             string     `json:"name"`
	ScanState        string     `json:"scan_state"`
	LastSuccessfulAt *time.Time `json:"last_successful_at,omitempty"`
	ContentRevision  uint64     `json:"content_revision"`
}

type MediaCoverageFreshness struct {
	CheckedAt        time.Time `json:"checked_at"`
	LibraryScanState string    `json:"library_scan_state"`
	TMDBState        string    `json:"tmdb_state"`
}

type MediaCoverageEpisode struct {
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name,omitempty"`
	AirDate       string `json:"air_date,omitempty"`
	Status        string `json:"status"`
	LibraryIDs    []uint `json:"library_ids"`
	uncertainty   string
}

type mediaCoverageUncertainty struct {
	Library  int
	AirDate  int
	Metadata int
	Seasons  int
}

type MediaCoverageCounts struct {
	Total   int `json:"total"`
	Present int `json:"present"`
	Missing int `json:"missing"`
	Future  int `json:"future"`
	Unknown int `json:"unknown"`
}

type MediaCoverageSeason struct {
	SeasonNumber int                    `json:"season_number"`
	Name         string                 `json:"name"`
	PosterURL    string                 `json:"poster_url,omitempty"`
	Special      bool                   `json:"special"`
	Status       string                 `json:"status"`
	Counts       MediaCoverageCounts    `json:"counts"`
	Episodes     []MediaCoverageEpisode `json:"episodes"`
	uncertainty  mediaCoverageUncertainty
}

type MediaCoverage struct {
	MediaType string                 `json:"media_type"`
	TMDBID    int64                  `json:"tmdb_id"`
	Title     string                 `json:"title"`
	Status    string                 `json:"status"`
	Libraries []MediaCoverageLibrary `json:"libraries"`
	Freshness MediaCoverageFreshness `json:"freshness"`
	Movie     *struct {
		Present    bool   `json:"present"`
		LibraryIDs []uint `json:"library_ids"`
	} `json:"movie,omitempty"`
	TV *struct {
		Counts  MediaCoverageCounts   `json:"counts"`
		Seasons []MediaCoverageSeason `json:"seasons"`
	} `json:"tv,omitempty"`
}

func NewMediaCoverageService(db *gorm.DB, metadata *MetadataSettingsService) *MediaCoverageService {
	return &MediaCoverageService{db: db, metadata: metadata, now: func() time.Time { return time.Now().UTC() }}
}

func (s *MediaCoverageService) SetCatalogSnapshotStore(store *CatalogSnapshotStore) {
	s.catalogStore = store
}

func (s *MediaCoverageService) Coverage(ctx context.Context, actor Actor, mediaType string, tmdbID int64) (MediaCoverage, error) {
	if !actor.HasPermission(authz.PermissionDiscoveryRead) || !actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		return MediaCoverage{}, appError(CodePermissionDenied, "无权查看媒体库覆盖率", nil)
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if tmdbID <= 0 || (mediaType != "movie" && mediaType != "tv") {
		return MediaCoverage{}, appError(CodeInvalidRequest, "媒体覆盖率身份无效", nil)
	}
	if s.metadata == nil {
		return MediaCoverage{}, appError(CodeTMDBUnavailable, "TMDB 详情服务暂时不可用", nil)
	}
	client, err := s.metadata.Client()
	if err != nil {
		return MediaCoverage{}, appError(CodeTMDBUnavailable, "TMDB 详情服务暂时不可用", nil)
	}
	verified, err := client.GetByID(ctx, mediaType, tmdbID, "zh-CN")
	if err != nil {
		return MediaCoverage{}, appError(tmdb.ErrorCode(err), "TMDB 作品身份无法解析", nil)
	}
	libraries, entries, reliable, scanState, err := s.coverageCatalog(ctx, actor, mediaType, tmdbID)
	if err != nil {
		return MediaCoverage{}, err
	}
	result := MediaCoverage{MediaType: mediaType, TMDBID: tmdbID, Title: verified.Title, Status: "unknown", Libraries: libraries, Freshness: MediaCoverageFreshness{CheckedAt: s.now(), LibraryScanState: scanState, TMDBState: "complete"}}
	if mediaType == "movie" {
		ids := uniqueEntryLibraries(entries, nil, nil)
		result.Movie = &struct {
			Present    bool   `json:"present"`
			LibraryIDs []uint `json:"library_ids"`
		}{Present: len(ids) > 0, LibraryIDs: ids}
		switch {
		case len(ids) > 0:
			result.Status = "present"
		case reliable:
			result.Status = "missing"
		default:
			result.Status = "unknown"
		}
		normalizeMediaCoverageCollections(&result)
		return result, nil
	}

	presence := make(map[[2]int][]uint)
	for _, entry := range entries {
		if entry.Season == nil || entry.Episode == nil || *entry.Season < 0 || *entry.Episode <= 0 {
			continue
		}
		key := [2]int{*entry.Season, *entry.Episode}
		presence[key] = appendUniqueUint(presence[key], entry.LibraryID)
	}
	seasons := append([]tmdb.SeasonSnapshot(nil), verified.Snapshot.Seasons...)
	knownSeasons := make(map[int]bool, len(seasons))
	for _, season := range seasons {
		knownSeasons[season.SeasonNumber] = true
	}
	// Catalog identity remains positive evidence when TMDB omits a season.
	for coordinate := range presence {
		if !knownSeasons[coordinate[0]] {
			seasons = append(seasons, tmdb.SeasonSnapshot{SeasonNumber: coordinate[0]})
			knownSeasons[coordinate[0]] = true
			result.Freshness.TMDBState = "partial"
		}
	}
	if len(seasons) > maxCoverageSeasons {
		seasons = seasons[:maxCoverageSeasons]
		result.Freshness.TMDBState = "partial"
	}
	sort.SliceStable(seasons, func(left, right int) bool { return seasons[left].SeasonNumber < seasons[right].SeasonNumber })
	seasonEpisodes := make([][]tmdb.EpisodeSnapshot, len(seasons))
	seasonErrors := make([]bool, len(seasons))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for index, season := range seasons {
		index, season := index, season
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				seasonErrors[index] = true
				return
			}
			episodes, fetchErr := client.GetTVSeasonEpisodes(ctx, tmdbID, season.SeasonNumber, "zh-CN")
			if fetchErr != nil {
				seasonErrors[index] = true
				return
			}
			seasonEpisodes[index] = episodes
		}()
	}
	wait.Wait()
	if ctx.Err() != nil {
		return MediaCoverage{}, ctx.Err()
	}
	tv := &struct {
		Counts  MediaCoverageCounts   `json:"counts"`
		Seasons []MediaCoverageSeason `json:"seasons"`
	}{Seasons: make([]MediaCoverageSeason, 0, len(seasons))}
	for index, season := range seasons {
		projection := MediaCoverageSeason{SeasonNumber: season.SeasonNumber, Name: season.Name, Special: season.SeasonNumber == 0, Status: "unknown", Episodes: []MediaCoverageEpisode{}}
		if upstream, imageErr := client.ImageURL(season.PosterPath, "w300"); imageErr == nil {
			projection.PosterURL = proxyDiscoveryImage("tmdb", upstream)
		}
		episodes := append([]tmdb.EpisodeSnapshot(nil), seasonEpisodes[index]...)
		metadataEpisodes := make(map[int]bool, len(episodes))
		for _, episode := range episodes {
			metadataEpisodes[episode.EpisodeNumber] = true
		}
		if seasonErrors[index] || len(episodes) == 0 {
			result.Freshness.TMDBState = "partial"
			projection.uncertainty.Seasons = 1
		}
		for coordinate := range presence {
			if coordinate[0] == season.SeasonNumber && !metadataEpisodes[coordinate[1]] {
				episodes = append(episodes, tmdb.EpisodeSnapshot{EpisodeNumber: coordinate[1], SeasonNumber: coordinate[0]})
				result.Freshness.TMDBState = "partial"
			}
		}
		sort.SliceStable(episodes, func(left, right int) bool { return episodes[left].EpisodeNumber < episodes[right].EpisodeNumber })
		seenEpisodes := make(map[int]struct{}, len(episodes))
		for _, episode := range episodes {
			if episode.EpisodeNumber <= 0 {
				result.Freshness.TMDBState = "partial"
				continue
			}
			if _, duplicate := seenEpisodes[episode.EpisodeNumber]; duplicate {
				result.Freshness.TMDBState = "partial"
				continue
			}
			seenEpisodes[episode.EpisodeNumber] = struct{}{}
			ids := append([]uint{}, presence[[2]int{season.SeasonNumber, episode.EpisodeNumber}]...)
			sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
			item := MediaCoverageEpisode{EpisodeNumber: episode.EpisodeNumber, Name: episode.Name, AirDate: episode.AirDate, LibraryIDs: ids}
			airing := coverageAirDateState(episode.AirDate, s.now())
			switch {
			case len(ids) > 0:
				item.Status = "present"
			case airing == "future":
				item.Status = "future"
			case airing == "aired" && reliable:
				item.Status = "missing"
			default:
				item.Status = "unknown"
				if airing == "aired" {
					item.uncertainty = "library"
					projection.uncertainty.Library++
				} else {
					item.uncertainty = "air_date"
					projection.uncertainty.AirDate++
				}
			}
			projection.Episodes = append(projection.Episodes, item)
			incrementCoverageCount(&projection.Counts, item.Status)
		}
		// TMDB occasionally returns a syntactically valid but truncated season
		// payload. Never silently shrink the declared season: retain the known
		// episode rows and account for the unavailable remainder as unknown.
		if missingFacts := max(0, season.EpisodeCount-projection.Counts.Total); missingFacts > 0 {
			projection.Counts.Total += missingFacts
			projection.Counts.Unknown += missingFacts
			projection.uncertainty.Metadata += missingFacts
			result.Freshness.TMDBState = "partial"
		}
		projection.Status = coverageSeasonStatus(projection.Counts)
		tv.Seasons = append(tv.Seasons, projection)
		if !projection.Special {
			addCoverageCounts(&tv.Counts, projection.Counts)
		}
	}
	result.TV = tv
	result.Status = coverageSeasonStatus(tv.Counts)
	normalizeMediaCoverageCollections(&result)
	return result, nil
}

func normalizeMediaCoverageCollections(result *MediaCoverage) {
	if result.Libraries == nil {
		result.Libraries = []MediaCoverageLibrary{}
	}
	if result.Movie != nil && result.Movie.LibraryIDs == nil {
		result.Movie.LibraryIDs = []uint{}
	}
	if result.TV == nil {
		return
	}
	if result.TV.Seasons == nil {
		result.TV.Seasons = []MediaCoverageSeason{}
	}
	for seasonIndex := range result.TV.Seasons {
		season := &result.TV.Seasons[seasonIndex]
		if season.Episodes == nil {
			season.Episodes = []MediaCoverageEpisode{}
		}
		for episodeIndex := range season.Episodes {
			episode := &season.Episodes[episodeIndex]
			if episode.LibraryIDs == nil {
				episode.LibraryIDs = []uint{}
			}
		}
	}
}

func (s *MediaCoverageService) coverageCatalog(ctx context.Context, actor Actor, mediaType string, tmdbID int64) ([]MediaCoverageLibrary, []models.MediaLibraryEntry, bool, string, error) {
	facade := &MediaLibraryService{db: s.db, catalogStore: s.catalogStore}
	var libraries []MediaCoverageLibrary
	var entries []models.MediaLibraryEntry
	var reliable bool
	state := "unknown"
	err := facade.withCatalogReadTx(ctx, func(tx *gorm.DB) error {
		var err error
		facade := &MediaLibraryService{db: s.db, catalogStore: s.catalogStore}
		authorized, err := facade.authorizedMediaLibraryIDsTx(tx, actor, authz.PermissionMediaLibrariesRead, true)
		if err != nil {
			return err
		}
		var records []models.MediaLibrary
		if err := tx.Select("media_libraries.*").Joins("JOIN storages ON storages.id=media_libraries.storage_id").Where("media_libraries.id IN ? AND media_libraries.enabled = ? AND storages.enabled = ?", authorized, true, true).Order("media_libraries.sort_order ASC,media_libraries.id ASC").Find(&records).Error; err != nil {
			return err
		}
		ids := make([]uint, 0, len(records))
		for _, record := range records {
			ids = append(ids, record.ID)
		}
		reader, err := PinCatalogTx(tx, ids)
		if err != nil {
			return err
		}
		states, err := libraryCatalogStatesTx(tx, reader, ids)
		if err != nil {
			return err
		}
		libraries = make([]MediaCoverageLibrary, 0, len(records))
		reliable = len(records) > 0
		state = "unscanned"
		for _, record := range records {
			projection := states[record.ID]
			reliable = reliable && projection.AbsenceReliable
			if projection.ScanState != "unscanned" {
				state = "partial"
			}
			libraries = append(libraries, MediaCoverageLibrary{ID: record.ID, Name: record.Name, ScanState: projection.ScanState, LastSuccessfulAt: record.LastSuccessfulScanAt, ContentRevision: record.ContentRevision})
		}
		if reliable {
			state = "complete"
		}
		entries, err = coverageEntries(reader, mediaType, tmdbID)
		return err
	})
	return libraries, entries, reliable, state, err
}

func coverageEntries(reader *CatalogReader, mediaType string, tmdbID int64) ([]models.MediaLibraryEntry, error) {
	var entries []models.MediaLibraryEntry
	err := reader.VisibleEntries().Where("media_type = ? AND tmdb_id = ? AND match_status = ?", mediaType, tmdbID, mediaRecognitionStatusMatched).Find(&entries).Error
	return entries, err
}

func coverageAirDateState(value string, now time.Time) string {
	date, err := time.Parse("2006-01-02", strings.TrimSpace(value))
	if err != nil {
		return "unknown"
	}
	today := now.UTC().Truncate(24 * time.Hour)
	if date.After(today) {
		return "future"
	}
	return "aired"
}

func coverageSeasonStatus(counts MediaCoverageCounts) string {
	switch {
	case counts.Total == 0 || counts.Unknown == counts.Total:
		return "unknown"
	case counts.Present == counts.Total:
		return "present"
	case counts.Present > 0 && counts.Missing > 0:
		return "partial"
	case counts.Missing > 0 && counts.Present == 0 && counts.Future == 0 && counts.Unknown == 0:
		return "missing"
	case counts.Future > 0 || counts.Unknown > 0:
		return "future_or_incomplete"
	default:
		return "partial"
	}
}

func incrementCoverageCount(counts *MediaCoverageCounts, status string) {
	counts.Total++
	switch status {
	case "present":
		counts.Present++
	case "missing":
		counts.Missing++
	case "future":
		counts.Future++
	default:
		counts.Unknown++
	}
}

func addCoverageCounts(target *MediaCoverageCounts, source MediaCoverageCounts) {
	target.Total += source.Total
	target.Present += source.Present
	target.Missing += source.Missing
	target.Future += source.Future
	target.Unknown += source.Unknown
}

func appendUniqueUint(values []uint, value uint) []uint {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func uniqueEntryLibraries(entries []models.MediaLibraryEntry, season, episode *int) []uint {
	result := []uint{}
	for _, entry := range entries {
		if season != nil && (entry.Season == nil || *entry.Season != *season) {
			continue
		}
		if episode != nil && (entry.Episode == nil || *entry.Episode != *episode) {
			continue
		}
		result = appendUniqueUint(result, entry.LibraryID)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}
