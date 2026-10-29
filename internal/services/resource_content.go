package services

import (
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"gorm.io/gorm"
)

const ContentInspectionCapability = "resource_content_v1"
const codeContentNoMissingFiles = "content_no_missing_files"

// ContentFileFacts is a bounded display projection. Provider IDs, torrent
// indices/hashes and upstream URLs remain in the owning private preview.
type ContentFileFacts struct {
	Token      string                    `json:"token,omitempty"`
	Path       string                    `json:"path"`
	Name       string                    `json:"name"`
	Size       int64                     `json:"size"`
	IsDir      bool                      `json:"is_dir"`
	Kind       string                    `json:"file_kind"`
	Season     *int                      `json:"season,omitempty"`
	Episodes   []int                     `json:"episodes"`
	WorkStatus string                    `json:"work_status"`
	Reason     string                    `json:"unresolved_reason,omitempty"`
	Subtitles  []ContentSubtitleEvidence `json:"subtitles,omitempty"`
}

type ContentSubtitleEvidence struct {
	Language string `json:"language"`
	Script   string `json:"script,omitempty"`
	Source   string `json:"source"`
}

type ContentPreviewInput struct {
	ResultToken  string `json:"result_token"`
	DownloaderID string `json:"downloader_id,omitempty"` // Legacy authorized association only.
	PreviewToken string `json:"preview_token,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type ContentPreviewSummary struct {
	Version        int                `json:"version"`
	Token          string             `json:"token,omitempty"`
	Status         string             `json:"status"`
	Complete       bool               `json:"complete"`
	Files          []ContentFileFacts `json:"files"`
	Analysis       *SiteShareAnalysis `json:"analysis,omitempty"`
	TotalCount     int                `json:"total_count"`
	DisplayedCount int                `json:"displayed_count"`
	NextOffset     *int               `json:"next_offset,omitempty"`
	TotalSize      int64              `json:"total_size"`
	CheckedAt      time.Time          `json:"checked_at"`
	ExpiresAt      time.Time          `json:"expires_at"`
	ErrorCode      string             `json:"error_code,omitempty"`
	Message        string             `json:"message"`
}

type ContentSelectionInput struct {
	SelectionToken      string   `json:"selection_token,omitempty"`
	ResultToken         string   `json:"result_token"`
	PreviewToken        string   `json:"preview_token"`
	SelectedEntryTokens []string `json:"selected_entry_tokens"`
}

type ContentSelectionSummary struct {
	Token               string   `json:"selection_token"`
	PreviewToken        string   `json:"preview_token"`
	SelectedEntryTokens []string `json:"selected_entry_tokens"`
	FileCount           int      `json:"file_count"`
	VideoCount          int      `json:"video_count"`
	TotalSize           int64    `json:"total_size"`
	SelectedBytes       int64    `json:"selected_bytes"`
}

// ResolvedContentSelection is private execution intent, never an HTTP DTO.
// The execution owner must also revalidate its selected downloader and target.
type ResolvedContentSelection struct {
	Manifest         *resourcecontent.Manifest
	Torrent          *resourcecontent.Selection
	Share            *cloud.ShareSelection
	Files            []SiteSharePreviewEntry
	SourceDigest     string
	SelectionDigest  string
	SelectedBytes    int64
	AllFilesVerified bool
	OriginalMagnet   string
	Guard            func(*gorm.DB) error
}

var subtitleLanguagePattern = regexp.MustCompile(`(?i)(?:^|[._\-\[\] ()])(?:zh[-_]?hans|chs|sc)(?:$|[._\-\[\] ()])`)
var subtitleTraditionalPattern = regexp.MustCompile(`(?i)(?:^|[._\-\[\] ()])(?:zh[-_]?hant|cht|tc)(?:$|[._\-\[\] ()])`)
var subtitleChinesePattern = regexp.MustCompile(`(?i)(?:^|[._\-\[\] ()])(?:zh|chi|zho)(?:$|[._\-\[\] ()])`)
var subtitleEnglishPattern = regexp.MustCompile(`(?i)(?:^|[._\-\[\] ()])(?:en|eng)(?:$|[._\-\[\] ()])`)
var subtitleJapanesePattern = regexp.MustCompile(`(?i)(?:^|[._\-\[\] ()])(?:ja|jpn)(?:$|[._\-\[\] ()])`)
var subtitleKoreanPattern = regexp.MustCompile(`(?i)(?:^|[._\-\[\] ()])(?:ko|kor)(?:$|[._\-\[\] ()])`)

func contentFileKind(entry SiteSharePreviewEntry) string {
	if entry.IsDir {
		return "directory"
	}
	if isVideoFile(entry.Path) && !followShareExtraPattern.MatchString(entry.Path) {
		return "video"
	}
	switch strings.ToLower(path.Ext(entry.Path)) {
	case ".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx", ".sup":
		return "subtitle"
	case ".zip", ".rar", ".7z", ".tar", ".gz":
		return "archive"
	default:
		return "other"
	}
}

func contentSubtitleEvidence(entry SiteSharePreviewEntry) []ContentSubtitleEvidence {
	name := strings.TrimSuffix(path.Base(entry.Path), path.Ext(entry.Path))
	source := "release_claim"
	sidecar := contentFileKind(entry) == "subtitle"
	if sidecar {
		source = "sidecar_filename"
	}
	var evidence []ContentSubtitleEvidence
	switch {
	case subtitleLanguagePattern.MatchString(name) || strings.Contains(name, "简中") || strings.Contains(name, "简体"):
		evidence = append(evidence, ContentSubtitleEvidence{Language: "zh", Script: "Hans", Source: source})
	case subtitleTraditionalPattern.MatchString(name) || strings.Contains(name, "繁中") || strings.Contains(name, "繁体"):
		evidence = append(evidence, ContentSubtitleEvidence{Language: "zh", Script: "Hant", Source: source})
	case sidecar && subtitleChinesePattern.MatchString(name) || strings.Contains(name, "中字") || strings.Contains(name, "中文字幕"):
		evidence = append(evidence, ContentSubtitleEvidence{Language: "zh", Source: source})
	}
	if sidecar && subtitleEnglishPattern.MatchString(name) || strings.Contains(name, "英文字幕") {
		evidence = append(evidence, ContentSubtitleEvidence{Language: "en", Source: source})
	}
	if sidecar && subtitleJapanesePattern.MatchString(name) || strings.Contains(name, "日文字幕") || strings.Contains(name, "日语字幕") {
		evidence = append(evidence, ContentSubtitleEvidence{Language: "ja", Source: source})
	}
	if sidecar && subtitleKoreanPattern.MatchString(name) || strings.Contains(name, "韩文字幕") || strings.Contains(name, "韩语字幕") {
		evidence = append(evidence, ContentSubtitleEvidence{Language: "ko", Source: source})
	}
	return evidence
}

func contentFileProjection(entry SiteSharePreviewEntry, claim siteResultClaim, analysis *SiteShareAnalysis, analyzed map[string]siteShareAnalyzedFile) ContentFileFacts {
	kind := contentFileKind(entry)
	facts := ContentFileFacts{Token: entry.Token, Path: entry.Path, Name: path.Base(entry.Path), Size: entry.Size, IsDir: entry.IsDir, Kind: kind, Episodes: []int{}, WorkStatus: "unrecognized", Subtitles: contentSubtitleEvidence(entry)}
	if kind != "video" {
		facts.WorkStatus = "not_applicable"
		return facts
	}
	parsed, ok := shareFileFacts(entry, claim)
	facts.Season, facts.Episodes = cloneInt(parsed.Season), append([]int{}, parsed.Episodes...)
	facts.Reason = "file_identity_unconfirmed"
	if !ok && parsed.Title == "" {
		facts.Reason = "work_context_missing"
	}
	if actual, exists := analyzed[entry.Token]; exists && actual.Group >= 0 && actual.Group < len(analysis.Groups) {
		group := analysis.Groups[actual.Group]
		facts.Season = cloneInt(group.Season)
		facts.WorkStatus = group.Status
		if group.Status == mediaRecognitionStatusMatched {
			facts.Reason = ""
			if group.MediaType == "tv" && (facts.Season == nil || len(facts.Episodes) == 0) {
				facts.Reason = "episode_coordinates_missing"
			}
		} else if group.ErrorCode != "" {
			facts.Reason = group.ErrorCode
		}
	}
	return facts
}
