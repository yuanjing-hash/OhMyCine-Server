package torznab

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/btrss"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/rssfeed"
)

const (
	Kind            = "torznab"
	maxXMLBytes     = 4 << 20
	maxTorrentBytes = 4 << 20
)

type Adapter struct {
	clientFactory func(site.Config) (*http.Client, *url.URL, error)
}

func New() *Adapter { return &Adapter{clientFactory: controlledClient} }

func NewForTest(client *http.Client, baseURL string) *Adapter {
	return &Adapter{clientFactory: func(site.Config) (*http.Client, *url.URL, error) {
		base, err := url.Parse(strings.TrimRight(baseURL, "/"))
		return client, base, err
	}}
}

func (a *Adapter) Kind() string { return Kind }

func (a *Adapter) Test(ctx context.Context, config site.Config) (site.Health, error) {
	body, err := a.request(ctx, config, url.Values{"t": {"caps"}})
	if err != nil {
		return site.Health{}, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, tokenErr := decoder.Token()
		if tokenErr == io.EOF {
			break
		}
		if tokenErr != nil {
			return site.Health{}, site.ErrInvalidReply
		}
		if start, ok := token.(xml.StartElement); ok {
			if strings.EqualFold(start.Name.Local, "caps") {
				return site.Health{Status: "online"}, nil
			}
			return site.Health{}, site.ErrInvalidReply
		}
	}
	return site.Health{}, site.ErrInvalidReply
}

func (a *Adapter) Search(ctx context.Context, config site.Config, query site.Query) (site.Page, error) {
	keyword := strings.TrimSpace(query.Keyword)
	if keyword == "" || len([]rune(keyword)) > 160 || query.Page < 1 || query.Page > 20 {
		return site.Page{}, site.ErrInvalidReply
	}
	if query.Year != nil {
		keyword += " " + strconv.Itoa(*query.Year)
	}
	values := url.Values{"t": {"search"}, "q": {keyword}, "limit": {"100"}, "offset": {strconv.Itoa((query.Page - 1) * 100)}}
	switch strings.ToLower(query.MediaType) {
	case "movie":
		values.Set("cat", "2000")
	case "tv":
		values.Set("cat", "5000")
	}
	body, err := a.request(ctx, config, values)
	if err != nil {
		return site.Page{}, err
	}
	parsed, err := rssfeed.Parse(body)
	if err != nil {
		return site.Page{}, err
	}
	// rssfeed.Parse omits malformed rows. Pagination and the skipped count
	// must still reflect the upstream page, not just its usable entries.
	var rawPage struct {
		Channel struct {
			Items []struct{} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &rawPage); err != nil {
		return site.Page{}, site.ErrInvalidReply
	}
	rawCount := len(rawPage.Channel.Items)
	result := site.Page{Page: query.Page, HasNext: query.Page < 20 && rawCount >= 100, Items: make([]site.Result, 0, len(parsed)), Skipped: rawCount - len(parsed)}
	for _, item := range parsed {
		if !titleMatchesKeyword(query.Keyword, item.Title) {
			result.Skipped++
			continue
		}
		identity := ""
		for _, candidate := range item.Sources {
			if magnet, ok := btrss.NormalizeMagnet(candidate); ok {
				identity = btrss.EncodeIdentity("magnet", magnet)
				break
			}
			if safeTorznabURL(config.BaseURL, candidate) {
				identity = btrss.EncodeIdentity("torrent", candidate)
				break
			}
		}
		if identity == "" {
			result.Skipped++
			continue
		}
		result.Items = append(result.Items, site.Result{TorrentID: identity, Title: item.Title, Subtitle: item.Subtitle, SizeBytes: item.SizeBytes, Published: item.Published, Seeders: item.Seeders, Leechers: item.Leechers, Completed: item.Completed})
	}
	return result, nil
}

// Torznab/Jackett may return cached or broadly matched entries unrelated to
// the requested title. Keep only releases whose title contains the normalized
// CJK query and every non-CJK query term as a separate release-name token.
// Year from Query.Year is deliberately not required: it is an upstream search
// hint, while releases may omit a year from their name.
func titleMatchesKeyword(keyword, title string) bool {
	var cjkQuery, term strings.Builder
	terms := make([]string, 0, 4)
	flushTerm := func() {
		if term.Len() > 0 {
			terms = append(terms, term.String())
			term.Reset()
		}
	}
	for _, r := range keyword {
		switch {
		case isCJK(r):
			flushTerm()
			cjkQuery.WriteRune(unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			term.WriteRune(unicode.ToLower(r))
		default:
			flushTerm()
		}
	}
	flushTerm()
	if cjkQuery.Len() == 0 && len(terms) == 0 {
		return false
	}

	var compactTitle, titleTerm strings.Builder
	titleTerms := make(map[string]struct{}, 12)
	flushTitleTerm := func() {
		if titleTerm.Len() > 0 {
			titleTerms[titleTerm.String()] = struct{}{}
			titleTerm.Reset()
		}
	}
	for _, r := range title {
		switch {
		case isCJK(r):
			flushTitleTerm()
			compactTitle.WriteRune(unicode.ToLower(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			lower := unicode.ToLower(r)
			compactTitle.WriteRune(lower)
			titleTerm.WriteRune(lower)
		default:
			flushTitleTerm()
		}
	}
	flushTitleTerm()
	if cjkQuery.Len() > 0 && !strings.Contains(compactTitle.String(), cjkQuery.String()) {
		return false
	}
	for _, term := range terms {
		if _, ok := titleTerms[term]; !ok {
			return false
		}
	}
	return true
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func (a *Adapter) Download(context.Context, site.Config, string) ([]byte, string, error) {
	return nil, "", site.ErrNotFound
}

func (a *Adapter) ResolveSource(ctx context.Context, config site.Config, identity string) (site.Source, error) {
	kind, raw, ok := btrss.DecodeIdentity(identity)
	if !ok {
		return site.Source{}, site.ErrNotFound
	}
	if kind == "magnet" {
		magnet, valid := btrss.NormalizeMagnet(raw)
		if !valid {
			return site.Source{}, site.ErrInvalidReply
		}
		return site.Source{Magnet: magnet}, nil
	}
	if kind != "torrent" || !safeTorznabURL(config.BaseURL, raw) {
		return site.Source{}, site.ErrInvalidReply
	}
	client, _, err := a.clientFactory(config)
	if err != nil {
		return site.Source{}, site.ErrUnavailable
	}
	target, _ := url.Parse(raw)
	query := target.Query()
	// The feed-provided URL is untrusted input even though it is same-origin.
	// Always replace any embedded key with the encrypted configured value.
	query.Set("apikey", config.APIKey)
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return site.Source{}, site.ErrUnavailable
	}
	request.Header.Set("Accept", "application/x-bittorrent")
	response, err := client.Do(request)
	if err != nil {
		return site.Source{}, site.ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return site.Source{}, site.ErrAuthentication
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return site.Source{}, site.ErrRateLimited
	}
	if response.StatusCode != http.StatusOK {
		return site.Source{}, site.ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTorrentBytes+1))
	if err != nil || len(body) < 16 || len(body) > maxTorrentBytes || body[0] != 'd' {
		return site.Source{}, site.ErrInvalidReply
	}
	filename := path.Base(target.Path)
	if !strings.EqualFold(path.Ext(filename), ".torrent") || len(filename) > 255 {
		filename = "torznab.torrent"
	}
	return site.Source{Torrent: body, Filename: filename}, nil
}

func (a *Adapter) request(ctx context.Context, config site.Config, query url.Values) ([]byte, error) {
	if strings.TrimSpace(config.APIKey) == "" || len(config.APIKey) > 2048 || strings.ContainsAny(config.APIKey, "\x00\r\n") {
		return nil, site.ErrAuthentication
	}
	client, base, err := a.clientFactory(config)
	if err != nil {
		return nil, site.ErrUnavailable
	}
	target := *base
	rootAddress := target.Path == "" || target.Path == "/"
	if !strings.HasSuffix(strings.TrimRight(target.Path, "/"), "/api") {
		target.Path = strings.TrimRight(target.Path, "/") + "/api"
	}
	query.Set("apikey", config.APIKey)
	target.RawQuery = query.Encode()
	body, status, err := requestTorznabTarget(ctx, client, &target)
	if rootAddress && status == http.StatusNotFound {
		// Jackett's root is not itself a Torznab endpoint. Only a 404 from
		// the generic /api path permits the same-origin all-indexer fallback.
		target.Path = "/api/v2.0/indexers/all/results/torznab/api"
		body, _, err = requestTorznabTarget(ctx, client, &target)
	}
	return body, err
}

func requestTorznabTarget(ctx context.Context, client *http.Client, target *url.URL) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, site.ErrUnavailable
	}
	request.Header.Set("Accept", "application/xml,application/rss+xml;q=0.9")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, site.ErrUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, response.StatusCode, site.ErrAuthentication
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return nil, response.StatusCode, site.ErrRateLimited
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, site.ErrUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxXMLBytes+1))
	if err != nil || len(body) > maxXMLBytes {
		return nil, response.StatusCode, site.ErrInvalidReply
	}
	return body, response.StatusCode, nil
}

func safeTorznabURL(baseURL, raw string) bool {
	base, baseErr := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	target, targetErr := url.Parse(strings.TrimSpace(raw))
	return baseErr == nil && targetErr == nil && (base.Scheme == "http" || base.Scheme == "https") && target.Scheme == base.Scheme && base.Host != "" && strings.EqualFold(base.Host, target.Host) && target.User == nil && target.Fragment == "" && len(raw) <= 8192
}

func controlledClient(config site.Config) (*http.Client, *url.URL, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, nil, site.ErrUnavailable
	}
	timeout := config.Timeout
	if timeout < 3*time.Second || timeout > 30*time.Second {
		timeout = 12 * time.Second
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 2 || next.URL.Scheme != base.Scheme || !strings.EqualFold(next.URL.Host, base.Host) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return client, base, nil
}
