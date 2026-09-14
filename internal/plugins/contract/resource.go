package contract

import (
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ResourceSearchRequest struct {
	ConnectionID string `json:"connectionId"`
	Query        string `json:"query"`
	Kind         string `json:"kind,omitempty"`
	Year         *int   `json:"year,omitempty"`
	Page         int    `json:"page,omitempty"`
}

type ResourceSearchItem struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	SizeBytes int64    `json:"sizeBytes"`
	Seeders   int      `json:"seeders"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

type ResourceSearchResponse struct {
	Items   []ResourceSearchItem `json:"items"`
	Page    int                  `json:"page"`
	HasNext bool                 `json:"hasNext"`
}

type ResourceResolveRequest struct {
	ConnectionID string `json:"connectionId"`
	ResourceID   string `json:"resourceId"`
}

type ResourceResolveResponse struct {
	Magnet string `json:"magnet"`
}
type ResourceHealthRequest struct {
	ConnectionID string `json:"connectionId"`
}

type ResourceHealthResponse struct {
	Status      string `json:"status"`
	AccountName string `json:"accountName,omitempty"`
}

type ResourceLoginRequest struct {
	ConnectionID string `json:"connectionId"`
	Username     string `json:"username"`
	Password     string `json:"password"`
}

type ResourceCookieRequest struct {
	ConnectionID string `json:"connectionId"`
	Cookie       string `json:"cookie"`
}

type ResourceCaptchaPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}
type ResourceCaptchaRequest struct {
	ConnectionID string                 `json:"connectionId"`
	ChallengeID  string                 `json:"challengeId"`
	Points       []ResourceCaptchaPoint `json:"points"`
}

type ResourceCaptchaChallenge struct {
	ChallengeID   string `json:"challengeId"`
	ImageAssetRef string `json:"imageAssetRef"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	Prompt        string `json:"prompt"`
	MaxPoints     int    `json:"maxPoints"`
}

type ResourceLoginResponse struct {
	State       string                    `json:"state"`
	AccountName string                    `json:"accountName,omitempty"`
	Challenge   *ResourceCaptchaChallenge `json:"challenge,omitempty"`
	ErrorCode   string                    `json:"errorCode,omitempty"`
}

var magnetInfoHash = regexp.MustCompile(`(?i)^[a-f0-9]{40}$`)

// NormalizeMagnet accepts only the BitTorrent v1 magnet identity and removes
// duplicate/unsafe parameters. Tracker URLs are intentionally preserved only
// when they are valid HTTPS/HTTP URLs; credentials and arbitrary data are not.
func NormalizeMagnet(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 16*1024 || !strings.EqualFold(parsed.Scheme, "magnet") || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.Fragment != "" {
		return "", false
	}
	values := parsed.Query()
	if len(values["xt"]) != 1 {
		return "", false
	}
	xt := strings.TrimSpace(values.Get("xt"))
	if !strings.HasPrefix(strings.ToLower(xt), "urn:btih:") || !magnetInfoHash.MatchString(strings.TrimSpace(xt[len("urn:btih:"):])) {
		return "", false
	}
	result := url.Values{}
	result.Set("xt", "urn:btih:"+strings.ToUpper(strings.TrimSpace(xt[len("urn:btih:"):])))
	for _, name := range []string{"dn", "xl", "tr"} {
		seen := map[string]struct{}{}
		for _, value := range values[name] {
			value = strings.TrimSpace(value)
			if value == "" || len(value) > 2048 || name == "tr" && !validTracker(value) {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result.Add(name, value)
		}
	}
	return "magnet:?" + result.Encode(), true
}

func validTracker(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" && parsed.User == nil
}

func (r ResourceSearchRequest) Validate() bool {
	return safeResourceText(r.ConnectionID, 128) && len([]rune(strings.TrimSpace(r.Query))) > 0 && len([]rune(r.Query)) <= 256 && len(r.Kind) <= 32 && (r.Page == 0 || r.Page >= 1 && r.Page <= 10000) && (r.Year == nil || *r.Year >= 1880 && *r.Year <= time.Now().UTC().Year()+2)
}
func (r ResourceResolveRequest) Validate() bool {
	return safeResourceText(r.ConnectionID, 128) && safeResourceText(r.ResourceID, 256)
}
func (r ResourceHealthRequest) Validate() bool { return safeResourceText(r.ConnectionID, 128) }
func (r ResourceLoginRequest) Validate() bool {
	return safeResourceText(r.ConnectionID, 128) && len([]rune(r.Username)) >= 2 && len([]rune(r.Username)) <= 128 && len([]rune(r.Password)) >= 6 && len([]rune(r.Password)) <= 256
}
func (r ResourceCookieRequest) Validate() bool {
	return safeResourceText(r.ConnectionID, 128) && len(r.Cookie) >= 1 && len(r.Cookie) <= 32*1024 && !strings.ContainsAny(r.Cookie, "\r\n")
}
func (r ResourceCaptchaRequest) Validate() bool {
	return safeResourceText(r.ConnectionID, 128) && safeResourceText(r.ChallengeID, 256) && len(r.Points) > 0 && len(r.Points) <= 16
}
func safeResourceText(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len([]rune(value)) <= maximum && !strings.ContainsAny(value, "\r\n")
}

func ParseResourceUpdatedAt(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if value, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return value.UTC(), true
		}
	}
	return time.Time{}, false
}

func FormatSizeBytes(raw string) int64 {
	text := strings.TrimSpace(strings.ReplaceAll(raw, ",", ""))
	if text == "" {
		return 0
	}
	cut := len(text)
	for index, character := range text {
		if (character < '0' || character > '9') && character != '.' {
			cut = index
			break
		}
	}
	number, suffix := strings.TrimSpace(text[:cut]), strings.TrimSpace(text[cut:])
	value, err := strconv.ParseFloat(number, 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	multiplier := float64(1)
	switch strings.ToUpper(strings.ReplaceAll(suffix, " ", "")) {
	case "K", "KB", "KIB":
		multiplier = 1 << 10
	case "M", "MB", "MIB":
		multiplier = 1 << 20
	case "G", "GB", "GIB":
		multiplier = 1 << 30
	case "T", "TB", "TIB":
		multiplier = 1 << 40
	case "", "B":
	default:
		return 0
	}
	bytes := value * multiplier
	if bytes >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(bytes)
}
