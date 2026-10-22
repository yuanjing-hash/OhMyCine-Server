package pansou

import (
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/btrss"
)

var messageIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var markup = regexp.MustCompile(`<[^>]*>`)
var ed2kPattern = regexp.MustCompile(`(?i)^ed2k://\|file\|[^|]+\|[0-9]{1,20}\|[a-f0-9]{32}\|/$`)

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func preserveExtractionCode(newer, older string) string {
	u, err := url.Parse(newer)
	if err != nil {
		return newer
	}
	v, err := url.Parse(older)
	if err != nil {
		return newer
	}
	query := u.Query()
	for _, key := range []string{"pwd", "password", "passcode", "accessCode", "code"} {
		if query.Get(key) == "" && v.Query().Get(key) != "" {
			query.Set(key, v.Query().Get(key))
		}
	}
	u.RawQuery = query.Encode()
	return u.String()
}
func parseOrigin(raw string) site.ResultOrigin {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "plugin:") {
		name := strings.ToLower(strings.TrimPrefix(raw, "plugin:"))
		if pluginPattern.MatchString(name) {
			return site.ResultOrigin{Kind: "plugin", Name: name}
		}
		return site.ResultOrigin{}
	}
	raw = strings.TrimPrefix(raw, "tg:")
	channels, err := NormalizeChannels([]string{raw})
	if err == nil && len(channels) == 1 {
		return site.ResultOrigin{Kind: "tg", Name: channels[0]}
	}
	return site.ResultOrigin{}
}

// NormalizeResource accepts only recognized public resource forms. Actual URI
// identity is authoritative; a mislabeled provider never gains a receiver.
func NormalizeResource(declared, raw, password string) (string, string, string, error) {
	if len(raw) > 8192 || strings.ContainsAny(raw, "\x00\r\n") {
		return "", "", "", site.ErrInvalidReply
	}
	declared = strings.ToLower(strings.TrimSpace(declared))
	accept := func(kind, normalized, identity string) (string, string, string, error) {
		if declared != "" && declared != kind {
			return "", "", "", site.ErrInvalidReply
		}
		return kind, normalized, identity, nil
	}
	if normalized, identity, err := quark.NormalizeShareLink(raw, password); err == nil {
		return accept("quark", normalized, identity)
	}
	if normalized, identity, err := pan115.NormalizeShareLink(raw, password); err == nil {
		return accept("115", normalized, identity)
	}
	if normalized, ok := btrss.NormalizeMagnet(raw); ok {
		u, _ := url.Parse(normalized)
		identity := strings.TrimPrefix(strings.ToLower(u.Query().Get("xt")), "urn:btih:")
		if len(identity) == 32 {
			decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(identity))
			if err != nil || len(decoded) != 20 {
				return "", "", "", site.ErrInvalidReply
			}
			identity = hex.EncodeToString(decoded)
		}
		return accept("magnet", "magnet:?xt=urn:btih:"+identity, identity)
	}
	if ed2kPattern.MatchString(raw) {
		parts := strings.Split(raw, "|")
		return accept("ed2k", raw, strings.ToLower(parts[4])+":"+parts[3])
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Port() != "" || len(u.Path) > 1024 {
		return "", "", "", site.ErrInvalidReply
	}
	host := strings.ToLower(u.Hostname())
	kind := ""
	switch host {
	case "www.aliyundrive.com", "aliyundrive.com", "www.alipan.com", "alipan.com":
		kind = "aliyun"
	case "pan.baidu.com":
		kind = "baidu"
	case "cloud.189.cn":
		kind = "tianyi"
	case "drive.uc.cn":
		kind = "uc"
	case "pan.xunlei.com":
		kind = "xunlei"
	case "www.123pan.com", "123pan.com", "www.123684.com", "123684.com", "www.123865.com", "123865.com":
		kind = "123"
	case "mypikpak.com", "www.mypikpak.com":
		kind = "pikpak"
	default:
		if declared != "others" || u.Hostname() == "" || u.Opaque != "" || strings.Contains(u.Hostname(), "\\") {
			return "", "", "", site.ErrInvalidReply
		}
		// Other public HTTP(S) links are display-only. They never enter a
		// downloader or cause this adapter to fetch the result URL.
		u.Fragment = ""
		return accept("others", u.String(), u.String())
	}
	if !strings.HasPrefix(u.Path, "/s/") && !(kind == "tianyi" && (strings.HasPrefix(u.Path, "/t/") || u.Path == "/web/share")) {
		return "", "", "", site.ErrInvalidReply
	}
	u.Host = host
	u.Fragment = ""
	query := url.Values{}
	for _, key := range []string{"pwd", "password", "accessCode", "code"} {
		value := u.Query().Get(key)
		if value != "" && len(value) <= 64 && !strings.ContainsAny(value, "\x00\r\n") {
			query.Set(key, value)
		}
	}
	if password != "" && len(password) <= 64 && !strings.ContainsAny(password, "\x00\r\n") {
		if kind == "baidu" {
			query.Set("pwd", password)
		} else {
			query.Set("password", password)
		}
	}
	u.RawQuery = query.Encode()
	identity := host + u.EscapedPath()
	if kind == "tianyi" && u.Path == "/web/share" {
		// This route carries the share identity in code, not in the path.
		// Dropping it would merge unrelated shares and their extraction codes.
		if query.Get("code") == "" {
			return "", "", "", site.ErrInvalidReply
		}
		identity += "?code=" + url.QueryEscape(query.Get("code"))
	}
	return accept(kind, u.String(), identity)
}

func parseTime(value string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05"} {
		if stamp, err := time.Parse(layout, value); err == nil && !stamp.IsZero() {
			return &stamp
		}
	}
	return nil
}
