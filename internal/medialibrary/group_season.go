package medialibrary

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	seasonReleaseMarker      = regexp.MustCompile(`(?i)(?:Season[ ._-]*|S\s*)0*([0-9]{1,2})|第\s*([0-9一二三四五六七八九十百两〇零]+)\s*季`)
	seasonReleaseRange       = regexp.MustCompile(`^\s*[-~～至到]\s*[0-9]`)
	numericWorkDirectory     = regexp.MustCompile(`^\s*([0-9]{4})\s*[\(\[（【]\s*((?:18|19|20|21)[0-9]{2})\s*[\)\]）】]\s*$`)
	numericSeasonTitlePrefix = regexp.MustCompile(`^[\s._-]*([0-9]{4})[\s._-]*$`)
)

// A release-named season is structural only with corroborating episode facts
// and the same title in the immediate outer work directory. Do not reinterpret
// download packages, arbitrary ancestors, aliases or movie titles as seasons.
// Exact Season/Specials folders retain their separate established parser path.
func isWorkSeasonReleaseDirectory(directory, outer string, parsed ParsedMedia) bool {
	if parsed.MediaType != "tv" || parsed.Season == nil || parsed.Episode == nil {
		return false
	}
	if _, _, episode := parseEpisodeStem(directory); episode != nil {
		return false
	}
	var marker []int
	for _, candidate := range seasonReleaseMarker.FindAllStringSubmatchIndex(directory, -1) {
		if !explicitEpisodeBoundaryAllowed(directory, candidate[0], candidate[1]) {
			continue
		}
		if marker != nil {
			return false
		}
		marker = candidate
	}
	if marker == nil || seasonReleaseRange.MatchString(directory[marker[1]:]) {
		return false
	}
	start, end := marker[2], marker[3]
	if start < 0 {
		start, end = marker[4], marker[5]
	}
	season, ok := parseNumberText(directory[start:end])
	if !ok || season != *parsed.Season {
		return false
	}
	parentTitle := cleanWorkTitle(outer)
	prefix := strings.TrimSpace(directory[:marker[0]])
	if parentKey, prefixKey := normalizeTitleKey(parentTitle), normalizeTitleKey(prefix); parentKey != "" && prefixKey != "" {
		return prefixKey == parentKey
	}
	// A legitimate four-digit work title is otherwise indistinguishable from a
	// year to the general title cleaner. Recover it only from the narrow form
	// "numeric title (independent premiere year)" and require the rich season
	// directory prefix to be that exact same numeric token. A bare year/category
	// ancestor therefore cannot become a work anchor.
	outerMatch := numericWorkDirectory.FindStringSubmatch(outer)
	prefixMatch := numericSeasonTitlePrefix.FindStringSubmatch(prefix)
	if len(outerMatch) != 3 || len(prefixMatch) != 2 || outerMatch[1] != prefixMatch[1] {
		return false
	}
	premiereYear, err := strconv.Atoi(outerMatch[2])
	return err == nil && premiereYear >= 1888 && premiereYear <= 2200
}
