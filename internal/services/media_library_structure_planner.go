package services

import (
	"context"
	"errors"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/releaseversion"
)

const (
	maxStructureIssueSamples          = 100
	maxStructureConflictSourceSamples = 20
	StructurePlanningWorkers          = 128
	structurePlanningBufferSize       = StructurePlanningWorkers * 2
)

type StructurePlanItem struct {
	Kind    string `json:"kind"`
	WorkKey string `json:"work_key"`
	// Title is retained only while the in-memory plan is assembled. Repair
	// execution does not need it, so it must not expand the private plan JSON.
	Title            string `json:"-"`
	RecognitionID    uint   `json:"-"`
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
	Code                string   `json:"code"`
	Kind                string   `json:"kind"`
	WorkKey             string   `json:"-"`
	Title               string   `json:"title,omitempty"`
	CurrentPath         string   `json:"current_path,omitempty"`
	ExpectedPath        string   `json:"expected_path,omitempty"`
	ConflictSources     []string `json:"conflict_sources,omitempty"`
	ConflictSourceCount int      `json:"conflict_source_count,omitempty"`
	Repairable          bool     `json:"repairable"`
}

type StructurePlan struct {
	Version         int                           `json:"version"`
	LibraryID       uint                          `json:"library_id"`
	Generation      uint64                        `json:"generation"`
	RuleFingerprint string                        `json:"rule_fingerprint"`
	Items           []StructurePlanItem           `json:"items"`
	Issues          []StructureIssue              `json:"issues"`
	IssueCount      int                           `json:"issue_count"`
	Unrecognized    int                           `json:"unrecognized"`
	CheckedItems    int                           `json:"checked_items"`
	Classifications StructureIssueClassifications `json:"classifications"`
	sampleCounts    map[string]int                `json:"-"`
}

type StructureIssueClassifications struct {
	Unrecognized    int `json:"unrecognized"`
	MissingEpisode  int `json:"missing_season_episode"`
	InvalidPath     int `json:"invalid_path"`
	TemplateError   int `json:"template_unavailable"`
	DuplicateTarget int `json:"duplicate_target"`
	SidecarConflict int `json:"sidecar_target_conflict"`
}

// StructurePlanner contains no storage behavior. It turns catalog facts and
// the same immutable naming policy used by Transfer into an exact move plan.
// Local and cloud backends consume this plan through separate implementations.
type StructurePlanner struct {
	// observe is test-only instrumentation. Production planning never depends
	// on it and every callback receives only a stable event code.
	observe func(string)
}

func (p StructurePlanner) Build(library models.MediaLibrary, entries []models.MediaLibraryEntry, assets []models.MediaLibrarySourceAsset, workKey string) (StructurePlan, error) {
	return p.BuildContext(context.Background(), library, entries, assets, workKey, nil)
}

type structurePlanTask struct {
	index        int
	entry        *models.MediaLibraryEntry
	asset        *models.MediaLibrarySourceAsset
	associations *structureAssociationIndex
}

type structurePlanCandidate struct {
	index            int
	kind             string
	workKey          string
	title            string
	source           string
	target           string
	providerID       string
	recognitionID    uint
	parentProviderID string
	size             int64
	allowRootSource  bool
	moveIssueCode    string
	issue            *StructureIssue
}

type structureVideoAssociation struct {
	source  string
	target  string
	workKey string
}

type structureAssociationIndex struct {
	byDirectoryBase       map[string]map[string][]structureVideoAssociation
	singleWorkByDirectory map[string]string
	workRoots             map[string]string
	workTitles            map[string]string
	workKeysByDirectory   map[string]map[string]struct{}
}

func (p StructurePlanner) BuildContext(ctx context.Context, library models.MediaLibrary, entries []models.MediaLibraryEntry, assets []models.MediaLibrarySourceAsset, workKey string, progress func(processed, total int)) (StructurePlan, error) {
	plan := StructurePlan{Version: 1, LibraryID: library.ID, Generation: library.BaselineGeneration, RuleFingerprint: libraryRuleFingerprint(library)}
	workKey = strings.TrimSpace(workKey)
	orderedEntries := append([]models.MediaLibraryEntry(nil), entries...)
	sort.SliceStable(orderedEntries, func(i, j int) bool { return orderedEntries[i].RelativePath < orderedEntries[j].RelativePath })
	orderedAssets := make([]models.MediaLibrarySourceAsset, 0, len(assets))
	for _, asset := range assets {
		if asset.Active {
			orderedAssets = append(orderedAssets, asset)
		}
	}
	sort.SliceStable(orderedAssets, func(i, j int) bool { return orderedAssets[i].RelativePath < orderedAssets[j].RelativePath })
	total := len(orderedEntries) + len(orderedAssets)
	plan.CheckedItems = total

	tasks := make(chan structurePlanTask, structurePlanningBufferSize)
	results := make(chan structurePlanCandidate, structurePlanningBufferSize)
	var workers sync.WaitGroup
	workers.Add(StructurePlanningWorkers)
	for worker := 0; worker < StructurePlanningWorkers; worker++ {
		go func() {
			defer workers.Done()
			if p.observe != nil {
				p.observe("worker_started")
			}
			for task := range tasks {
				if p.observe != nil {
					p.observe("task_started")
				}
				candidate := structurePlanCandidate{index: task.index}
				if task.entry != nil {
					candidate = buildStructureVideoCandidate(library, *task.entry, task.index, workKey)
				} else if task.asset != nil {
					candidate = buildStructureSidecarCandidate(*task.asset, task.index, workKey, task.associations)
				}
				select {
				case results <- candidate:
				case <-ctx.Done():
					return
				}
				if p.observe != nil {
					p.observe("task_finished")
				}
			}
		}()
	}
	defer func() {
		close(tasks)
		workers.Wait()
	}()

	processed := 0
	runTasks := func(batch []structurePlanTask) ([]structurePlanCandidate, error) {
		senderDone := make(chan struct{})
		go func() {
			defer close(senderDone)
			for _, task := range batch {
				select {
				case tasks <- task:
				case <-ctx.Done():
					return
				}
			}
		}()
		batchResults := make([]structurePlanCandidate, 0, len(batch))
		for len(batchResults) < len(batch) {
			select {
			case result := <-results:
				batchResults = append(batchResults, result)
				processed++
				if progress != nil {
					progress(processed, total)
				}
			case <-ctx.Done():
				<-senderDone
				return nil, ctx.Err()
			}
		}
		<-senderDone
		sort.Slice(batchResults, func(i, j int) bool { return batchResults[i].index < batchResults[j].index })
		return batchResults, nil
	}

	videoTasks := make([]structurePlanTask, len(orderedEntries))
	for i := range orderedEntries {
		videoTasks[i] = structurePlanTask{index: i, entry: &orderedEntries[i]}
	}
	videoCandidates, err := runTasks(videoTasks)
	if err != nil {
		return StructurePlan{}, err
	}
	associations := buildStructureAssociationIndex(videoCandidates, orderedEntries)
	assetTasks := make([]structurePlanTask, len(orderedAssets))
	for i := range orderedAssets {
		assetTasks[i] = structurePlanTask{index: len(orderedEntries) + i, asset: &orderedAssets[i], associations: associations}
	}
	assetCandidates, err := runTasks(assetTasks)
	if err != nil {
		return StructurePlan{}, err
	}
	candidates := append(videoCandidates, assetCandidates...)
	for _, candidate := range candidates {
		if candidate.issue != nil {
			plan.addIssue(*candidate.issue)
		}
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

func buildStructureVideoCandidate(library models.MediaLibrary, entry models.MediaLibraryEntry, index int, workKey string) structurePlanCandidate {
	candidate := structurePlanCandidate{index: index, kind: "video", workKey: entry.WorkKey, title: entry.Title, providerID: entry.ProviderID, size: entry.Size}
	if entry.RecognitionID != nil {
		candidate.recognitionID = *entry.RecognitionID
	}
	candidate.source = safeStructurePath(entry.RelativePath)
	if workKey != "" && entry.WorkKey != workKey {
		return candidate
	}
	if candidate.source == "" {
		candidate.issue = &StructureIssue{Code: "invalid_path", Kind: "video", WorkKey: entry.WorkKey, Title: entry.Title}
		return candidate
	}
	// Fast catalog publication intentionally exposes entries before background
	// metadata recognition completes. Pending facts are not recognition
	// failures and must not be presented as work that needs manual repair. The
	// recognition worker schedules another latest-wins diagnosis after it has
	// committed the final projection for this generation.
	if strings.TrimSpace(entry.MatchStatus) == "" || entry.MatchStatus == mediaRecognitionStatusPending {
		return candidate
	}
	if entry.MatchStatus != mediaRecognitionStatusMatched || entry.TMDBID == nil || (entry.MediaType != "movie" && entry.MediaType != "tv") {
		candidate.issue = &StructureIssue{Code: "media_unrecognized", Kind: "video", WorkKey: entry.WorkKey, Title: entry.Title, CurrentPath: candidate.source}
		return candidate
	}
	target, err := structureVideoTarget(library, entry)
	if err != nil {
		code := "template_unavailable"
		if errors.Is(err, errPackageEpisodeUnrecognized) {
			code = "missing_season_episode"
		}
		candidate.issue = &StructureIssue{Code: code, Kind: "video", WorkKey: entry.WorkKey, Title: entry.Title, CurrentPath: candidate.source}
		return candidate
	}
	if target == "" {
		candidate.issue = &StructureIssue{Code: "invalid_path", Kind: "video", WorkKey: entry.WorkKey, Title: entry.Title, CurrentPath: candidate.source}
		return candidate
	}
	candidate.target = target
	return candidate
}

func buildStructureAssociationIndex(candidates []structurePlanCandidate, entries []models.MediaLibraryEntry) *structureAssociationIndex {
	index := &structureAssociationIndex{byDirectoryBase: map[string]map[string][]structureVideoAssociation{}, singleWorkByDirectory: map[string]string{}, workRoots: map[string]string{}, workTitles: map[string]string{}, workKeysByDirectory: map[string]map[string]struct{}{}}
	groups := map[string][]structurePlanCandidate{}
	for _, candidate := range candidates {
		if candidate.source != "" && candidate.workKey != "" {
			directory := pathpkg.Dir(candidate.source)
			if index.workKeysByDirectory[directory] == nil {
				index.workKeysByDirectory[directory] = map[string]struct{}{}
			}
			index.workKeysByDirectory[directory][candidate.workKey] = struct{}{}
		}
		if candidate.target != "" {
			groups[strings.ToLower(candidate.target)] = append(groups[strings.ToLower(candidate.target)], candidate)
		}
	}
	mediaTypeByPath := make(map[string]string, len(entries))
	for _, entry := range entries {
		mediaTypeByPath[safeStructurePath(entry.RelativePath)] = entry.MediaType
	}
	for _, candidate := range candidates {
		if candidate.target == "" || len(groups[strings.ToLower(candidate.target)]) > 1 {
			continue
		}
		directory := pathpkg.Dir(candidate.source)
		if index.byDirectoryBase[directory] == nil {
			index.byDirectoryBase[directory] = map[string][]structureVideoAssociation{}
		}
		base := strings.ToLower(strings.TrimSuffix(pathpkg.Base(candidate.source), pathpkg.Ext(candidate.source)))
		index.byDirectoryBase[directory][base] = append(index.byDirectoryBase[directory][base], structureVideoAssociation{source: candidate.source, target: candidate.target, workKey: candidate.workKey})
		workRoot := pathpkg.Dir(candidate.target)
		if mediaTypeByPath[candidate.source] == "tv" {
			workRoot = pathpkg.Dir(workRoot)
		}
		if current := index.workRoots[candidate.workKey]; current == "" || workRoot < current {
			index.workRoots[candidate.workKey] = workRoot
		}
		index.workTitles[candidate.workKey] = candidate.title
	}
	for directory, byBase := range index.byDirectoryBase {
		for base := range byBase {
			sort.Slice(byBase[base], func(i, j int) bool { return byBase[base][i].source < byBase[base][j].source })
		}
		if works := index.workKeysByDirectory[directory]; len(works) == 1 {
			for only := range works {
				index.singleWorkByDirectory[directory] = only
			}
		}
	}
	return index
}

func buildStructureSidecarCandidate(asset models.MediaLibrarySourceAsset, index int, workKey string, associations *structureAssociationIndex) structurePlanCandidate {
	candidate := structurePlanCandidate{index: index, kind: "sidecar", providerID: asset.ProviderID, parentProviderID: asset.ParentProviderID, size: asset.Size}
	candidate.source = safeStructurePath(asset.RelativePath)
	if candidate.source == "" {
		candidate.issue = &StructureIssue{Code: "invalid_path", Kind: "sidecar", Title: asset.Name}
		return candidate
	}
	target, associatedWork := structureSidecarTargetIndexed(candidate.source, associations)
	if target == candidate.source || associatedWork == "" || (workKey != "" && associatedWork != workKey) {
		return candidate
	}
	target = safeStructurePath(target)
	if target == "" {
		candidate.issue = &StructureIssue{Code: "invalid_path", Kind: "sidecar", Title: asset.Name, CurrentPath: candidate.source}
		return candidate
	}
	candidate.target, candidate.workKey = target, associatedWork
	candidate.title = associations.workTitles[associatedWork]
	return candidate
}

func appendStructureCandidates(plan *StructurePlan, candidates []structurePlanCandidate) {
	groups := make(map[string][]int, len(candidates))
	for i := range candidates {
		if candidates[i].target != "" {
			key := strings.ToLower(candidates[i].target)
			groups[key] = append(groups[key], i)
		}
	}
	type targetConflict struct {
		code        string
		sources     []string
		sourceCount int
	}
	blocked := make(map[int]targetConflict)
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		code := structureTargetConflictCode(candidates, members)
		for _, member := range members {
			if candidates[member].kind == "sidecar" {
				code = "sidecar_target_conflict"
				break
			}
		}
		conflict := targetConflict{code: code, sources: boundedStructureConflictSources(candidates, members), sourceCount: len(members)}
		for _, member := range members {
			blocked[member] = conflict
		}
	}
	for i, candidate := range candidates {
		if candidate.target == "" {
			continue
		}
		if conflict, exists := blocked[i]; exists {
			plan.addIssue(StructureIssue{Code: conflict.code, Kind: candidate.kind, WorkKey: candidate.workKey, Title: candidate.title, CurrentPath: candidate.source, ExpectedPath: candidate.target, ConflictSources: conflict.sources, ConflictSourceCount: conflict.sourceCount})
			continue
		}
		if strings.EqualFold(candidate.source, candidate.target) {
			continue
		}
		plan.Items = append(plan.Items, StructurePlanItem{Kind: candidate.kind, WorkKey: candidate.workKey, Title: candidate.title, RecognitionID: candidate.recognitionID, SourceRelative: candidate.source, TargetRelative: candidate.target, ProviderID: candidate.providerID, ParentProviderID: candidate.parentProviderID, AllowProviderRootSource: candidate.allowRootSource, Size: candidate.size})
		issueCode := candidate.moveIssueCode
		if issueCode == "" {
			issueCode = "path_mismatch"
		}
		plan.addIssue(StructureIssue{Code: issueCode, Kind: candidate.kind, WorkKey: candidate.workKey, Title: candidate.title, CurrentPath: candidate.source, ExpectedPath: candidate.target, Repairable: true})
	}
}

func structureTargetConflictCode(candidates []structurePlanCandidate, members []int) string {
	if len(members) < 2 {
		return "duplicate_target"
	}
	providerID := strings.TrimSpace(candidates[members[0]].providerID)
	if providerID != "" {
		sameProviderFact := true
		for _, member := range members[1:] {
			if strings.TrimSpace(candidates[member].providerID) != providerID {
				sameProviderFact = false
				break
			}
		}
		if sameProviderFact {
			return "catalog_duplicate_conflict"
		}
	}

	recognitionIDs := make(map[uint]struct{}, len(members))
	weakSourceIdentity := false
	for _, member := range members {
		candidate := candidates[member]
		if candidate.recognitionID != 0 {
			recognitionIDs[candidate.recognitionID] = struct{}{}
		}
		if structureSourceTitleSimilarity(candidate.source, candidate.title) < mediarecognition.DefaultScoreConfig().ExactTitleThreshold {
			weakSourceIdentity = true
		}
	}
	// Independently recognized works that converge on one target are not safe
	// to call duplicate files when one source title does not support the bound
	// identity. This is diagnostic-only: it never changes recognition and still
	// blocks every member from automatic repair.
	if len(recognitionIDs) > 1 && weakSourceIdentity {
		return "recognition_suspect_conflict"
	}
	return "duplicate_target"
}

func structureSourceTitleSimilarity(source, recognizedTitle string) float64 {
	recognizedTitle = strings.TrimSpace(recognizedTitle)
	if recognizedTitle == "" {
		return 0
	}
	clean := safeStructurePath(source)
	if clean == "" {
		return 0
	}
	parts := strings.Split(clean, "/")
	values := make([]string, 0, 4)
	if len(parts) > 0 {
		values = append(values, strings.TrimSuffix(parts[len(parts)-1], pathpkg.Ext(parts[len(parts)-1])))
	}
	for index := len(parts) - 2; index >= 0 && len(values) < 4; index-- {
		values = append(values, parts[index])
	}
	best := 0.0
	for _, value := range values {
		parsed := medialibrary.ParseMedia(value, value)
		for _, title := range []string{parsed.Title, parsed.SeriesTitle, value} {
			best = max(best, mediarecognition.TitleSimilarity(title, recognizedTitle, mediarecognition.BuiltInHanEquivalence))
		}
	}
	return best
}

func boundedStructureConflictSources(candidates []structurePlanCandidate, members []int) []string {
	sources := make([]string, 0, min(len(members), maxStructureConflictSourceSamples))
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		source := safeStructurePath(candidates[member].source)
		if source == "" {
			continue
		}
		if _, exists := seen[source]; exists {
			continue
		}
		seen[source] = struct{}{}
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool { return strings.ToLower(sources[i]) < strings.ToLower(sources[j]) })
	if len(sources) > maxStructureConflictSourceSamples {
		sources = sources[:maxStructureConflictSourceSamples]
	}
	return sources
}

func (p *StructurePlan) addIssue(issue StructureIssue) {
	p.IssueCount++
	issue.Title = safeMediaDisplayName(issue.Title)
	issue.ConflictSources = sanitizeStructureConflictSources(issue.ConflictSources)
	switch issue.Code {
	case "media_unrecognized":
		p.Unrecognized++
		p.Classifications.Unrecognized++
	case "missing_season_episode":
		p.Classifications.MissingEpisode++
	case "invalid_path":
		p.Classifications.InvalidPath++
	case "template_unavailable":
		p.Classifications.TemplateError++
	case "duplicate_target":
		p.Classifications.DuplicateTarget++
	case "recognition_suspect_conflict", "catalog_duplicate_conflict":
		p.Classifications.DuplicateTarget++
	case "sidecar_target_conflict":
		p.Classifications.SidecarConflict++
	}
	if len(p.Issues) < maxStructureIssueSamples {
		p.Issues = append(p.Issues, issue)
		if p.sampleCounts == nil {
			p.rebuildIssueSampleCounts()
		} else {
			p.sampleCounts[issue.Code]++
		}
		return
	}
	// Diagnostics persist only a bounded public sample. Balance that sample
	// across the issue classes that are actually present instead of letting a
	// large early class (commonly missing episodes) hide every later conflict.
	if p.sampleCounts == nil {
		p.rebuildIssueSampleCounts()
	}
	newCount := p.sampleCounts[issue.Code]
	largestCode, largestCount := "", 0
	for _, sampled := range p.Issues {
		count := p.sampleCounts[sampled.Code]
		if count > largestCount || (count == largestCount && (largestCode == "" || sampled.Code < largestCode)) {
			largestCode, largestCount = sampled.Code, count
		}
	}
	if largestCode == "" || largestCount <= newCount+1 {
		return
	}
	for index := len(p.Issues) - 1; index >= 0; index-- {
		if p.Issues[index].Code == largestCode {
			p.Issues[index] = issue
			p.sampleCounts[largestCode]--
			p.sampleCounts[issue.Code]++
			return
		}
	}
}

func sanitizeStructureConflictSources(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := make([]string, 0, min(len(values), maxStructureConflictSourceSamples))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = safeStructurePath(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) == maxStructureConflictSourceSamples {
			break
		}
	}
	return result
}

func (p *StructurePlan) rebuildIssueSampleCounts() {
	p.sampleCounts = make(map[string]int, len(p.Issues))
	for _, issue := range p.Issues {
		p.sampleCounts[issue.Code]++
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

func structureSidecarTargetIndexed(source string, index *structureAssociationIndex) (string, string) {
	if index == nil {
		return source, ""
	}
	sourceDir := pathpkg.Dir(source)
	sourceBase := strings.TrimSuffix(pathpkg.Base(source), pathpkg.Ext(source))
	extension := strings.ToLower(pathpkg.Ext(source))
	lowerSource := strings.ToLower(sourceBase)
	videoMatches := index.byDirectoryBase[sourceDir][lowerSource]
	if len(videoMatches) == 0 {
		for position := len(lowerSource) - 1; position > 0; position-- {
			if lowerSource[position] != '.' && lowerSource[position] != '-' {
				continue
			}
			if matches := index.byDirectoryBase[sourceDir][lowerSource[:position]]; len(matches) > 0 {
				videoMatches = matches
				break
			}
		}
	}
	for _, video := range videoMatches {
		videoSource, videoTarget := video.source, video.target
		videoBase := strings.TrimSuffix(pathpkg.Base(videoSource), pathpkg.Ext(videoSource))
		lowerVideo := strings.ToLower(videoBase)
		if strings.EqualFold(sourceBase, videoBase) || strings.HasPrefix(lowerSource, lowerVideo+".") || strings.HasPrefix(lowerSource, lowerVideo+"-") {
			suffix := sourceBase[len(videoBase):]
			targetBase := strings.TrimSuffix(pathpkg.Base(videoTarget), pathpkg.Ext(videoTarget))
			return pathpkg.Join(pathpkg.Dir(videoTarget), targetBase+suffix+extension), video.workKey
		}
	}
	// Generic poster/fanart/NFO files belong to the only work in their current
	// directory. Preserve their name while moving them beside that work.
	matchedWork := index.singleWorkByDirectory[sourceDir]
	if matchedWork == "" {
		return source, ""
	}
	directoryWorks := index.workKeysByDirectory[pathpkg.Dir(source)]
	if len(directoryWorks) != 1 {
		return source, ""
	}
	if _, sameWork := directoryWorks[matchedWork]; !sameWork {
		return source, ""
	}
	workRoot := index.workRoots[matchedWork]
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
