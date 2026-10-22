package pansou

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

const Kind = "pansou_tg"
const MaxChannels = 1024

var channelPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,63}$`)
var pluginPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
var knownTypes = map[string]bool{"115": true, "quark": true, "aliyun": true, "baidu": true, "tianyi": true, "uc": true, "xunlei": true, "123": true, "pikpak": true, "magnet": true, "ed2k": true, "others": true}

func NormalizeChannels(values []string) ([]string, error) {
	if len(values) > 4096 {
		return nil, site.ErrInvalidReply
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range values {
		name := strings.TrimSpace(raw)
		if strings.Contains(name, "://") {
			u, err := url.Parse(name)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || (strings.ToLower(u.Host) != "t.me" && strings.ToLower(u.Host) != "telegram.me") {
				return nil, site.ErrInvalidReply
			}
			name = strings.TrimPrefix(strings.Trim(u.Path, "/"), "s/")
		}
		name = strings.ToLower(strings.TrimPrefix(name, "@"))
		if !channelPattern.MatchString(name) {
			return nil, site.ErrInvalidReply
		}
		if !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	if len(out) > MaxChannels {
		return nil, site.ErrInvalidReply
	}
	return out, nil
}

func NormalizeTypes(values []string) ([]string, error) {
	if len(values) > 32 {
		return nil, site.ErrInvalidReply
	}
	out := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if !knownTypes[value] {
			return nil, site.ErrInvalidReply
		}
		if !seen[value] {
			out = append(out, value)
			seen[value] = true
		}
	}
	sort.Strings(out)
	return out, nil
}

func NormalizeConfig(input *site.CloudConfig) (*site.CloudConfig, error) {
	if input == nil {
		return nil, site.ErrInvalidReply
	}
	out := *input
	if out.Provider == "" {
		out.Provider = "all"
	}
	if out.Provider != "all" && out.Provider != "115" && out.Provider != "quark" {
		return nil, site.ErrInvalidReply
	}
	if out.SourceScope == "" {
		out.SourceScope = "all"
	}
	if out.SourceScope != "all" && out.SourceScope != "tg" && out.SourceScope != "plugin" {
		return nil, site.ErrInvalidReply
	}
	if out.ChannelMode == "" {
		if len(input.Channels) > 0 {
			out.ChannelMode = "custom_only"
		} else {
			out.ChannelMode = "defaults_plus_custom"
		}
	}
	if out.ChannelMode != "custom_only" && out.ChannelMode != "defaults_plus_custom" {
		return nil, site.ErrInvalidReply
	}
	var err error
	out.Channels, err = NormalizeChannels(input.Channels)
	if err != nil {
		return nil, err
	}
	if out.SourceScope != "plugin" && out.ChannelMode == "custom_only" && len(out.Channels) == 0 {
		return nil, site.ErrInvalidReply
	}
	if out.TypePolicy == "" {
		out.TypePolicy = "auto"
	}
	if out.TypePolicy != "auto" && out.TypePolicy != "all" && out.TypePolicy != "manual" {
		return nil, site.ErrInvalidReply
	}
	out.CloudTypes, err = NormalizeTypes(input.CloudTypes)
	if err != nil || out.TypePolicy == "manual" && len(out.CloudTypes) == 0 {
		return nil, site.ErrInvalidReply
	}
	if len(out.Plugins) > 128 {
		return nil, site.ErrInvalidReply
	}
	out.Plugins = []string{}
	seen := map[string]bool{}
	for _, value := range input.Plugins {
		value = strings.ToLower(strings.TrimSpace(value))
		if !pluginPattern.MatchString(value) {
			return nil, site.ErrInvalidReply
		}
		if !seen[value] {
			out.Plugins = append(out.Plugins, value)
			seen[value] = true
		}
	}
	sort.Strings(out.Plugins)
	return &out, nil
}

func configKey(c site.Config) string {
	raw, _ := json.Marshal(struct {
		Base, Username, Password string
		Cloud                    *site.CloudConfig
	}{c.BaseURL, c.Username, c.Password, c.Cloud})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (a *Adapter) ResolveChannelScope(ctx context.Context, c site.Config, refresh bool) (site.ChannelScope, error) {
	cfg, err := NormalizeConfig(c.Cloud)
	if err != nil {
		return site.ChannelScope{}, err
	}
	result := site.ChannelScope{SourceScope: cfg.SourceScope, ChannelMode: cfg.ChannelMode, DefaultChannels: []string{}, CustomChannels: cfg.Channels, EffectiveChannels: []string{}, CustomCount: len(cfg.Channels)}
	if cfg.SourceScope == "plugin" {
		return result, nil
	}
	if cfg.ChannelMode == "custom_only" {
		result.EffectiveChannels = append([]string(nil), cfg.Channels...)
		result.EffectiveCount = len(cfg.Channels)
		return result, nil
	}
	key := configKey(c)
	a.mu.Lock()
	cached, ok := a.defaults[key]
	a.mu.Unlock()
	if !refresh && ok && cached.expires.After(time.Now()) {
		result.DefaultChannels = append([]string(nil), cached.channels...)
	} else {
		ctx, cancel := boundedContext(ctx, c)
		defer cancel()
		var reply struct {
			Channels []string `json:"channels"`
			Data     *struct {
				Channels []string `json:"channels"`
			} `json:"data"`
		}
		if err := a.request(ctx, c, http.MethodGet, "/api/health", nil, "", &reply); err != nil {
			result.DefaultErrorCode = "pansou_default_channels_unavailable"
			return result, err
		}
		channels := reply.Channels
		if channels == nil && reply.Data != nil {
			channels = reply.Data.Channels
		}
		if channels == nil {
			result.DefaultErrorCode = "pansou_default_channels_invalid"
			return result, site.ErrInvalidReply
		}
		channels, err = NormalizeChannels(channels)
		if err != nil {
			result.DefaultErrorCode = "pansou_channel_limit_or_invalid"
			return result, err
		}
		result.DefaultChannels = channels
		a.mu.Lock()
		if a.defaults == nil {
			a.defaults = map[string]defaultsEntry{}
		}
		if len(a.defaults) >= 128 {
			for k := range a.defaults {
				delete(a.defaults, k)
				break
			}
		}
		a.defaults[key] = defaultsEntry{append([]string(nil), channels...), time.Now().Add(5 * time.Minute)}
		a.mu.Unlock()
	}
	result.DefaultCount = len(result.DefaultChannels)
	result.EffectiveChannels, err = NormalizeChannels(append(append([]string(nil), result.DefaultChannels...), cfg.Channels...))
	if err != nil {
		result.DefaultErrorCode = "pansou_channel_limit_or_invalid"
		return result, err
	}
	result.EffectiveCount = len(result.EffectiveChannels)
	return result, nil
}
