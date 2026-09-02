package services

import (
	"errors"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/releaseversion"
)

const maxStructureIssueSamples = 100

type StructurePlanItem struct {
	Kind             string `json:"kind"`
	WorkKey          string `json:"work_key"`
	SourceRelative   string `json:"source_relative"`
	TargetRelative   string `json:"target_relative"`
	ProviderID       string `json:"provider_id,omitempty"`
	ParentProviderID string `json:"parent_provider_id,omitempty"`
	// AllowProviderRootSource is reserved for the verified historical 115
	// cid=0 transfer defect. Normal structure plans never set it.
	AllowProviderRootSource bool  `json:"allow_provider_root_source,omitempty"`
	Size                    int64 `json:"size"`
}

type StructureIssue struct {
	Code         string `json:"code"`
	Kind         string `json:"kind"`
	WorkKey      string `json:"-"`
	Title        string `json:"title,omitempty"`
	CurrentPath  string `json:"current_path,omitempty"`
	ExpectedPath string `json:"expected_path,omitempty"`
	Repairable   bool   `json:"repairable"`
}

type StructurePlan struct {
	Version         int                 `json:"version"`
	LibraryID       uint                `json:"library_id"`
	Generation      uint64              `json:"generation"`
	RuleFingerprint string              `json:"rule_fingerprint"`
	Items           []StructurePlanItem `json:"items"`
	Issues          []StructureIssue    `json:"issues"`
	IssueCount      int                 `json:"issue_count"`
	Unrecognized    int                 `json:"unrecognized"`
}

// StructurePlanner contains no storage behavior. It turns catalog facts and
// the same immutable naming policy used by Transfer into an exact move plan.
// Local and cloud backends consume this plan through separate implementations.
type StructurePlanner struct{}

func (StructurePlanner) Build(library models.MediaLibrary, entries []models.MediaLibraryEntry, assets []models.MediaLibrarySourceAsset, workKey string) (StructurePlan, error) {
	plan := StructurePlan{Version: 1, LibraryID: library.ID, Generation: library.BaselineGeneration, RuleFingerprint: libraryRuleFingerprint(library)}
	workKey = strings.TrimSpace(workKey)
	filtered := make([]models.MediaLibraryEntry, 0, len(entries))
	workKeysByDirectory := make(map[string]map[string]struct{}, len(entries))
	for _, entry := range entries {
		entryPath := safeStructurePath(entry.RelativePath)
		if entryPath != "" && entry.WorkKey != "" {
			directory := pathpkg.Dir(entryPath)
			if workKeysByDirectory[directory] == nil {
				workKeysByDirectory[directory] = map[string]struct{}{}
			}
			workKeysByDirectory[directory][entry.WorkKey] = struct{}{}
		}
		if workKey != "" && entry.WorkKey != workKey {
			continue
		}
		if entry.MatchStatus != mediaRecognitionStatusMatched || entry.TMDBID == nil || (entry.MediaType != "movie" && entry.MediaType != "tv") {
			plan.Unrecognized++
			plan.addIssue(StructureIssue{Code: "media_unrecognized", Kind: "video", WorkKey: entry.WorkKey, Title: entry.Title, CurrentPath: safeStructurePath(entry.RelativePath), Repairable: false})
			continue
		}
		filtered = append(filtered, entry)
	}

	videoTargets := make(map[string]string, len(filtered))
	videoWork := make(map[string]string, len(filtered))
	workRoots := make(map[string]string, len(filtered))
	workTitles := make(map[string]string, len(filtered))
	usedTargets := make(map[string]string, len(filtered)+len(assets))
	for _, entry := range filtered {
		target, err := structureVideoTarget(library, entry)
		if err != nil {
			return StructurePlan{}, err
		}
		source := safeStructurePath(entry.RelativePath)
		if source == "" || target == "" {
			return StructurePlan{}, errors.New("structure path is invalid")
		}
		key := strings.ToLower(target)
		if other, exists := usedTargets[key]; exists && !strings.EqualFold(other, source) {
			return StructurePlan{}, errors.New("structure target is duplicated")
		}
		usedTargets[key] = source
		videoTargets[source] = target
		videoWork[source] = entry.WorkKey
		workRoot := pathpkg.Dir(target)
		if entry.MediaType == "tv" {
			workRoot = pathpkg.Dir(workRoot)
		}
		if current := workRoots[entry.WorkKey]; current == "" || workRoot < current {
			workRoots[entry.WorkKey] = workRoot
		}
		workTitles[entry.WorkKey] = entry.Title
		if !strings.EqualFold(source, target) {
			plan.Items = append(plan.Items, StructurePlanItem{Kind: "video", WorkKey: entry.WorkKey, SourceRelative: source, TargetRelative: target, ProviderID: entry.ProviderID, Size: entry.Size})
			plan.addIssue(StructureIssue{Code: "path_mismatch", Kind: "video", WorkKey: entry.WorkKey, Title: entry.Title, CurrentPath: source, ExpectedPath: target, Repairable: true})
		}
	}

	activeAssets := make([]models.MediaLibrarySourceAsset, 0, len(assets))
	for _, asset := range assets {
		if asset.Active {
			activeAssets = append(activeAssets, asset)
		}
	}
	for _, asset := range activeAssets {
		source := safeStructurePath(asset.RelativePath)
		if source == "" {
			continue
		}
		target, associatedWork := structureSidecarTarget(source, videoTargets, videoWork, workRoots, workKeysByDirectory)
		if target == source || (workKey != "" && associatedWork != workKey) {
			continue
		}
		key := strings.ToLower(target)
		if other, exists := usedTargets[key]; exists && !strings.EqualFold(other, source) {
			return StructurePlan{}, errors.New("structure sidecar target is duplicated")
		}
		usedTargets[key] = source
		plan.Items = append(plan.Items, StructurePlanItem{Kind: "sidecar", WorkKey: associatedWork, SourceRelative: source, TargetRelative: target, ProviderID: asset.ProviderID, ParentProviderID: asset.ParentProviderID, Size: asset.Size})
		plan.addIssue(StructureIssue{Code: "path_mismatch", Kind: "sidecar", WorkKey: associatedWork, Title: workTitles[associatedWork], CurrentPath: source, ExpectedPath: target, Repairable: true})
	}

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

func (p *StructurePlan) addIssue(issue StructureIssue) {
	p.IssueCount++
	if len(p.Issues) < maxStructureIssueSamples {
		p.Issues = append(p.Issues, issue)
	}
}

func structureVideoTarget(library models.MediaLibrary, entry models.MediaLibraryEntry) (string, error) {
	title := entry.Title
	if entry.MediaType == "tv" && strings.TrimSpace(entry.SeriesTitle) != "" {
		title = entry.SeriesTitle
	}
	values := transferTemplateValues{Category: entry.CategoryName, Title: title, Year: entry.ReleaseYear, Season: entry.Season, Episode: entry.Episode}
	dirTemplate, fileTemplate := library.MovieDirectoryTemplate, library.MovieFilenameTemplate
	if entry.MediaType == "tv" {
		if entry.Season == nil || entry.Episode == nil {
			return "", errPackageEpisodeUnrecognized
		}
		dirTemplate, fileTemplate = library.TVDirectoryTemplate, library.TVFilenameTemplate
	} else {
		values.Version = releaseversion.Parse(entry.RelativePath)
	}
	directory, err := renderImportTemplate(dirTemplate, values, true)
	if err != nil {
		return "", err
	}
	base, err := renderImportTemplate(fileTemplate, values, false)
	if err != nil {
		return "", err
	}
	if entry.MediaType == "movie" && values.Version != "" && !strings.Contains(fileTemplate, "{version}") && !strings.Contains(strings.ToLower(base), strings.ToLower(values.Version)) {
		base = appendMovieReleaseVersion(base, values.Version)
	}
	extension := strings.ToLower(pathpkg.Ext(strings.ReplaceAll(entry.RelativePath, "\\", "/")))
	target := filepath.ToSlash(filepath.Join(directory, base+extension))
	return sanitizeTransferRelativePath(target)
}

func structureSidecarTarget(source string, videos, workByVideo, workRoots map[string]string, workKeysByDirectory map[string]map[string]struct{}) (string, string) {
	sourceDir := pathpkg.Dir(source)
	sourceBase := strings.TrimSuffix(pathpkg.Base(source), pathpkg.Ext(source))
	extension := strings.ToLower(pathpkg.Ext(source))
	for videoSource, videoTarget := range videos {
		if pathpkg.Dir(videoSource) != sourceDir {
			continue
		}
		videoBase := strings.TrimSuffix(pathpkg.Base(videoSource), pathpkg.Ext(videoSource))
		lowerSource, lowerVideo := strings.ToLower(sourceBase), strings.ToLower(videoBase)
		if strings.EqualFold(sourceBase, videoBase) || strings.HasPrefix(lowerSource, lowerVideo+".") || strings.HasPrefix(lowerSource, lowerVideo+"-") {
			suffix := sourceBase[len(videoBase):]
			targetBase := strings.TrimSuffix(pathpkg.Base(videoTarget), pathpkg.Ext(videoTarget))
			return pathpkg.Join(pathpkg.Dir(videoTarget), targetBase+suffix+extension), workByVideo[videoSource]
		}
	}
	// Generic poster/fanart/NFO files belong to the only work in their current
	// directory. Preserve their name while moving them beside that work.
	matchedWork := ""
	for videoSource := range videos {
		if pathpkg.Dir(videoSource) != pathpkg.Dir(source) {
			continue
		}
		if matchedWork != "" && matchedWork != workByVideo[videoSource] {
			return source, ""
		}
		matchedWork = workByVideo[videoSource]
	}
	if matchedWork == "" {
		return source, ""
	}
	directoryWorks := workKeysByDirectory[pathpkg.Dir(source)]
	if len(directoryWorks) != 1 {
		return source, ""
	}
	if _, sameWork := directoryWorks[matchedWork]; !sameWork {
		return source, ""
	}
	workRoot := workRoots[matchedWork]
	if workRoot == "" {
		return source, ""
	}
	return pathpkg.Join(workRoot, pathpkg.Base(source)), matchedWork
}

func safeStructurePath(value string) string {
	value = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/")
	clean, err := sanitizeTransferRelativePath(value)
	if err != nil {
		return ""
	}
	return clean
}
