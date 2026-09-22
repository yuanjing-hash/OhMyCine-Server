package pansou

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

const Kind = "pansou_tg"

var channelPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,63}$`)
var messageIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var markup = regexp.MustCompile(`<[^>]*>`)

func NormalizeConfig(input *site.CloudConfig) (*site.CloudConfig, error) {
	if input == nil || input.Provider != "115" || len(input.Channels) == 0 || len(input.Channels) > 100 {
		return nil, site.ErrInvalidReply
	}
	out := &site.CloudConfig{Provider: "115", AuthEnabled: input.AuthEnabled, Channels: []string{}}
	seen := map[string]bool{}
	for _, raw := range input.Channels {
		name := strings.TrimSpace(raw)
		if strings.Contains(name, "://") {
			u, err := url.Parse(name)
			if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || (u.Host != "t.me" && u.Host != "telegram.me") {
				return nil, site.ErrInvalidReply
			}
			name = strings.TrimPrefix(strings.Trim(u.Path, "/"), "s/")
		}
		name = strings.TrimPrefix(name, "@")
		if !channelPattern.MatchString(name) {
			return nil, site.ErrInvalidReply
		}
		name = strings.ToLower(name)
		if !seen[name] {
			out.Channels = append(out.Channels, name)
			seen[name] = true
		}
	}
	return out, nil
}

type Adapter struct{ client *http.Client }

func New() *Adapter                           { return &Adapter{} }
func NewForTest(client *http.Client) *Adapter { return &Adapter{client: client} }
func (*Adapter) Kind() string                 { return Kind }
func (a *Adapter) Test(ctx context.Context, c site.Config) (site.Health, error) {
	// A bounded search checks authentication and the actual search contract.
	_, err := a.Search(ctx, c, site.Query{Keyword: "电影", Page: 1})
	return site.Health{Status: "online"}, err
}
func (*Adapter) Download(context.Context, site.Config, string) ([]byte, string, error) {
	return nil, "", site.ErrNotFound
}
func (*Adapter) ResolveSource(_ context.Context, c site.Config, identity string) (site.Source, error) {
	if c.Cloud == nil || c.Cloud.Provider != "115" {
		return site.Source{}, site.ErrInvalidReply
	}
	normalized, _, err := pan115.NormalizeShareLink(identity, "")
	if err != nil {
		return site.Source{}, site.ErrInvalidReply
	}
	return site.Source{ShareURL: normalized, CloudProvider: "115"}, nil
}

type searchReply struct {
	Code *int `json:"code"`
	Data *struct {
		Total   *int `json:"total"`
		Results []struct {
			Title     string `json:"title"`
			Datetime  string `json:"datetime"`
			Channel   string `json:"channel"`
			MessageID string `json:"message_id"`
			Links     []struct {
				Type      string `json:"type"`
				URL       string `json:"url"`
				Password  string `json:"password"`
				WorkTitle string `json:"work_title"`
				Datetime  string `json:"datetime"`
			} `json:"links"`
		} `json:"results"`
	} `json:"data"`
}

func (a *Adapter) Search(ctx context.Context, c site.Config, q site.Query) (site.Page, error) {
	cfg, err := NormalizeConfig(c.Cloud)
	if err != nil || strings.TrimSpace(q.Keyword) == "" || len([]rune(q.Keyword)) > 160 || q.Page < 1 || q.Page > 20 {
		return site.Page{}, site.ErrInvalidReply
	}
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return site.Page{}, site.ErrUnavailable
	}
	timeout := c.Timeout
	if timeout < 3*time.Second || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := a.client
	if client == nil {
		client = &http.Client{}
	}
	controlled := *client
	controlled.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &controlled
	call := func(path string, payload any, token string, out any) error {
		raw, err := json.Marshal(payload)
		if err != nil {
			return site.ErrInvalidReply
		}
		target := *base
		target.Path = strings.TrimRight(base.Path, "/") + path
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(raw))
		if err != nil {
			return site.ErrUnavailable
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
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
		body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		if err != nil || len(body) > 4<<20 || json.Unmarshal(body, out) != nil {
			return site.ErrInvalidReply
		}
		return nil
	}
	login := func() (string, error) {
		if !cfg.AuthEnabled {
			return "", nil
		}
		if c.Username == "" || c.Password == "" {
			return "", site.ErrAuthentication
		}
		var response struct {
			Token string `json:"token"`
		}
		if err := call("/api/auth/login", map[string]string{"username": c.Username, "password": c.Password}, "", &response); err != nil {
			return "", err
		}
		if response.Token == "" || len(response.Token) > 8192 || strings.ContainsAny(response.Token, "\r\n") {
			return "", site.ErrAuthentication
		}
		return response.Token, nil
	}
	token, err := login()
	if err != nil {
		return site.Page{}, err
	}
	payload := map[string]any{"kw": q.Keyword, "channels": cfg.Channels, "cloud_types": []string{"115"}, "src": "tg", "res": "results", "refresh": true}
	var response searchReply
	err = call("/api/search", payload, token, &response)
	if err == site.ErrAuthentication && cfg.AuthEnabled {
		token, err = login()
		if err == nil {
			err = call("/api/search", payload, token, &response)
		}
	}
	if err != nil {
		return site.Page{}, err
	}
	if response.Code == nil {
		return site.Page{}, site.ErrInvalidReply
	}
	if *response.Code != 0 {
		return site.Page{}, site.ErrUnavailable
	}
	if response.Data == nil || response.Data.Total == nil || *response.Data.Total < 0 {
		return site.Page{}, site.ErrInvalidReply
	}
	allowed := map[string]bool{}
	for _, name := range cfg.Channels {
		allowed[name] = true
	}
	page := site.Page{Page: q.Page, Items: []site.Result{}}
	for _, message := range response.Data.Results {
		channel := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(message.Channel), "@"))
		if !allowed[channel] {
			page.Skipped++
			continue
		}
		for _, link := range message.Links {
			if link.Type != "115" {
				continue
			}
			rawTitle := message.Title
			if strings.TrimSpace(link.WorkTitle) != "" {
				rawTitle = link.WorkTitle
			}
			title := strings.TrimSpace(html.UnescapeString(markup.ReplaceAllString(rawTitle, "")))
			if title == "" || len([]rune(title)) > 512 || strings.ContainsAny(title, "\x00\r\n") {
				page.Skipped++
				continue
			}
			normalized, identity, err := pan115.NormalizeShareLink(link.URL, link.Password)
			if err != nil {
				page.Skipped++
				continue
			}
			digest := sha256.Sum256([]byte("115:" + identity))
			item := site.Result{TorrentID: normalized, Title: title, SourceKind: "115_share", CloudProvider: "115", Channel: channel, Fingerprint: hex.EncodeToString(digest[:])}
			item.Published = parseTime(link.Datetime)
			if item.Published == nil {
				item.Published = parseTime(message.Datetime)
			}
			if channel != "" && messageIDPattern.MatchString(message.MessageID) {
				item.PostURL = "https://t.me/" + channel + "/" + message.MessageID
			}
			page.Items = append(page.Items, item)
		}
	}
	sort.SliceStable(page.Items, func(i, j int) bool {
		a, b := page.Items[i].Published, page.Items[j].Published
		return a != nil && (b == nil || a.After(*b))
	})
	// Sort before deduplication so a share updated with new episodes keeps its latest title.
	unique := make([]site.Result, 0, len(page.Items))
	seen := map[string]bool{}
	for _, item := range page.Items {
		if !seen[item.Fingerprint] {
			unique = append(unique, item)
			seen[item.Fingerprint] = true
		}
	}
	page.Items = unique
	start := (q.Page - 1) * 50
	if start >= len(page.Items) {
		page.Items = []site.Result{}
		return page, nil
	}
	end := min(start+50, len(page.Items))
	page.HasNext = q.Page < 20 && end < len(page.Items)
	page.Items = page.Items[start:end]
	return page, nil
}

func parseTime(value string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if stamp, err := time.Parse(layout, value); err == nil && !stamp.IsZero() {
			return &stamp
		}
	}
	return nil
}
