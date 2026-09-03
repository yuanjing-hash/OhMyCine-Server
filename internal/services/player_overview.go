package services

import (
	"sort"
	"strconv"
	"sync"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

const (
	playerOverviewVersion            = "v1"
	playerOverviewStatusOK           = "ok"
	playerOverviewStatusUnavailable  = "unavailable"
	playerOverviewHeroLimit          = 12
	playerOverviewContinueLimit      = 12
	playerOverviewRecentlyAddedLimit = 24
	playerOverviewFavoritesLimit     = 12
	playerOverviewCollectionsLimit   = 12
	playerOverviewHistoryLimit       = 12
	playerOverviewLibrariesLimit     = 24
	playerOverviewCatalogPageSize    = 20
	playerOverviewHistoryPageSize    = 100
)

// PlayerOverviewSection is an independently degradable, bounded home summary.
// List is always an array so one failed section never changes the response
// shape or prevents the Player from rendering the remaining sections.
type PlayerOverviewSection[T any] struct {
	Status    string `json:"status"`
	List      []T    `json:"list"`
	HasMore   bool   `json:"has_more"`
	ErrorCode string `json:"error_code,omitempty"`
}

type PlayerOverviewSections struct {
	Featured             PlayerOverviewSection[PlayerMediaItem]         `json:"featured"`
	ContinueWatching     PlayerOverviewSection[PlayerHistoryChange]     `json:"continue_watching"`
	RecentlyAdded        PlayerOverviewSection[PlayerMediaItem]         `json:"recently_added"`
	Favorites            PlayerOverviewSection[PlayerMediaItem]         `json:"favorites"`
	AutomaticCollections PlayerOverviewSection[PlayerCollectionSummary] `json:"automatic_collections"`
	ManualCollections    PlayerOverviewSection[PlayerCollectionSummary] `json:"manual_collections"`
	RecentHistory        PlayerOverviewSection[PlayerHistoryChange]     `json:"recent_history"`
	MediaLibraries       PlayerOverviewSection[PlayerMediaLibrary]      `json:"media_libraries"`
}

type PlayerOverview struct {
	Version  string                 `json:"version"`
	Sections PlayerOverviewSections `json:"sections"`
}

type playerOverviewHistoryReader interface {
	List(actor Actor, page, pageSize int, sourceKind string) (PlayerHistoryPage, error)
}

type playerOverviewStateReader interface {
	Favorites(actor Actor) ([]PlayerMediaItem, error)
	Collections(actor Actor, kind string) ([]PlayerCollectionSummary, error)
}

type playerOverviewLibraryReader interface {
	PlayerLibraries(actor Actor) ([]PlayerMediaLibrary, error)
	PlayerCatalog(actor Actor, libraryID uint, query MediaPageQuery) (PlayerMediaItemPage, error)
}

type PlayerOverviewService struct {
	history   playerOverviewHistoryReader
	state     playerOverviewStateReader
	libraries playerOverviewLibraryReader
}

func NewPlayerOverviewService(history *PlayerHistoryService, state *PlayerMediaStateService, libraries *MediaLibraryService) *PlayerOverviewService {
	return newPlayerOverviewService(history, state, libraries)
}

func newPlayerOverviewService(history playerOverviewHistoryReader, state playerOverviewStateReader, libraries playerOverviewLibraryReader) *PlayerOverviewService {
	return &PlayerOverviewService{history: history, state: state, libraries: libraries}
}

func (s *PlayerOverviewService) Overview(actor Actor) PlayerOverview {
	result := PlayerOverview{Version: playerOverviewVersion}

	// Library visibility is one authorization snapshot for every catalog and
	// history section. State sections still run when this shared read fails.
	libraries, librariesErr := s.loadLibraries(actor)
	allowedLibraries := make(map[uint]struct{}, len(libraries))
	for _, library := range libraries {
		allowedLibraries[library.ID] = struct{}{}
	}
	var catalogOnce sync.Once
	var catalogItems []PlayerMediaItem
	var catalogTotal int
	var catalogErr error
	loadCatalog := func() {
		catalogOnce.Do(func() {
			if librariesErr != nil {
				catalogErr = librariesErr
				return
			}
			catalogItems, catalogTotal, catalogErr = s.catalogCandidates(actor, libraries)
		})
	}
	var historyOnce sync.Once
	var historyPage PlayerHistoryPage
	var historyErr error
	loadHistory := func() {
		historyOnce.Do(func() {
			if librariesErr != nil {
				historyErr = librariesErr
				return
			}
			if s == nil || s.history == nil {
				historyErr = appError(CodeInvalidRequest, "Server 暂不支持播放历史", nil)
				return
			}
			historyPage, historyErr = s.history.List(actor, 1, playerOverviewHistoryPageSize, "server")
		})
	}
	var collectionsOnce sync.Once
	var collectionSummaries []PlayerCollectionSummary
	var collectionsErr error
	loadCollections := func() {
		collectionsOnce.Do(func() {
			if s == nil || s.state == nil {
				collectionsErr = appError(CodeInvalidRequest, "Server 暂不支持合集", nil)
				return
			}
			collectionSummaries, collectionsErr = s.state.Collections(actor, "")
		})
	}

	var wait sync.WaitGroup
	wait.Add(8)
	go func() {
		defer wait.Done()
		result.Sections.Featured = loadPlayerOverviewSection(func() ([]PlayerMediaItem, bool, error) {
			loadCatalog()
			if catalogErr != nil {
				return nil, false, catalogErr
			}
			featured := make([]PlayerMediaItem, 0, len(catalogItems))
			for _, item := range catalogItems {
				if item.BackdropPath != "" {
					featured = append(featured, item)
				}
			}
			return boundedOverviewList(featured, playerOverviewHeroLimit, catalogTotal > len(catalogItems))
		})
	}()
	go func() {
		defer wait.Done()
		result.Sections.ContinueWatching = loadPlayerOverviewSection(func() ([]PlayerHistoryChange, bool, error) {
			loadHistory()
			if historyErr != nil {
				return nil, false, historyErr
			}
			return playerOverviewHistoryItems(historyPage, allowedLibraries, true)
		})
	}()
	go func() {
		defer wait.Done()
		result.Sections.RecentlyAdded = loadPlayerOverviewSection(func() ([]PlayerMediaItem, bool, error) {
			loadCatalog()
			if catalogErr != nil {
				return nil, false, catalogErr
			}
			return boundedOverviewList(catalogItems, playerOverviewRecentlyAddedLimit, catalogTotal > len(catalogItems))
		})
	}()
	go func() {
		defer wait.Done()
		result.Sections.Favorites = loadPlayerOverviewSection(func() ([]PlayerMediaItem, bool, error) {
			if s == nil || s.state == nil {
				return nil, false, appError(CodeInvalidRequest, "Server 暂不支持收藏", nil)
			}
			items, err := s.state.Favorites(actor)
			if err != nil {
				return nil, false, err
			}
			return boundedOverviewList(items, playerOverviewFavoritesLimit, false)
		})
	}()
	go func() {
		defer wait.Done()
		result.Sections.AutomaticCollections = loadPlayerOverviewSection(func() ([]PlayerCollectionSummary, bool, error) {
			loadCollections()
			return playerOverviewCollectionItems(collectionSummaries, models.PlayerMediaCollectionSourceTMDB, collectionsErr)
		})
	}()
	go func() {
		defer wait.Done()
		result.Sections.ManualCollections = loadPlayerOverviewSection(func() ([]PlayerCollectionSummary, bool, error) {
			loadCollections()
			return playerOverviewCollectionItems(collectionSummaries, models.PlayerMediaCollectionSourceManual, collectionsErr)
		})
	}()
	go func() {
		defer wait.Done()
		result.Sections.RecentHistory = loadPlayerOverviewSection(func() ([]PlayerHistoryChange, bool, error) {
			loadHistory()
			if historyErr != nil {
				return nil, false, historyErr
			}
			return playerOverviewHistoryItems(historyPage, allowedLibraries, false)
		})
	}()
	go func() {
		defer wait.Done()
		result.Sections.MediaLibraries = loadPlayerOverviewSection(func() ([]PlayerMediaLibrary, bool, error) {
			if librariesErr != nil {
				return nil, false, librariesErr
			}
			return boundedOverviewList(libraries, playerOverviewLibrariesLimit, false)
		})
	}()
	wait.Wait()
	return result
}

func (s *PlayerOverviewService) BrowserOverview(actor Actor) BrowserMediaOverview {
	result := s.Overview(actor)
	libraries, ok := s.libraries.(*MediaLibraryService)
	if !ok {
		libraries = nil
	}
	return BrowserMediaOverview{
		Version: result.Version,
		Sections: BrowserMediaOverviewSections{
			ContinueWatching:     browserSection(result.Sections.ContinueWatching, func(items []PlayerHistoryChange) []BrowserHistoryItem { return browserHistoryItems(items, libraries) }),
			RecentlyAdded:        browserSection(result.Sections.RecentlyAdded, browserMediaItems),
			Favorites:            browserSection(result.Sections.Favorites, browserMediaItems),
			AutomaticCollections: browserSection(result.Sections.AutomaticCollections, browserCollections),
			ManualCollections:    browserSection(result.Sections.ManualCollections, browserCollections),
			MediaLibraries:       browserSection(result.Sections.MediaLibraries, browserLibraries),
		},
	}
}

func (s *PlayerOverviewService) loadLibraries(actor Actor) ([]PlayerMediaLibrary, error) {
	if s == nil || s.libraries == nil {
		return nil, appError(CodeInvalidRequest, "Server 暂不支持媒体总览", nil)
	}
	return s.libraries.PlayerLibraries(actor)
}

func (s *PlayerOverviewService) catalogCandidates(actor Actor, libraries []PlayerMediaLibrary) ([]PlayerMediaItem, int, error) {
	if len(libraries) > playerOverviewHeroLimit {
		libraries = libraries[:playerOverviewHeroLimit]
	}
	items := make([]PlayerMediaItem, 0, len(libraries)*playerOverviewCatalogPageSize)
	total := 0
	successful := 0
	var firstErr error
	for _, library := range libraries {
		page, err := s.libraries.PlayerCatalog(actor, library.ID, MediaPageQuery{Page: 1, PageSize: playerOverviewCatalogPageSize})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		successful++
		total += page.Total
		items = append(items, page.List...)
	}
	if len(libraries) > 0 && successful == 0 {
		return nil, 0, firstErr
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].ModifiedAt.Equal(items[right].ModifiedAt) {
			return items[left].ItemToken < items[right].ItemToken
		}
		return items[left].ModifiedAt.After(items[right].ModifiedAt)
	})
	return items, total, nil
}

func playerOverviewHistoryItems(page PlayerHistoryPage, allowedLibraries map[uint]struct{}, continueOnly bool) ([]PlayerHistoryChange, bool, error) {
	items := make([]PlayerHistoryChange, 0, len(page.List))
	for _, item := range page.List {
		libraryID, parseErr := strconv.ParseUint(item.LibraryID, 10, 32)
		if parseErr != nil || libraryID == 0 {
			continue
		}
		if _, allowed := allowedLibraries[uint(libraryID)]; !allowed {
			continue
		}
		if continueOnly && !playerOverviewContinueEligible(item) {
			continue
		}
		items = append(items, item)
	}
	limit := playerOverviewHistoryLimit
	if continueOnly {
		limit = playerOverviewContinueLimit
	}
	return boundedOverviewList(items, limit, page.HasMore)
}

func playerOverviewContinueEligible(item PlayerHistoryChange) bool {
	if item.Deleted || item.Completed || item.Position <= 0 {
		return false
	}
	return item.Duration == nil || *item.Duration <= 0 || item.Position / *item.Duration < 0.92
}

func playerOverviewCollectionItems(items []PlayerCollectionSummary, source string, err error) ([]PlayerCollectionSummary, bool, error) {
	if err != nil {
		return nil, false, err
	}
	filtered := make([]PlayerCollectionSummary, 0, len(items))
	for _, item := range items {
		if item.Source == source {
			filtered = append(filtered, item)
		}
	}
	return boundedOverviewList(filtered, playerOverviewCollectionsLimit, false)
}

func loadPlayerOverviewSection[T any](loader func() ([]T, bool, error)) PlayerOverviewSection[T] {
	items, hasMore, err := loader()
	if err != nil {
		return PlayerOverviewSection[T]{Status: playerOverviewStatusUnavailable, List: []T{}, ErrorCode: ErrorCode(err)}
	}
	if items == nil {
		items = []T{}
	}
	return PlayerOverviewSection[T]{Status: playerOverviewStatusOK, List: items, HasMore: hasMore}
}

func boundedOverviewList[T any](items []T, limit int, inheritedHasMore bool) ([]T, bool, error) {
	hasMore := inheritedHasMore || len(items) > limit
	if len(items) > limit {
		items = items[:limit]
	}
	return items, hasMore, nil
}
