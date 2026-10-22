package site

import (
	"context"
	"errors"
	"time"
)

var (
	ErrAuthentication  = errors.New("site_authentication_failed")
	ErrRateLimited     = errors.New("site_rate_limited")
	ErrUnavailable     = errors.New("site_unavailable")
	ErrInvalidReply    = errors.New("site_invalid_response")
	ErrNotFound        = errors.New("site_result_not_found")
	ErrSnapshotExpired = errors.New("site_search_snapshot_expired")
)

type CloudConfig struct {
	Provider    string   `json:"provider"`
	Channels    []string `json:"channels"`
	AuthEnabled bool     `json:"auth_enabled"`
	SourceScope string   `json:"source_scope,omitempty"`
	ChannelMode string   `json:"channel_mode,omitempty"`
	Plugins     []string `json:"plugins,omitempty"`
	TypePolicy  string   `json:"type_policy,omitempty"`
	CloudTypes  []string `json:"cloud_types,omitempty"`
}

type Config struct {
	Cloud     *CloudConfig
	Username  string
	Password  string
	BaseURL   string
	Cookie    string
	Passkey   string
	APIKey    string
	UserAgent string
	Timeout   time.Duration
	// SearchTimeout is set only for search. Probe and download clients retain
	// their original bounds even when a slow-search policy is configured.
	SearchTimeout     time.Duration
	BrowserEmulation  bool
	BrowserServiceURL string
	RenderedFetcher   RenderedFetcher
}

func RequestTimeout(config Config, fallback time.Duration) time.Duration {
	if config.SearchTimeout >= 3*time.Second && config.SearchTimeout <= 180*time.Second {
		return config.SearchTimeout
	}
	if config.Timeout >= 3*time.Second && config.Timeout <= 30*time.Second {
		return config.Timeout
	}
	return fallback
}

// Source is the server-only download material resolved from an opaque search
// result identity. Exactly one of Torrent, Magnet or ShareURL is populated.
type Source struct {
	ShareURL      string
	CloudProvider string
	Torrent       []byte
	Filename      string
	Magnet        string
}

type Query struct {
	Keyword    string
	MediaType  string
	Year       *int
	TMDBID     *int64
	Page       int
	TypePolicy string
	CloudTypes []string
	// ScopeKey is supplied by SiteService and binds snapshots to actor,
	// authorization, site revision and the actual client capability contract.
	ScopeKey   string
	SnapshotID string
	Refresh    bool
}

type Result struct {
	SourceKind    string         `json:"source_kind,omitempty"`
	CloudProvider string         `json:"cloud_provider,omitempty"`
	Channel       string         `json:"channel,omitempty"`
	PostURL       string         `json:"post_url,omitempty"`
	Fingerprint   string         `json:"-"`
	ResultType    string         `json:"result_type,omitempty"`
	Origins       []ResultOrigin `json:"origins,omitempty"`
	TorrentID     string         `json:"-"`
	Title         string         `json:"title"`
	Subtitle      string         `json:"subtitle,omitempty"`
	SizeBytes     int64          `json:"size_bytes,omitempty"`
	Published     *time.Time     `json:"published_at,omitempty"`
	Seeders       *int           `json:"seeders,omitempty"`
	Leechers      *int           `json:"leechers,omitempty"`
	Completed     *int           `json:"completed,omitempty"`
	Promotion     string         `json:"promotion,omitempty"`
	Quality       string         `json:"quality,omitempty"`
	Tags          []string       `json:"tags,omitempty"`
}

type Page struct {
	Page              int
	HasNext           bool
	Items             []Result
	Skipped           int
	SnapshotID        string
	SnapshotExpiresAt *time.Time
	Partial           bool
	Truncated         bool
	Completion        string
	Warnings          []SearchWarning
	Scope             *ChannelScope
}

type ResultOrigin struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	PostURL string `json:"post_url,omitempty"`
}

type SearchWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ChannelScope struct {
	SourceScope       string   `json:"source_scope"`
	ChannelMode       string   `json:"channel_mode"`
	DefaultChannels   []string `json:"default_channels"`
	CustomChannels    []string `json:"custom_channels"`
	EffectiveChannels []string `json:"effective_channels"`
	DefaultCount      int      `json:"default_count"`
	CustomCount       int      `json:"custom_count"`
	EffectiveCount    int      `json:"effective_count"`
	DefaultErrorCode  string   `json:"default_error_code,omitempty"`
}

type ChannelScopeResolver interface {
	ResolveChannelScope(context.Context, Config, bool) (ChannelScope, error)
}

type Health struct {
	Username string
	Status   string
}

type Adapter interface {
	Kind() string
	Test(context.Context, Config) (Health, error)
	Search(context.Context, Config, Query) (Page, error)
	Download(context.Context, Config, string) ([]byte, string, error)
}

// SourceResolver resolves native site identities to bounded torrent files,
// normalized magnets, or provider-validated cloud share URLs. The identity
// never leaves the SiteService result vault.
type SourceResolver interface {
	ResolveSource(context.Context, Config, string) (Source, error)
}
