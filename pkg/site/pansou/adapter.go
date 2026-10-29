package pansou

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

const maxSnapshotItems = 1000
const maxSnapshots = 32
const snapshotTTL = 10 * time.Minute

type snapshot struct {
	key     string
	expires time.Time
	page    site.Page
}
type defaultsEntry struct {
	channels []string
	expires  time.Time
}
type Adapter struct {
	client    *http.Client
	mu        sync.Mutex
	snapshots map[string]snapshot
	defaults  map[string]defaultsEntry
}

func New() *Adapter                           { return &Adapter{} }
func NewForTest(client *http.Client) *Adapter { return &Adapter{client: client} }
func (*Adapter) Kind() string                 { return Kind }
func (a *Adapter) Test(ctx context.Context, c site.Config) (site.Health, error) {
	_, err := a.Search(ctx, c, site.Query{Keyword: "电影", Page: 1, TypePolicy: "all", Refresh: true})
	return site.Health{Status: "online"}, err
}
func (*Adapter) Download(context.Context, site.Config, string) ([]byte, string, error) {
	return nil, "", site.ErrNotFound
}
func (*Adapter) ResolveSource(_ context.Context, c site.Config, identity string) (site.Source, error) {
	if _, err := NormalizeConfig(c.Cloud); err != nil {
		return site.Source{}, err
	}
	kind, normalized, _, err := NormalizeResource("", identity, "")
	if err != nil {
		return site.Source{}, err
	}
	switch kind {
	case "115", "quark":
		return site.Source{ShareURL: normalized, CloudProvider: kind}, nil
	case "magnet":
		return site.Source{Magnet: normalized}, nil
	default:
		return site.Source{}, site.ErrNotFound
	}
}

func baseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, site.ErrUnavailable
	}
	return u, nil
}
func (a *Adapter) request(ctx context.Context, c site.Config, method, path string, payload any, token string, out any) error {
	u, err := baseURL(c.BaseURL)
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return site.ErrInvalidReply
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return site.ErrUnavailable
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.Client{}
	if a.client != nil {
		client = *a.client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return site.ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return site.ErrAuthentication
	}
	if response.StatusCode == 429 {
		return site.ErrRateLimited
	}
	if response.StatusCode != 200 {
		return site.ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 || json.Unmarshal(raw, out) != nil {
		return site.ErrInvalidReply
	}
	return nil
}
func boundedContext(ctx context.Context, c site.Config) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, site.RequestTimeout(c, 30*time.Second))
}

type linkReply struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Password  string `json:"password"`
	WorkTitle string `json:"work_title"`
	Title     string `json:"title"`
	Note      string `json:"note"`
	Datetime  string `json:"datetime"`
	Source    string `json:"source"`
}
type searchReply struct {
	Code *int `json:"code"`
	Data *struct {
		Total   *int `json:"total"`
		Results []struct {
			UniqueID  string      `json:"unique_id"`
			Title     string      `json:"title"`
			Datetime  string      `json:"datetime"`
			Channel   string      `json:"channel"`
			Source    string      `json:"source"`
			MessageID string      `json:"message_id"`
			Links     []linkReply `json:"links"`
		} `json:"results"`
		Merged map[string][]linkReply `json:"merged_by_type"`
	} `json:"data"`
}

func queryKey(c site.Config, q site.Query) string {
	raw, _ := json.Marshal(struct {
		Config, Scope, Keyword, Media, Policy string
		Year                                  *int
		TMDB                                  *int64
		Types                                 []string
	}{configKey(c), q.ScopeKey, q.Keyword, q.MediaType, q.TypePolicy, q.Year, q.TMDBID, q.CloudTypes})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func clonePage(p site.Page, number int) site.Page {
	items := p.Items
	start := (number - 1) * 50
	p.Page = number
	p.Items = []site.Result{}
	p.HasNext = false
	if start >= len(items) {
		return p
	}
	end := min(start+50, len(items))
	p.Items = append(p.Items, items[start:end]...)
	p.HasNext = end < len(items) && number < 20
	return p
}
func (a *Adapter) cachedSnapshot(key string, q site.Query) (site.Page, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if q.Refresh {
		return site.Page{}, false, nil
	}
	now := time.Now()
	for id, value := range a.snapshots {
		if !value.expires.After(now) {
			delete(a.snapshots, id)
		}
	}
	if q.SnapshotID != "" {
		entry, ok := a.snapshots[q.SnapshotID]
		if !ok || entry.key != key {
			return site.Page{}, false, site.ErrSnapshotExpired
		}
		return clonePage(entry.page, q.Page), true, nil
	}
	for _, entry := range a.snapshots {
		if entry.key == key {
			return clonePage(entry.page, q.Page), true, nil
		}
	}
	if q.Page > 1 {
		return site.Page{}, false, site.ErrSnapshotExpired
	}
	return site.Page{}, false, nil
}

func (a *Adapter) Search(ctx context.Context, c site.Config, q site.Query) (site.Page, error) {
	cfg, err := NormalizeConfig(c.Cloud)
	if err != nil || strings.TrimSpace(q.Keyword) == "" || len([]rune(q.Keyword)) > 160 || q.Page < 1 || q.Page > 20 || q.Refresh && q.Page != 1 {
		return site.Page{}, site.ErrInvalidReply
	}
	c.Cloud = cfg
	if q.TypePolicy == "" {
		q.TypePolicy = cfg.TypePolicy
	}
	if q.CloudTypes == nil {
		q.CloudTypes = cfg.CloudTypes
	}
	if q.TypePolicy == "auto" && len(q.CloudTypes) == 0 {
		q.CloudTypes = []string{"magnet"}
	}
	if q.TypePolicy != "auto" && q.TypePolicy != "all" && q.TypePolicy != "manual" {
		return site.Page{}, site.ErrInvalidReply
	}
	q.CloudTypes, err = NormalizeTypes(q.CloudTypes)
	if err != nil || q.TypePolicy == "manual" && len(q.CloudTypes) == 0 {
		return site.Page{}, site.ErrInvalidReply
	}
	if q.TypePolicy == "all" {
		q.CloudTypes = nil
	}
	key := queryKey(c, q)
	if cached, ok, err := a.cachedSnapshot(key, q); err != nil || ok {
		return cached, err
	}
	ctx, cancel := boundedContext(ctx, c)
	defer cancel()
	scope, scopeErr := a.ResolveChannelScope(ctx, c, false)
	source := cfg.SourceScope
	page := site.Page{Items: []site.Result{}, Scope: &scope, Completion: "received"}
	if scopeErr != nil {
		if source == "tg" {
			return site.Page{}, scopeErr
		}
		source = "plugin"
		page.Partial = true
		page.Warnings = append(page.Warnings, site.SearchWarning{Code: scope.DefaultErrorCode, Message: "TG默认频道未能完整读取，保留插件结果；请重新检测频道范围"})
	}
	login := func() (string, error) {
		if !cfg.AuthEnabled {
			return "", nil
		}
		if c.Username == "" || c.Password == "" {
			return "", site.ErrAuthentication
		}
		var reply struct {
			Token string `json:"token"`
		}
		if err := a.request(ctx, c, http.MethodPost, "/api/auth/login", map[string]string{"username": c.Username, "password": c.Password}, "", &reply); err != nil {
			return "", err
		}
		if reply.Token == "" || len(reply.Token) > 8192 || strings.ContainsAny(reply.Token, "\r\n") {
			return "", site.ErrAuthentication
		}
		return reply.Token, nil
	}
	token, err := login()
	if err != nil {
		return site.Page{}, err
	}
	payload := map[string]any{"kw": q.Keyword, "src": source, "res": "all", "refresh": q.Refresh}
	if source != "plugin" {
		payload["channels"] = scope.EffectiveChannels
	}
	if len(cfg.Plugins) > 0 && source != "tg" {
		payload["plugins"] = cfg.Plugins
	}
	if q.TypePolicy != "all" {
		payload["cloud_types"] = q.CloudTypes
	}
	var response searchReply
	err = a.request(ctx, c, http.MethodPost, "/api/search", payload, token, &response)
	if err == site.ErrAuthentication && cfg.AuthEnabled {
		token, err = login()
		if err == nil {
			err = a.request(ctx, c, http.MethodPost, "/api/search", payload, token, &response)
		}
	}
	if err != nil {
		return site.Page{}, err
	}
	if response.Code == nil || response.Data == nil || response.Data.Total == nil || *response.Data.Total < 0 {
		return site.Page{}, site.ErrInvalidReply
	}
	if *response.Code != 0 {
		return site.Page{}, site.ErrUnavailable
	}
	allowedChannels := map[string]bool{}
	for _, name := range scope.EffectiveChannels {
		allowedChannels[name] = true
	}
	allowedPlugins := map[string]bool{}
	for _, name := range cfg.Plugins {
		allowedPlugins[name] = true
	}
	types := map[string]bool{}
	for _, name := range q.CloudTypes {
		types[name] = true
	}
	appendLink := func(link linkReply, rawTitle, datetime string, origin site.ResultOrigin) {
		if origin.Kind == "tg" && (source == "plugin" || !allowedChannels[origin.Name]) || origin.Kind == "plugin" && (source == "tg" || len(allowedPlugins) > 0 && !allowedPlugins[origin.Name]) || origin.Kind == "" {
			page.Skipped++
			return
		}
		kind, normalized, identity, err := NormalizeResource(link.Type, link.URL, link.Password)
		if err != nil {
			page.Skipped++
			return
		}
		if q.TypePolicy != "all" && !types[kind] {
			return
		}
		if strings.TrimSpace(link.WorkTitle) != "" {
			rawTitle = link.WorkTitle
		} else if rawTitle == "" {
			rawTitle = first(link.Title, link.Note)
		}
		title := strings.TrimSpace(html.UnescapeString(markup.ReplaceAllString(rawTitle, "")))
		if title == "" || len([]rune(title)) > 512 || strings.ContainsAny(title, "\x00\r\n") {
			page.Skipped++
			return
		}
		sum := sha256.Sum256([]byte(kind + ":" + identity))
		item := site.Result{PayloadKind: site.PayloadUnknown, TorrentID: normalized, Title: title, ResultType: kind, Origins: []site.ResultOrigin{origin}, Fingerprint: hex.EncodeToString(sum[:])}
		switch kind {
		case "115", "quark":
			item.PayloadKind = site.PayloadShare
			item.SourceKind = kind + "_share"
			item.CloudProvider = kind
		case "magnet":
			item.PayloadKind = site.PayloadMagnet
			item.SourceKind = "bt"
		default:
			item.SourceKind = "external_link"
			if kind != "ed2k" && kind != "others" {
				item.PayloadKind = site.PayloadShare
				item.CloudProvider = kind
			}
		}
		if origin.Kind == "tg" {
			item.Channel = origin.Name
			item.PostURL = origin.PostURL
		}
		item.Published = parseTime(link.Datetime)
		if item.Published == nil {
			item.Published = parseTime(datetime)
		}
		page.Items = append(page.Items, item)
	}
	for _, message := range response.Data.Results {
		origin := parseOrigin(first(message.Source, message.Channel))
		if origin.Kind == "" && message.Source == "" && message.Channel == "" {
			// PanSou's raw results identify plugins by their unique_id prefix;
			// merged_by_type also supplies an explicit plugin:<name> source.
			if name, _, ok := strings.Cut(message.UniqueID, "-"); ok {
				origin = parseOrigin("plugin:" + name)
			}
		}
		if origin.Kind == "tg" && messageIDPattern.MatchString(message.MessageID) {
			origin.PostURL = "https://t.me/" + origin.Name + "/" + message.MessageID
		}
		for _, link := range message.Links {
			itemOrigin := origin
			if link.Source != "" {
				itemOrigin = parseOrigin(link.Source)
			}
			appendLink(link, message.Title, message.Datetime, itemOrigin)
		}
	}
	keys := make([]string, 0, len(response.Data.Merged))
	for kind := range response.Data.Merged {
		keys = append(keys, kind)
	}
	sort.Strings(keys)
	for _, kind := range keys {
		for _, link := range response.Data.Merged[kind] {
			link.Type = kind
			appendLink(link, "", link.Datetime, parseOrigin(link.Source))
		}
	}
	sort.SliceStable(page.Items, func(i, j int) bool {
		x, y := page.Items[i].Published, page.Items[j].Published
		return x != nil && (y == nil || x.After(*y))
	})
	unique := []site.Result{}
	positions := map[string]int{}
	for _, item := range page.Items {
		if index, exists := positions[item.Fingerprint]; exists {
			old := &unique[index]
			old.TorrentID = preserveExtractionCode(old.TorrentID, item.TorrentID)
			for _, origin := range item.Origins {
				present := false
				for _, saved := range old.Origins {
					if saved.Kind == origin.Kind && saved.Name == origin.Name && saved.PostURL == origin.PostURL {
						present = true
						break
					}
				}
				if !present && len(old.Origins) < 32 {
					old.Origins = append(old.Origins, origin)
				}
			}
			continue
		}
		positions[item.Fingerprint] = len(unique)
		unique = append(unique, item)
	}
	page.Items = unique
	if len(page.Items) > maxSnapshotItems {
		page.Items = page.Items[:maxSnapshotItems]
		page.Partial = true
		page.Truncated = true
		page.Warnings = append(page.Warnings, site.SearchWarning{Code: "pansou_snapshot_truncated", Message: "本批结果超过1000条，已保留前1000条；请缩小关键词或类型后重搜"})
	}
	page.Warnings = append(page.Warnings, site.SearchWarning{Code: "pansou_received_batch", Message: "已收到本次PanSou响应；上游未提供逐插件完成凭据"})
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return site.Page{}, site.ErrUnavailable
	}
	page.SnapshotID = hex.EncodeToString(random[:])
	expires := time.Now().Add(snapshotTTL)
	page.SnapshotExpiresAt = &expires
	a.mu.Lock()
	if a.snapshots == nil {
		a.snapshots = map[string]snapshot{}
	}
	for id, entry := range a.snapshots {
		if entry.key == key || !entry.expires.After(time.Now()) {
			delete(a.snapshots, id)
		}
	}
	if len(a.snapshots) >= maxSnapshots {
		var oldestID string
		var oldest time.Time
		for id, entry := range a.snapshots {
			if oldestID == "" || entry.expires.Before(oldest) {
				oldestID = id
				oldest = entry.expires
			}
		}
		delete(a.snapshots, oldestID)
	}
	a.snapshots[page.SnapshotID] = snapshot{key, expires, page}
	a.mu.Unlock()
	return clonePage(page, q.Page), nil
}
