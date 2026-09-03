package services

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type playerOverviewHistoryFake struct {
	page PlayerHistoryPage
	err  error
}

func (fake *playerOverviewHistoryFake) List(Actor, int, int, string) (PlayerHistoryPage, error) {
	return fake.page, fake.err
}

type playerOverviewStateFake struct {
	favorites      []PlayerMediaItem
	collections    []PlayerCollectionSummary
	favoritesErr   error
	collectionsErr error
}

func (fake *playerOverviewStateFake) Favorites(Actor) ([]PlayerMediaItem, error) {
	return fake.favorites, fake.favoritesErr
}

func (fake *playerOverviewStateFake) Collections(Actor, string) ([]PlayerCollectionSummary, error) {
	return fake.collections, fake.collectionsErr
}

type playerOverviewLibrariesFake struct {
	libraries    []PlayerMediaLibrary
	librariesErr error
	pages        map[uint]PlayerMediaItemPage
	pageErrors   map[uint]error
	mu           sync.Mutex
	pageSizes    []int
}

func (fake *playerOverviewLibrariesFake) PlayerLibraries(Actor) ([]PlayerMediaLibrary, error) {
	return fake.libraries, fake.librariesErr
}

func (fake *playerOverviewLibrariesFake) PlayerCatalog(_ Actor, libraryID uint, query MediaPageQuery) (PlayerMediaItemPage, error) {
	fake.mu.Lock()
	fake.pageSizes = append(fake.pageSizes, query.PageSize)
	fake.mu.Unlock()
	return fake.pages[libraryID], fake.pageErrors[libraryID]
}

func TestPlayerOverviewBoundsOrdersAndFiltersAuthorizedServerState(t *testing.T) {
	now := time.Now().UTC()
	libraries := make([]PlayerMediaLibrary, 0, 25)
	for index := 1; index <= 25; index++ {
		libraries = append(libraries, PlayerMediaLibrary{ID: uint(index), Name: fmt.Sprintf("Library %02d", index)})
	}
	catalog := make([]PlayerMediaItem, 0, 30)
	for index := 0; index < 30; index++ {
		catalog = append(catalog, PlayerMediaItem{
			ID: fmt.Sprintf("work-%02d", index), ItemToken: fmt.Sprintf("work|1|%02d", index),
			LibraryID: 1, Title: fmt.Sprintf("Work %02d", index), Kind: "movie",
			BackdropPath: "/backdrop.jpg", ModifiedAt: now.Add(time.Duration(index) * time.Minute),
		})
	}
	libraryReader := &playerOverviewLibrariesFake{libraries: libraries, pages: map[uint]PlayerMediaItemPage{1: {List: catalog, Total: 40}}, pageErrors: map[uint]error{}}
	for index := 2; index <= 25; index++ {
		libraryReader.pages[uint(index)] = PlayerMediaItemPage{}
	}

	duration := 100.0
	history := &playerOverviewHistoryFake{page: PlayerHistoryPage{HasMore: true, List: []PlayerHistoryChange{
		{SyncKey: "allowed-progress", SourceKind: "server", LibraryID: "1", Position: 10, Duration: &duration},
		{SyncKey: "denied-progress", SourceKind: "server", LibraryID: "99", Position: 20, Duration: &duration},
		{SyncKey: "completed", SourceKind: "server", LibraryID: "1", Position: 100, Duration: &duration, Completed: true},
	}}}
	favorites := make([]PlayerMediaItem, 13)
	collections := make([]PlayerCollectionSummary, 0, 26)
	for index := 0; index < 13; index++ {
		favorites[index] = PlayerMediaItem{ID: fmt.Sprintf("favorite-%02d", index)}
		collections = append(collections,
			PlayerCollectionSummary{ID: fmt.Sprintf("automatic-%02d", index), Source: "tmdb"},
			PlayerCollectionSummary{ID: fmt.Sprintf("manual-%02d", index), Source: "manual"},
		)
	}
	state := &playerOverviewStateFake{favorites: favorites, collections: collections}

	overview := newPlayerOverviewService(history, state, libraryReader).Overview(Actor{})
	if overview.Version != "v1" {
		t.Fatalf("version=%q", overview.Version)
	}
	if len(overview.Sections.Featured.List) != 12 || !overview.Sections.Featured.HasMore || overview.Sections.Featured.List[0].ID != "work-29" {
		t.Fatalf("featured=%+v", overview.Sections.Featured)
	}
	if len(overview.Sections.RecentlyAdded.List) != 24 || !overview.Sections.RecentlyAdded.HasMore || overview.Sections.RecentlyAdded.List[0].ID != "work-29" {
		t.Fatalf("recently added=%+v", overview.Sections.RecentlyAdded)
	}
	if got := overview.Sections.ContinueWatching.List; len(got) != 1 || got[0].SyncKey != "allowed-progress" {
		t.Fatalf("continue watching leaked or selected invalid history: %+v", got)
	}
	if got := overview.Sections.RecentHistory.List; len(got) != 2 || got[0].SyncKey != "allowed-progress" || got[1].SyncKey != "completed" {
		t.Fatalf("recent history leaked inaccessible library state: %+v", got)
	}
	if len(overview.Sections.Favorites.List) != 12 || !overview.Sections.Favorites.HasMore {
		t.Fatalf("favorites not bounded: %+v", overview.Sections.Favorites)
	}
	if len(overview.Sections.AutomaticCollections.List) != 12 || !overview.Sections.AutomaticCollections.HasMore || len(overview.Sections.ManualCollections.List) != 12 || !overview.Sections.ManualCollections.HasMore {
		t.Fatalf("collections not split/bounded: automatic=%+v manual=%+v", overview.Sections.AutomaticCollections, overview.Sections.ManualCollections)
	}
	if len(overview.Sections.MediaLibraries.List) != 24 || !overview.Sections.MediaLibraries.HasMore {
		t.Fatalf("libraries not bounded: %+v", overview.Sections.MediaLibraries)
	}
	libraryReader.mu.Lock()
	defer libraryReader.mu.Unlock()
	if len(libraryReader.pageSizes) != 12 {
		t.Fatalf("catalog calls=%d want=12", len(libraryReader.pageSizes))
	}
	for _, pageSize := range libraryReader.pageSizes {
		if pageSize != playerOverviewCatalogPageSize {
			t.Fatalf("catalog page size=%d", pageSize)
		}
	}
}

func TestPlayerOverviewSectionFailureDoesNotFailSiblings(t *testing.T) {
	service := newPlayerOverviewService(
		&playerOverviewHistoryFake{page: PlayerHistoryPage{List: []PlayerHistoryChange{}}},
		&playerOverviewStateFake{
			favoritesErr: errors.New("database unavailable"),
			collections:  []PlayerCollectionSummary{{ID: "manual", Source: "manual"}},
		},
		&playerOverviewLibrariesFake{libraries: []PlayerMediaLibrary{}, pages: map[uint]PlayerMediaItemPage{}, pageErrors: map[uint]error{}},
	)

	overview := service.Overview(Actor{})
	if overview.Sections.Favorites.Status != playerOverviewStatusUnavailable || overview.Sections.Favorites.ErrorCode != "INTERNAL_ERROR" || overview.Sections.Favorites.List == nil {
		t.Fatalf("failed favorites section=%+v", overview.Sections.Favorites)
	}
	if overview.Sections.ManualCollections.Status != playerOverviewStatusOK || len(overview.Sections.ManualCollections.List) != 1 {
		t.Fatalf("manual collections were affected by favorites failure: %+v", overview.Sections.ManualCollections)
	}
	if overview.Sections.Featured.Status != playerOverviewStatusOK || overview.Sections.RecentHistory.Status != playerOverviewStatusOK || overview.Sections.MediaLibraries.Status != playerOverviewStatusOK {
		t.Fatalf("independent sections did not remain available: %+v", overview.Sections)
	}
}

func TestPlayerOverviewCatalogKeepsSuccessfulLibraryWhenSiblingFails(t *testing.T) {
	now := time.Now().UTC()
	libraryReader := &playerOverviewLibrariesFake{
		libraries:  []PlayerMediaLibrary{{ID: 1}, {ID: 2}},
		pages:      map[uint]PlayerMediaItemPage{1: {List: []PlayerMediaItem{{ID: "available", ItemToken: "work|1|available", BackdropPath: "/backdrop.jpg", ModifiedAt: now}}, Total: 1}},
		pageErrors: map[uint]error{2: errors.New("one library failed")},
	}
	overview := newPlayerOverviewService(
		&playerOverviewHistoryFake{page: PlayerHistoryPage{List: []PlayerHistoryChange{}}},
		&playerOverviewStateFake{},
		libraryReader,
	).Overview(Actor{})
	if overview.Sections.Featured.Status != playerOverviewStatusOK || len(overview.Sections.Featured.List) != 1 || overview.Sections.RecentlyAdded.Status != playerOverviewStatusOK || len(overview.Sections.RecentlyAdded.List) != 1 {
		t.Fatalf("one library failure hid successful catalog summary: featured=%+v recent=%+v", overview.Sections.Featured, overview.Sections.RecentlyAdded)
	}
}
