package services

import "encoding/json"

const (
	SearchSnapshotEventBytes = 384 << 10
	SearchStreamBytes        = 12 << 20
	SearchStreamInterimBytes = 8 << 20
	searchFinalSnapshotBytes = (4 << 20) - (256 << 10)
)

type detailedSearchKey struct{}

type searchSnapshotBudget struct {
	latest                               map[uint]SiteSearchGroup
	reserved, interim                    int
	eventLimit, finalLimit, interimLimit int
}

func newSearchSnapshotBudget() *searchSnapshotBudget {
	return &searchSnapshotBudget{latest: map[uint]SiteSearchGroup{}, eventLimit: SearchSnapshotEventBytes - 1024, finalLimit: searchFinalSnapshotBytes, interimLimit: SearchStreamInterimBytes}
}

// Admission preserves every previously accepted row and token. Replacements
// fit before publication; old operations retain their immutable source claim.
func (b *searchSnapshotBudget) admit(candidate SiteSearchGroup) (SiteSearchGroup, bool) {
	previous := b.latest[candidate.SiteID]
	oldJSON, _ := json.Marshal(previous)
	if previous.SiteID == 0 {
		oldJSON = nil
	}
	if previous.IncompleteReason == CodeSearchResultLimit {
		candidate.Partial, candidate.IncompleteReason = true, CodeSearchResultLimit
	}
	candidateKeys := make(map[string]struct{}, len(candidate.Items))
	for _, item := range candidate.Items {
		candidateKeys[searchDisplayKey(item)] = struct{}{}
	}
	retainsPrevious := true
	for _, item := range previous.Items {
		if _, ok := candidateKeys[searchDisplayKey(item)]; !ok {
			retainsPrevious = false
			break
		}
	}
	// The ordinary batch fits in one marshal. Only the boundary case needs
	// row-by-row admission; keep normal alias processing linear in row count.
	encodedCandidate, candidateErr := json.Marshal(candidate)
	if retainsPrevious && candidateErr == nil && len(encodedCandidate) <= b.eventLimit && b.reserved-len(oldJSON)+len(encodedCandidate) <= b.finalLimit {
		result := cloneSearchGroup(candidate)
		if previous.IncompleteReason == CodeSearchResultLimit {
			result.Partial, result.IncompleteReason = true, CodeSearchResultLimit
		}
		encoded, _ := json.Marshal(result)
		b.reserved += len(encoded) - len(oldJSON)
		b.latest[result.SiteID] = cloneSearchGroup(result)
		return result, true
	}
	result := cloneSearchGroup(candidate)
	result.Items = append([]SiteSearchResult{}, previous.Items...)
	keys := map[string]int{}
	for i, item := range result.Items {
		keys[searchDisplayKey(item)] = i
	}
	limited := false
	for _, item := range candidate.Items {
		key := searchDisplayKey(item)
		index, exists := keys[key]
		if exists && result.Items[index].Token == item.Token {
			continue
		}
		var old SiteSearchResult
		if exists {
			old = result.Items[index]
			result.Items[index] = item
		} else {
			result.Items = append(result.Items, item)
		}
		encoded, err := json.Marshal(result)
		if err != nil || len(encoded) > b.eventLimit || b.reserved-len(oldJSON)+len(encoded) > b.finalLimit {
			limited = true
			if exists {
				result.Items[index] = old
			} else {
				result.Items = result.Items[:len(result.Items)-1]
			}
			continue
		}
		if !exists {
			keys[key] = len(result.Items) - 1
		}
	}
	if limited || previous.IncompleteReason == CodeSearchResultLimit {
		result.Partial, result.IncompleteReason = true, CodeSearchResultLimit
	}
	encoded, _ := json.Marshal(result)
	b.reserved += len(encoded) - len(oldJSON)
	b.latest[result.SiteID] = cloneSearchGroup(result)
	return result, !limited
}

func searchDisplayKey(item SiteSearchResult) string {
	if item.ResourceFingerprint != "" {
		return item.ResourceFingerprint
	}
	return item.Token
}

func mergeSearchItems(previous, current []SiteSearchResult) []SiteSearchResult {
	result := append([]SiteSearchResult{}, previous...)
	keys := make(map[string]int, len(result))
	for index, item := range result {
		keys[searchDisplayKey(item)] = index
	}
	for _, item := range current {
		key := searchDisplayKey(item)
		if index, ok := keys[key]; ok {
			result[index] = item
		} else {
			keys[key] = len(result)
			result = append(result, item)
		}
	}
	if len(result) > 0 && result[0].MatchedName != "" {
		sortIdentitySearchItems(result)
	}
	return result
}

func cloneSearchGroup(group SiteSearchGroup) SiteSearchGroup {
	copy := group
	if group.Complete != nil {
		complete := *group.Complete
		copy.Complete = &complete
	}
	copy.Items = make([]SiteSearchResult, len(group.Items))
	for i, item := range group.Items {
		copy.Items[i] = item
		copy.Items[i].Tags = append([]string(nil), item.Tags...)
		copy.Items[i].Seeders = cloneInt(item.Seeders)
		copy.Items[i].Leechers = cloneInt(item.Leechers)
		copy.Items[i].Completed = cloneInt(item.Completed)
		if item.Published != nil {
			published := *item.Published
			copy.Items[i].Published = &published
		}
		if item.ShareValidation != nil {
			validation := *item.ShareValidation
			copy.Items[i].ShareValidation = &validation
		}
	}
	return copy
}
