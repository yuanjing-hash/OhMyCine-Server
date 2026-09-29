package torznab

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/btrss"
)

const (
	jackettResultsPath = "/api/v2.0/indexers/all/results"
	maxJackettBytes    = 32 << 20
)

// Jackett's manual-search endpoint is different from its Torznab all-indexer
// feed. The latter can return a smaller or differently selected result set.
// Only a root or an explicit Jackett all-indexer URL is eligible; other
// Torznab/Prowlarr API paths retain their existing request contract.
func jackettManualPath(baseURL string) (string, bool) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || base == nil || base.Host == "" {
		return "", false
	}
	configuredPath := strings.TrimRight(base.Path, "/")
	if configuredPath == "" {
		return jackettResultsPath, true
	}
	for _, suffix := range []string{
		jackettResultsPath + "/torznab/api",
		jackettResultsPath + "/torznab",
	} {
		if strings.HasSuffix(configuredPath, suffix) {
			return strings.TrimSuffix(configuredPath, suffix) + jackettResultsPath, true
		}
	}
	return "", false
}

type jackettRelease struct {
	Title       string `json:"Title"`
	Link        string `json:"Link"`
	MagnetURI   string `json:"MagnetUri"`
	PublishDate string `json:"PublishDate"`
	Size        *int64 `json:"Size"`
	Seeders     *int64 `json:"Seeders"`
	Peers       *int64 `json:"Peers"`
	Grabs       *int64 `json:"Grabs"`
}

func (a *Adapter) searchJackett(ctx context.Context, config site.Config, query site.Query, keyword, manualPath string) (site.Page, bool, error) {
	if !validAPIKey(config.APIKey) {
		return site.Page{}, true, site.ErrAuthentication
	}
	client, base, err := a.clientFactory(config)
	if err != nil {
		return site.Page{}, true, site.ErrUnavailable
	}
	target := *base
	target.Path = manualPath
	values := url.Values{"apikey": {config.APIKey}, "query": {keyword}}
	switch strings.ToLower(query.MediaType) {
	case "movie":
		values.Set("Category[]", "2000")
	case "tv":
		values.Set("Category[]", "5000")
	}
	target.RawQuery = values.Encode()
	body, status, err := requestTarget(ctx, client, &target, "application/json", maxJackettBytes)
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return site.Page{}, false, nil
	}
	if err != nil {
		return site.Page{}, true, err
	}
	var payload struct {
		Results json.RawMessage `json:"Results"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return site.Page{}, true, site.ErrInvalidReply
	}
	results := bytes.TrimSpace(payload.Results)
	if len(results) == 0 || results[0] != '[' {
		return site.Page{}, true, site.ErrInvalidReply
	}
	var releases []jackettRelease
	if json.Unmarshal(results, &releases) != nil {
		return site.Page{}, true, site.ErrInvalidReply
	}
	start := (query.Page - 1) * searchPageSize
	end := min(start+searchPageSize, len(releases))
	page := site.Page{Page: query.Page, HasNext: query.Page < 20 && end < len(releases), Items: make([]site.Result, 0, searchPageSize)}
	if start >= len(releases) {
		return page, true, nil
	}
	for _, release := range releases[start:end] {
		title := cleanJackettText(release.Title, 512)
		if title == "" {
			page.Skipped++
			continue
		}
		identity := jackettSourceIdentity(config.BaseURL, release.Link, release.MagnetURI)
		if identity == "" {
			page.Skipped++
			continue
		}
		item := site.Result{TorrentID: identity, Title: title, Seeders: jackettCount(release.Seeders), Leechers: jackettCount(release.Peers), Completed: jackettCount(release.Grabs)}
		if release.Size != nil && *release.Size > 0 {
			item.SizeBytes = *release.Size
		}
		if published := jackettPublished(release.PublishDate); published != nil {
			item.Published = published
		}
		page.Items = append(page.Items, item)
	}
	return page, true, nil
}

func jackettSourceIdentity(baseURL, link, magnetURI string) string {
	if safeTorznabURL(baseURL, link) {
		// Jackett's proxy links can include its API key. Remove that key from
		// the server-private identity and supply the configured key at resolve.
		target, _ := url.Parse(strings.TrimSpace(link))
		values := target.Query()
		for key := range values {
			if strings.EqualFold(key, "apikey") || strings.EqualFold(key, "passkey") {
				values.Del(key)
			}
		}
		target.RawQuery = values.Encode()
		return btrss.EncodeIdentity("torrent", target.String())
	}
	if magnet, ok := btrss.NormalizeMagnet(magnetURI); ok {
		return btrss.EncodeIdentity("magnet", magnet)
	}
	return ""
}

func cleanJackettText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" || len([]rune(value)) > maxRunes || strings.ContainsRune(value, 0) {
		return ""
	}
	return value
}

func jackettCount(value *int64) *int {
	if value == nil || *value < 0 || *value > 1<<31-1 {
		return nil
	}
	count := int(*value)
	return &count
}

func jackettPublished(value string) *time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
	}
	return nil
}

func validAPIKey(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 2048 && !strings.ContainsAny(value, "\x00\r\n")
}
