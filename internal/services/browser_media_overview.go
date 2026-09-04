package services

import "time"

// BrowserMediaItem is the minimal Server catalog projection used by the
// cookie-authenticated Web UI. It deliberately excludes Player device tokens,
// provider locators, physical paths, and stream identities.
type BrowserMediaItem struct {
	LibraryID    uint      `json:"library_id"`
	WorkID       string    `json:"work_id"`
	Title        string    `json:"title"`
	Kind         string    `json:"kind"`
	ReleaseYear  *int      `json:"release_year,omitempty"`
	Rating       float64   `json:"rating,omitempty"`
	PosterURL    string    `json:"poster_url,omitempty"`
	BackdropURL  string    `json:"backdrop_url,omitempty"`
	SeasonCount  int64     `json:"season_count,omitempty"`
	EpisodeCount int64     `json:"episode_count,omitempty"`
	CategoryName string    `json:"category_name,omitempty"`
	ModifiedAt   time.Time `json:"modified_at"`
}

type BrowserHistoryItem struct {
	LibraryID   uint     `json:"library_id"`
	WorkID      string   `json:"work_id"`
	SourceKind  string   `json:"source_kind"`
	SourceName  string   `json:"source_name"`
	Playable    bool     `json:"playable"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	MediaType   string   `json:"media_type,omitempty"`
	PosterURL   string   `json:"poster_url,omitempty"`
	BackdropURL string   `json:"backdrop_url,omitempty"`
	Position    float64  `json:"position"`
	Duration    *float64 `json:"duration,omitempty"`
	Completed   bool     `json:"completed"`
	UpdatedAt   int64    `json:"updated_at"`
}

type BrowserHistoryPage struct {
	List     []BrowserHistoryItem `json:"list"`
	Total    int64                `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"page_size"`
	HasMore  bool                 `json:"has_more"`
}

type BrowserCollectionSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	ItemCount   int    `json:"item_count"`
	PosterURL   string `json:"poster_url,omitempty"`
	BackdropURL string `json:"backdrop_url,omitempty"`
}

type BrowserMediaLibrary struct {
	ID             uint       `json:"id"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
	EntryCount     int64      `json:"entry_count"`
	WorkCount      int64      `json:"work_count"`
	ArtworkURL     string     `json:"artwork_url,omitempty"`
	LastSuccessful *time.Time `json:"last_successful_scan_at,omitempty"`
}

type BrowserMediaOverviewSections struct {
	ContinueWatching     PlayerOverviewSection[BrowserHistoryItem]       `json:"continue_watching"`
	RecentlyAdded        PlayerOverviewSection[BrowserMediaItem]         `json:"recently_added"`
	Favorites            PlayerOverviewSection[BrowserMediaItem]         `json:"favorites"`
	AutomaticCollections PlayerOverviewSection[BrowserCollectionSummary] `json:"automatic_collections"`
	ManualCollections    PlayerOverviewSection[BrowserCollectionSummary] `json:"manual_collections"`
	MediaLibraries       PlayerOverviewSection[BrowserMediaLibrary]      `json:"media_libraries"`
}

type BrowserMediaOverview struct {
	Version  string                       `json:"version"`
	Sections BrowserMediaOverviewSections `json:"sections"`
}

func browserMediaItem(item PlayerMediaItem) BrowserMediaItem {
	return BrowserMediaItem{
		LibraryID: item.LibraryID, WorkID: item.ID, Title: item.Title, Kind: item.Kind,
		ReleaseYear: item.ReleaseYear, Rating: item.Rating, PosterURL: item.PosterURL,
		BackdropURL: item.BackdropURL, SeasonCount: item.SeasonCount,
		EpisodeCount: item.EpisodeCount, CategoryName: item.CategoryName,
		ModifiedAt: item.ModifiedAt,
	}
}

func browserMediaItems(items []PlayerMediaItem) []BrowserMediaItem {
	result := make([]BrowserMediaItem, 0, len(items))
	for _, item := range items {
		result = append(result, browserMediaItem(item))
	}
	return result
}

func browserCollection(item PlayerCollectionSummary) BrowserCollectionSummary {
	return BrowserCollectionSummary{
		ID: item.ID, Name: item.Name, Kind: item.Kind, Source: item.Source,
		ItemCount: item.ItemCount, PosterURL: item.PosterURL, BackdropURL: item.BackdropURL,
	}
}

func browserCollections(items []PlayerCollectionSummary) []BrowserCollectionSummary {
	result := make([]BrowserCollectionSummary, 0, len(items))
	for _, item := range items {
		result = append(result, browserCollection(item))
	}
	return result
}

func browserLibrary(item PlayerMediaLibrary) BrowserMediaLibrary {
	return BrowserMediaLibrary{
		ID: item.ID, Name: item.Name, Status: item.Status, EntryCount: item.EntryCount,
		WorkCount: item.WorkCount, ArtworkURL: item.ArtworkURL, LastSuccessful: item.LastSuccessful,
	}
}

func browserLibraries(items []PlayerMediaLibrary) []BrowserMediaLibrary {
	result := make([]BrowserMediaLibrary, 0, len(items))
	for _, item := range items {
		result = append(result, browserLibrary(item))
	}
	return result
}

func browserHistoryItem(item PlayerHistoryChange, libraries *MediaLibraryService) (BrowserHistoryItem, bool) {
	result := BrowserHistoryItem{
		WorkID: item.SyncKey, SourceKind: item.SourceKind,
		SourceName: browserHistorySourceName(item), Title: item.DisplayTitle,
		Subtitle: item.DisplaySubtitle, MediaType: item.MediaType,
		PosterURL: safeHistoryArtwork(item.PosterURL), BackdropURL: safeHistoryArtwork(item.BackdropURL), Position: item.Position,
		Duration: cloneHistoryFloat64(item.Duration), Completed: item.Completed, UpdatedAt: item.UpdatedAt,
	}
	if item.SourceKind != "server" {
		return result, result.WorkID != ""
	}
	parsed, err := parseServerHistoryToken(item.ItemToken)
	if err != nil {
		return BrowserHistoryItem{}, false
	}
	result.LibraryID, result.WorkID, result.Playable = parsed.libraryID, parsed.workToken, true
	if libraries != nil {
		result.PosterURL = libraries.catalogImageURL(item.PosterPath, "w500")
		result.BackdropURL = libraries.catalogImageURL(item.BackdropPath, "w1280")
	}
	return result, true
}

func browserHistorySourceName(item PlayerHistoryChange) string {
	if item.SourceName != "" {
		return item.SourceName
	}
	switch item.SourceKind {
	case "server":
		return "OhMyCine Server"
	case "emby":
		return "Emby"
	case "jellyfin":
		return "Jellyfin"
	case "local", "local-file":
		return "本机媒体"
	case "115":
		return "115"
	default:
		return item.SourceKind
	}
}

func browserHistoryItems(items []PlayerHistoryChange, libraries *MediaLibraryService) []BrowserHistoryItem {
	result := make([]BrowserHistoryItem, 0, len(items))
	for _, item := range items {
		if projected, ok := browserHistoryItem(item, libraries); ok {
			result = append(result, projected)
		}
	}
	return result
}

func browserSection[T, U any](section PlayerOverviewSection[T], mapItems func([]T) []U) PlayerOverviewSection[U] {
	return PlayerOverviewSection[U]{Status: section.Status, List: mapItems(section.List), HasMore: section.HasMore, ErrorCode: section.ErrorCode}
}
