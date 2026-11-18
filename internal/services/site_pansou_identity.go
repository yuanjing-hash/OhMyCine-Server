package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"golang.org/x/text/unicode/norm"
)

const panSouQueryVariant = "pansou_query_variant"

// Some PanSou posts omit both the subtitle and an internal 又 in the title.
// Reserve one query for that compact form without promoting it to a TMDB name.
func panSouIdentityQueries(names []tmdb.SearchName) []tmdb.SearchName {
	if len(names) == 0 || names[0].Kind != "localized" || names[0].Locale != "zh-CN" {
		return names
	}
	prefix, subtitle, hasSubtitle := strings.Cut(norm.NFKC.String(names[0].Value), ":")
	prefix = strings.TrimSpace(prefix)
	if !hasSubtitle || strings.TrimSpace(subtitle) == "" || strings.ContainsFunc(prefix, func(r rune) bool { return !unicode.Is(unicode.Han, r) }) {
		return names
	}
	left, right, hasConjunction := strings.Cut(prefix, "又")
	if !hasConjunction || strings.Contains(right, "又") || utf8.RuneCountInString(left) < 2 || utf8.RuneCountInString(right) < 2 {
		return names
	}
	variant := left + right
	for _, name := range names {
		if identitySearchKey(name.Value) == variant {
			return names
		}
	}
	kept := min(len(names), tmdb.DefaultIdentitySearchNameLimit-1)
	insertion := min(2, kept)
	queries := make([]tmdb.SearchName, 0, kept+1)
	queries = append(queries, names[:insertion]...)
	queries = append(queries, tmdb.SearchName{Value: variant, Locale: "zh-CN", Kind: panSouQueryVariant})
	return append(queries, names[insertion:kept]...)
}

type identitySearchSnapshot struct {
	key     string
	expires time.Time
	group   SiteSearchGroup
}

func identitySnapshotPage(group SiteSearchGroup, page int, queryAttempted bool) SiteSearchGroup {
	items := group.Items
	group.Page = page
	// This field describes the current request. A local cached projection
	// must not inherit the upstream query performed to create the snapshot.
	group.QueryAttempted = queryAttempted
	group.Items = []SiteSearchResult{}
	group.HasNext = false
	start := (page - 1) * 50
	if start >= len(items) {
		return group
	}
	end := min(start+50, len(items))
	group.Items = append(group.Items, items[start:end]...)
	group.HasNext = end < len(items) && page < 20
	return group
}

// PanSou's upstream pagination is a local projection of one JSON batch. Freeze
// the complete, deduplicated TMDB alias projection too, so aliases and sorting
// cannot move previously delivered rows across pages or repeat full searches.
func (s *SiteService) searchPanSouIdentitySite(ctx context.Context, actor Actor, input MediaIdentitySearchInput, verified tmdb.Match, names []tmdb.SearchName, record models.Site) SiteSearchGroup {
	failure := SiteSearchGroup{SiteID: record.ID, SiteName: record.Name, SiteType: "cloud_share", Status: "error", Page: input.Page, Items: []SiteSearchResult{}}
	keyInput := input
	keyInput.Page = 0
	keyInput.SnapshotID = ""
	keyInput.Refresh = false
	keyInput.ProgressVersion = 0 // Transport progress does not change result identity.
	config, configErr := s.config(record)
	if configErr != nil {
		failure.ErrorCode = CodeSiteCredentialInvalid
		return failure
	}
	policy, policyErr := s.panSouQuery(actor, record, config, SiteSearchInput{TypePolicy: input.TypePolicy, CloudTypes: input.CloudTypes, ClientCapabilities: input.ClientCapabilities})
	if policyErr != nil {
		failure.ErrorCode = ErrorCode(policyErr)
		return failure
	}
	queries := panSouIdentityQueries(names)
	raw, _ := json.Marshal(struct {
		Actor          string
		Site           uint
		Revision       uint64
		Input          MediaIdentitySearchInput
		Names          []tmdb.SearchName
		Queries        []tmdb.SearchName
		Policy         string
		Types          []string
		ReceivingScope string
		ResultPolicy   string
	}{discoveryAuthorizationScope(actor), record.ID, record.Revision, keyInput, names, queries, policy.TypePolicy, policy.CloudTypes, policy.ScopeKey, input.resultPolicy})
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:])
	s.vaultMu.Lock()
	for id, entry := range s.identitySnapshots {
		if !entry.expires.After(s.now()) {
			delete(s.identitySnapshots, id)
		}
	}
	if !input.Refresh {
		if input.SnapshotID != "" {
			entry, ok := s.identitySnapshots[input.SnapshotID]
			if !ok || entry.key != key {
				s.vaultMu.Unlock()
				failure.ErrorCode = CodeSiteResultExpired
				return failure
			}
			s.vaultMu.Unlock()
			return identitySnapshotPage(entry.group, input.Page, false)
		}
		for _, entry := range s.identitySnapshots {
			if entry.key == key {
				s.vaultMu.Unlock()
				return identitySnapshotPage(entry.group, input.Page, false)
			}
		}
	}
	s.vaultMu.Unlock()
	if input.Page > 1 {
		failure.ErrorCode = CodeSiteResultExpired
		return failure
	}
	var all *SiteSearchGroup
	seen := map[string]int{}
	for page := 1; page <= 20; page++ {
		partInput := input
		partInput.Page = page
		partInput.SnapshotID = ""
		partInput.Refresh = input.Refresh && page == 1
		part := s.searchMediaIdentitySitePage(ctx, actor, partInput, verified, names, queries, record)
		if all == nil {
			copy := part
			copy.Items = []SiteSearchResult{}
			copy.SnapshotID = ""
			copy.SnapshotExpiresAt = nil
			all = &copy
		} else {
			all.Skipped += part.Skipped
			all.Partial = all.Partial || part.Partial
			all.Truncated = all.Truncated || part.Truncated
			all.ErrorCount += part.ErrorCount
			if part.Status != "success" {
				all.Partial = true
			}
		}
		for _, item := range part.Items {
			if index, ok := seen[item.ResourceFingerprint]; ok {
				if preferIdentityObservation(input, item, all.Items[index]) {
					s.deleteClaim(all.Items[index].Token)
					all.Items[index] = item
				} else {
					s.deleteClaim(item.Token)
				}
				continue
			}
			if len(all.Items) >= 1000 {
				s.deleteClaim(item.Token)
				all.Truncated = true
				all.Partial = true
				continue
			}
			seen[item.ResourceFingerprint] = len(all.Items)
			all.Items = append(all.Items, item)
		}
		if !part.HasNext || ctx.Err() != nil || len(all.Items) >= 1000 {
			if part.HasNext {
				all.Truncated = true
				all.Partial = true
			}
			break
		}
	}
	if all == nil {
		return failure
	}
	sortIdentitySearchItems(all.Items, input.preferShares)
	if all.Truncated {
		all.Warnings = append(all.Warnings, sitepkg.SearchWarning{Code: "pansou_identity_snapshot_truncated", Message: "作品搜索已达到本批1000条上限，请缩小搜索范围"})
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		failure.ErrorCode = CodeSiteUnavailable
		return failure
	}
	all.SnapshotID = hex.EncodeToString(token[:])
	expires := s.now().Add(ptResultTTL)
	all.SnapshotExpiresAt = &expires
	s.vaultMu.Lock()
	if s.identitySnapshots == nil {
		s.identitySnapshots = map[string]identitySearchSnapshot{}
	}
	for id, entry := range s.identitySnapshots {
		if entry.key == key {
			delete(s.identitySnapshots, id)
		}
	}
	if len(s.identitySnapshots) >= 32 {
		var oldestID string
		var oldest time.Time
		for id, entry := range s.identitySnapshots {
			if oldestID == "" || entry.expires.Before(oldest) {
				oldestID = id
				oldest = entry.expires
			}
		}
		delete(s.identitySnapshots, oldestID)
	}
	s.identitySnapshots[all.SnapshotID] = identitySearchSnapshot{key, expires, *all}
	s.vaultMu.Unlock()
	return identitySnapshotPage(*all, input.Page, all.QueryAttempted)
}
