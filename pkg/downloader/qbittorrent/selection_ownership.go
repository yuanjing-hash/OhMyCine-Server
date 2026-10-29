package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// Ordinary and selected operations use the same native aliases, including a
// hybrid's v1 and qB's shortened v2 ID. Full manifest identity is still checked
// separately by selectedState; these keys only serialize local operations.
func identityHashes(identity resourcecontent.Identity) []string {
	var hashes []string
	if identity.V1 != "" {
		hashes = append(hashes, identity.V1)
	}
	if identity.V2 != "" {
		hashes = append(hashes, identity.V2)
		if len(identity.V2) == 64 {
			hashes = append(hashes, identity.V2[:40])
		}
	}
	return hashes
}

func (c *Client) nativeLocks(ctx context.Context, aliases []string) (func(), error) {
	keys := make([]string, 0, len(aliases))
	seen := map[string]bool{}
	for _, alias := range aliases {
		key := c.baseURL + "\x00" + c.username + "\x00" + strings.ToLower(alias)
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	sort.Strings(keys)
	var releases []func()
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, key := range keys {
		unlock, err := acquireNativeLock(ctx, key)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, unlock)
	}
	return release, nil
}

func acquireNativeLock(ctx context.Context, key string) (func(), error) {
	selectiveLocks.Lock()
	entry := selectiveLocks.entries[key]
	if entry == nil {
		entry = &selectiveLock{token: make(chan struct{}, 1)}
		selectiveLocks.entries[key] = entry
	}
	entry.refs++
	selectiveLocks.Unlock()
	drop := func() {
		selectiveLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(selectiveLocks.entries, key)
		}
		selectiveLocks.Unlock()
	}
	select {
	case entry.token <- struct{}{}:
		return func() { <-entry.token; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, downloader.Error("downloader_request_cancelled", true, ctx.Err())
	}
}

func sourceIdentity(source downloader.Source) (resourcecontent.Identity, error) {
	if source.Kind == downloader.SourceTorrent {
		manifest, err := resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{MaxMetainfoBytes: downloader.MaxTorrentBytes})
		if err != nil {
			// Retain the old ordinary wire contract for metainfo that the strict
			// inspection parser cannot prove. It must pass the unknown-source
			// account-wide absence check, never bypass managed ownership.
			return resourcecontent.Identity{}, nil
		}
		return manifest.Identity, nil
	}
	u, err := url.Parse(source.URL)
	if err != nil {
		return resourcecontent.Identity{}, downloader.Error("downloader_source_invalid", false, err)
	}
	if !strings.EqualFold(u.Scheme, "magnet") {
		return resourcecontent.Identity{}, nil
	}
	// Identity extraction must not reject otherwise compatible ordinary tracker
	// hints or cause any source fetch. Only the original exact topics are parsed.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return resourcecontent.Identity{}, downloader.Error("downloader_source_invalid", false, err)
	}
	identityOnly := url.Values{"xt": query["xt"]}
	magnet, err := resourcecontent.ParseMagnet("magnet:?" + identityOnly.Encode())
	if err != nil {
		return resourcecontent.Identity{}, nil
	}
	return magnet.Identity, nil
}

func managedSelectionTags(tags string) bool {
	for _, tag := range strings.Split(tags, ",") {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if strings.HasPrefix(tag, "omc-sel-") || strings.HasPrefix(tag, "omc-group-") {
			return true
		}
	}
	return false
}

func (c *Client) torrentInfos(ctx context.Context, values url.Values, requireOwnershipFacts bool) ([]torrentInfo, error) {
	var infos []torrentInfo
	err := c.session(ctx, func(cookie string) error {
		body, status, err := c.do(ctx, cookie, http.MethodGet, "/api/v2/torrents/info?"+values.Encode(), "", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(status)
		}
		if requireOwnershipFacts {
			// Only an explicit complete array with native identity and tag facts
			// can prove absence. null, partial rows and wrong types are not an
			// empty native account and must never authorize a native creation.
			var facts []struct {
				Hash *string `json:"hash"`
				Tags *string `json:"tags"`
			}
			if err = json.Unmarshal(body, &facts); err != nil || facts == nil || len(facts) > maxManifestFiles {
				return downloader.Error("downloader_response_invalid", false, err)
			}
			for _, fact := range facts {
				if fact.Hash == nil || !validTorrentHash(*fact.Hash) || fact.Tags == nil {
					return downloader.Error("downloader_response_invalid", false, nil)
				}
			}
		}
		if err = json.Unmarshal(body, &infos); err != nil || len(infos) > maxManifestFiles {
			return downloader.Error("downloader_response_invalid", false, err)
		}
		return nil
	})
	return infos, err
}

func (c *Client) rejectManagedSource(ctx context.Context, identity resourcecontent.Identity, selected bool) error {
	hashes := identityHashes(identity)
	values := url.Values{}
	if len(hashes) != 0 {
		values.Set("hashes", strings.Join(hashes, "|"))
	} else {
		// The provider URL has no trustworthy hash. Scan a bounded native view
		// while admission is serialized; do not submit when managed ownership
		// exists or when the bounded response cannot prove its absence.
		values.Set("limit", "2049")
	}
	infos, err := c.torrentInfos(ctx, values, true)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if managedSelectionTags(info.Tags) || selected {
			if len(hashes) == 0 {
				return downloader.Error("downloader_selection_identity_required", false, nil)
			}
			return downloader.Error("downloader_selection_owner_conflict", false, nil)
		}
	}
	return nil
}

// Returns a locked ordinary native ID. A direct hash is re-read under the
// same aliases used by selected writes; a tag cannot become control authority
// merely by resolving to a managed resource.
func (c *Client) ordinaryHash(ctx context.Context, id string) (string, func(), error) {
	hash := id
	if strings.HasPrefix(id, "tag:") {
		task, err := c.Get(ctx, id)
		if err != nil {
			return "", nil, err
		}
		hash = task.ID
	}
	aliases := []string{hash}
	if len(hash) == 64 && validTorrentHash(hash) {
		aliases = append(aliases, hash[:40])
	}
	release, err := c.nativeLocks(ctx, aliases)
	if err != nil {
		return "", nil, err
	}
	task, err := c.Get(ctx, id)
	if err != nil {
		// Preserve the ordinary direct-hash no-op control contract for absent
		// tasks. A selected creation of this same hash cannot race the action.
		if code, retryable := downloader.ErrorInfo(err); code == "downloader_task_not_found" && !retryable && !strings.HasPrefix(id, "tag:") {
			return hash, release, nil
		}
		release()
		return "", nil, err
	}
	if !strings.EqualFold(task.ID, hash) && !(len(hash) == 64 && strings.EqualFold(task.ID, hash[:40])) {
		release()
		return "", nil, downloader.Error("downloader_selection_owner_conflict", false, nil)
	}
	return task.ID, release, nil
}
