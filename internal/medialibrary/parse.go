package medialibrary

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// The marker itself is matched separately from its boundaries so adjacent
	// CJK work/release text is preserved (for example 中文名S02E02更多资源).
	// parseExplicitSxxExx rejects only ASCII word embedding such as
	// HOUSE01E02B, which is not an explicit filename marker boundary.
	sxxexxPattern      = regexp.MustCompile(`(?i)S\s*0*([0-9]{1,2})\s*E\s*0*([0-9]{1,5})`)
	oneXEpisodePattern = regexp.MustCompile(`(?i)(?:^|[. _-])0*([0-9]{1,2})x0*([0-9]{1,5})(?:[. _-]|$)`)
	episodeOnlyPattern = regexp.MustCompile(`(?i)(?:^|[. _-])(?:EP?|Episode)\s*0*([0-9]{1,5})(?:[. _-]|$)`)
	// A bare trailing number is only trusted when a following bracket carries
	// release metadata. This covers common anime/PT names such as
	// "Work - 09 [language] (WEB ...)" without turning titles such as
	// "Catch-22 (2019)" into episodes.
	trailingBracketEpisodePattern = regexp.MustCompile(`(?i)-\s*0*([0-9]{1,5})\s*(\[[^\]]{1,128}\]|【[^】]{1,128}】)`)
	chineseEpisodePattern         = regexp.MustCompile(`第\s*([0-9一二三四五六七八九十百两〇零]+)\s*[集话話]`)
	seasonFolderPattern           = regexp.MustCompile(`(?i)^(?:Season|Seanson|S)\s*0*([0-9]{1,2})$`)
	specialsFolderPattern         = regexp.MustCompile(`(?i)^(?:Specials?|Extras?)$`)
	chineseSeasonPattern          = regexp.MustCompile(`^第\s*([0-9一二三四五六七八九十百两〇零]+)\s*季$`)
	seasonScopedEpisodePattern    = regexp.MustCompile(`(?:^|[. _-])0*([0-9]{1,4})\s*$`)
	yearPattern                   = regexp.MustCompile(`(?:^|[\s._(（\[【-])((?:18|19|20|21)[0-9]{2})(?:$|[\s._)）\]】-])`)
	bracketNoisePattern           = regexp.MustCompile(`\[[^\]]+\]|\([^)]*\)|【[^】]+】|（[^）]*）`)
	technicalTokenPattern         = regexp.MustCompile(`(?i)(?:^|[. _-])(?:2160p|1080p|720p|576p|480p|UHD|BluRay|BDRip|WEB[- .]?DL|WEBRip|HDTV|DVDRip|REMUX|x264|x265|H\.?264|H\.?265|HEVC|AV1|AAC|DTS(?:-HD)?|TrueHD|Atmos|DDP?5(?:\.1)?|HDR10?|DoVi|10bit|8bit|Proper|Repack)(?:$|[. _-])`)
	spacePattern                  = regexp.MustCompile(`\s+`)
)

var reservedStructureSegments = map[string]struct{}{
	"download": {}, "downloads": {}, "complete": {}, "completed": {}, "incoming": {},
	"temp": {}, "tmp": {}, "media": {}, "library": {}, "libraries": {}, "movie": {},
	"movies": {}, "film": {}, "films": {}, "tv": {}, "tvseries": {}, "series": {},
	"show": {}, "shows": {}, "video": {}, "videos": {}, "下载": {}, "下载完成": {},
	"未整理": {}, "待整理": {}, "临时": {}, "影视": {}, "影视库": {}, "媒体库": {},
	"片库": {}, "视频": {}, "电影": {}, "剧集": {}, "电视剧": {},
}

type ParsedMedia struct {
	MediaType   string
	Title       string
	SeriesTitle string
	Year        *int
	Season      *int
	Episode     *int
}

func ParseMedia(name, providerPath string) ParsedMedia {
	segments := splitMediaPath(providerPath)
	fileName := name
	if fileName == "" && len(segments) > 0 {
		fileName = segments[len(segments)-1]
	}
	parents := segments
	if len(parents) > 0 {
		parents = parents[:len(parents)-1]
	}
	stem := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	episodeTitle, season, episode := parseEpisodeStem(stem)
	seasonIndex, folderSeason := findSeasonFolder(parents)
	if season == nil {
		season = folderSeason
	}
	if episode == nil && seasonIndex >= 1 && folderSeason != nil {
		if scopedTitle, scopedEpisode := parseSeasonScopedTrailingEpisode(stem); scopedEpisode != nil {
			episodeTitle, episode = scopedTitle, scopedEpisode
		}
	}

	if episode != nil {
		if season == nil {
			defaultSeason := 1
			season = &defaultSeason
		}
		seriesTitle := ""
		var year *int
		if seasonIndex >= 1 {
			seriesTitle, year = parseWorkIdentity(parents[seasonIndex-1])
		}
		if seriesTitle == "" {
			seriesTitle, year = parseWorkIdentity(episodeTitle)
		}
		if seriesTitle == "" && len(parents) > 0 {
			seriesTitle, year = parseWorkIdentity(parents[len(parents)-1])
		}
		if seriesTitle == "" {
			seriesTitle = episodeTitle
		}
		if seriesTitle == "" {
			seriesTitle = cleanMediaTitle(stem)
		}
		return ParsedMedia{MediaType: "tv", Title: seriesTitle, SeriesTitle: seriesTitle, Year: year, Season: season, Episode: episode}
	}

	if seasonIndex >= 1 {
		seriesTitle, year := parseWorkIdentity(parents[seasonIndex-1])
		if seriesTitle != "" {
			return ParsedMedia{MediaType: "tv", Title: seriesTitle, SeriesTitle: seriesTitle, Year: year, Season: season}
		}
	}

	title, year := titleYearFromFolder(parents)
	if title == "" {
		title = cleanMediaTitle(stem)
		year = extractMediaYear(stem)
	}
	if title == "" {
		title = strings.TrimSpace(stem)
	}
	return ParsedMedia{MediaType: "movie", Title: title, Year: year}
}

func WorkKey(parsed ParsedMedia, identity string) string {
	kind := "movie"
	keyMaterial := strings.TrimSpace(identity)
	if parsed.MediaType == "tv" {
		kind = "series"
		keyMaterial = normalizeTitleKey(parsed.SeriesTitle)
	}
	if keyMaterial == "" {
		kind = "file"
		keyMaterial = strings.TrimSpace(parsed.Title)
	}
	digest := sha256.Sum256([]byte(kind + "\x00" + keyMaterial))
	return kind + ":" + hex.EncodeToString(digest[:16])
}

func parseEpisodeStem(stem string) (string, *int, *int) {
	if start, end, season, episode, ok := parseExplicitSxxExx(stem); ok {
		return cleanMediaTitle(stem[:start] + " " + stem[end:]), season, episode
	}
	if match := oneXEpisodePattern.FindStringSubmatch(stem); len(match) == 3 {
		season, episode := atoiPointer(match[1]), atoiPointer(match[2])
		return cleanMediaTitle(oneXEpisodePattern.ReplaceAllString(stem, " ")), season, episode
	}
	if match := chineseEpisodePattern.FindStringSubmatch(stem); len(match) == 2 {
		if number, ok := parseNumberText(match[1]); ok {
			return cleanMediaTitle(chineseEpisodePattern.ReplaceAllString(stem, " ")), nil, &number
		}
	}
	if match := episodeOnlyPattern.FindStringSubmatch(stem); len(match) == 2 {
		episode := atoiPointer(match[1])
		return cleanMediaTitle(episodeOnlyPattern.ReplaceAllString(stem, " ")), nil, episode
	}
	if match := trailingBracketEpisodePattern.FindStringSubmatch(stem); len(match) == 3 && trailingEpisodeBracketHasReleaseEvidence(match[2]) {
		episode := atoiPointer(match[1])
		return cleanMediaTitle(trailingBracketEpisodePattern.ReplaceAllString(stem, " ")), nil, episode
	}
	return cleanMediaTitle(stem), nil, nil
}

func parseExplicitSxxExx(stem string) (int, int, *int, *int, bool) {
	searchOffset := 0
	for searchOffset < len(stem) {
		match := sxxexxPattern.FindStringSubmatchIndex(stem[searchOffset:])
		if len(match) != 6 {
			break
		}
		start, end := searchOffset+match[0], searchOffset+match[1]
		if explicitEpisodeBoundaryAllowed(stem, start, end) {
			seasonText := stem[searchOffset+match[2] : searchOffset+match[3]]
			episodeText := stem[searchOffset+match[4] : searchOffset+match[5]]
			return start, end, atoiPointer(seasonText), atoiPointer(episodeText), true
		}
		searchOffset = end
	}
	return 0, 0, nil, nil, false
}

func explicitEpisodeBoundaryAllowed(value string, start, end int) bool {
	if start > 0 {
		before, _ := utf8.DecodeLastRuneInString(value[:start])
		if isASCIIAlphaNumeric(before) {
			return false
		}
	}
	if end < len(value) {
		after, _ := utf8.DecodeRuneInString(value[end:])
		if isASCIIAlphaNumeric(after) {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(value rune) bool {
	return value <= unicode.MaxASCII && (unicode.IsLetter(value) || unicode.IsDigit(value))
}

func parseSeasonScopedTrailingEpisode(stem string) (string, *int) {
	candidate := bracketNoisePattern.ReplaceAllString(stem, " ")
	for technicalTokenPattern.MatchString(candidate) {
		candidate = technicalTokenPattern.ReplaceAllString(candidate, " ")
	}
	match := seasonScopedEpisodePattern.FindStringSubmatch(strings.TrimSpace(candidate))
	if len(match) != 2 {
		return cleanMediaTitle(stem), nil
	}
	episode, err := strconv.Atoi(match[1])
	if err != nil || episode <= 0 || (episode >= 1888 && episode <= 2200) {
		return cleanMediaTitle(stem), nil
	}
	title := seasonScopedEpisodePattern.ReplaceAllString(candidate, " ")
	return cleanMediaTitle(title), &episode
}

func trailingEpisodeBracketHasReleaseEvidence(value string) bool {
	return technicalTokenPattern.MatchString(value) ||
		strings.ContainsAny(strings.ToLower(value), "字幕语語粤粵国國英日韩韓音轨軌")
}

func findSeasonFolder(segments []string) (int, *int) {
	for index := len(segments) - 1; index >= 0; index-- {
		if season, ok := seasonFolderNumber(segments[index]); ok {
			return index, season
		}
	}
	return -1, nil
}

func seasonFolderNumber(value string) (*int, bool) {
	value = strings.TrimSpace(value)
	if match := seasonFolderPattern.FindStringSubmatch(value); len(match) == 2 {
		return atoiPointer(match[1]), true
	}
	if match := chineseSeasonPattern.FindStringSubmatch(value); len(match) == 2 {
		if number, ok := parseNumberText(match[1]); ok {
			return &number, true
		}
	}
	if specialsFolderPattern.MatchString(value) {
		season := 0
		return &season, true
	}
	return nil, false
}

// IsSeasonFolderName reports whether a single safe path segment carries
// explicit season context. Services use it only after reducing a path to its
// basename, so no provider path crosses this parser boundary.
func IsSeasonFolderName(value string) bool {
	_, ok := seasonFolderNumber(value)
	return ok
}

func titleYearFromFolder(segments []string) (string, *int) {
	for index := len(segments) - 1; index >= 0; index-- {
		if yearPattern.MatchString(segments[index]) {
			if title, year := parseWorkIdentity(segments[index]); title != "" {
				return title, year
			}
		}
	}
	return "", nil
}

func parseWorkIdentity(value string) (string, *int) {
	return cleanWorkTitle(value), extractMediaYear(value)
}

func extractMediaYear(value string) *int {
	match := yearPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return nil
	}
	year, err := strconv.Atoi(match[1])
	if err != nil || year < 1888 || year > 2200 {
		return nil
	}
	return &year
}

func cleanMediaTitle(value string) string {
	value = bracketNoisePattern.ReplaceAllString(value, " ")
	value = yearPattern.ReplaceAllString(value, " ")
	for technicalTokenPattern.MatchString(value) {
		value = technicalTokenPattern.ReplaceAllString(value, " ")
	}
	value = strings.NewReplacer(".", " ", "_", " ", "—", " ", "-", " ").Replace(value)
	return strings.TrimSpace(spacePattern.ReplaceAllString(value, " "))
}

func cleanWorkTitle(value string) string {
	title := cleanMediaTitle(value)
	if title == "" {
		return ""
	}
	if match := seasonFolderPattern.FindStringSubmatch(strings.TrimSpace(value)); len(match) == 2 {
		return ""
	}
	if match := chineseSeasonPattern.FindStringSubmatch(strings.TrimSpace(value)); len(match) == 2 {
		return ""
	}
	if _, reserved := reservedStructureSegments[normalizeStructureKey(title)]; reserved {
		return ""
	}
	return title
}

func normalizeTitleKey(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(cleanMediaTitle(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func normalizeStructureKey(value string) string {
	return normalizeTitleKey(value)
}

func splitMediaPath(value string) []string {
	parts := strings.Split(strings.ReplaceAll(value, "\\", "/"), "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			segments = append(segments, trimmed)
		}
	}
	return segments
}

func atoiPointer(value string) *int {
	number, _ := strconv.Atoi(value)
	return &number
}

func parseNumberText(value string) (int, bool) {
	if number, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return number, true
	}
	digits := map[rune]int{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 1 {
		if number, ok := digits[runes[0]]; ok {
			return number, true
		}
		if runes[0] == '十' {
			return 10, true
		}
	}
	for index, r := range runes {
		if r != '十' {
			continue
		}
		tens := 1
		if index > 0 {
			var ok bool
			tens, ok = digits[runes[index-1]]
			if !ok {
				return 0, false
			}
		}
		ones := 0
		if index+1 < len(runes) {
			var ok bool
			ones, ok = digits[runes[index+1]]
			if !ok {
				return 0, false
			}
		}
		return tens*10 + ones, true
	}
	return 0, false
}
