package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type identitySearchSnapshot struct {
	key     string
	expires time.Time
	group   SiteSearchGroup
}

func identitySnapshotPage(group SiteSearchGroup, page int) SiteSearchGroup {
	items := group.Items
	group.Page = page
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
	raw, _ := json.Marshal(struct {
		Actor    string
		Site     uint
		Revision uint64
		Input    MediaIdentitySearchInput
		Names    []tmdb.SearchName
		Policy   string
		Types    []string
	}{discoveryAuthorizationScope(actor), record.ID, record.Revision, keyInput, names, policy.TypePolicy, policy.CloudTypes})
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
			return identitySnapshotPage(entry.group, input.Page)
		}
		for _, entry := range s.identitySnapshots {
			if entry.key == key {
				s.vaultMu.Unlock()
				return identitySnapshotPage(entry.group, input.Page)
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
		part := s.searchMediaIdentitySitePage(ctx, actor, partInput, verified, names, record)
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
			if _, ok := seen[item.ResourceFingerprint]; ok {
				s.deleteClaim(item.Token)
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
	sort.SliceStable(all.Items, func(i, j int) bool {
		a, b := all.Items[i], all.Items[j]
		if intValue(a.Seeders) != intValue(b.Seeders) {
			return intValue(a.Seeders) > intValue(b.Seeders)
		}
		if compared := compareOptionalTime(a.Published, b.Published); compared != 0 {
			return compared > 0
		}
		if a.SizeBytes != b.SizeBytes {
			return a.SizeBytes > b.SizeBytes
		}
		return a.Title < b.Title
	})
	if all.Truncated {
		all.Warnings = append(all.Warnings, sitepkg.SearchWarning{Code: "pansou_identity_snapshot_truncated", Message: "作品搜索已达到本批1000条上限，请缩小搜索范围"})
	}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		failure.ErrorCode = CodeSiteUnavailable
		return failure
	}
	all.SnapshotID = hex.EncodeToString(token[:])
	expires := s.now().Add(10 * time.Minute)
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
	return identitySnapshotPage(*all, input.Page)
}
