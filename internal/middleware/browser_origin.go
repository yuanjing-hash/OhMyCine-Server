package middleware

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// BrowserOriginAllowed checks browser provenance for mutations and WebSocket
// upgrades. Browser-supplied same-origin metadata survives reverse proxies that
// rewrite Host or terminate TLS; forwarded headers are never trust evidence.
func BrowserOriginAllowed(request *http.Request, allowedOrigins []string) bool {
	if len(request.Header.Values("Sec-Fetch-Site")) > 1 {
		return false
	}
	site := strings.ToLower(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site")))
	if site == "cross-site" {
		return false
	}

	var origin *url.URL
	var originValue string
	if values := request.Header.Values("Origin"); len(values) != 0 {
		if len(values) != 1 {
			return false
		}
		originValue = strings.TrimSpace(values[0])
		var err error
		origin, err = url.Parse(originValue)
		if err != nil || !validBrowserOriginURL(origin) || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || strings.Contains(originValue, "#") {
			return false
		}
	}
	if site == "same-origin" {
		return true
	}
	if origin == nil && request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions {
		// Referer is a fallback for older browsers' mutations, never for a
		// WebSocket handshake. An invalid supplied Origin cannot use it.
		values := request.Header.Values("Referer")
		if len(values) != 1 {
			return false
		}
		var err error
		origin, err = url.Parse(strings.TrimSpace(values[0]))
		if err != nil || !validBrowserOriginURL(origin) {
			return false
		}
		originValue = origin.Scheme + "://" + origin.Host
	}
	if origin == nil {
		return false
	}
	for _, allowed := range allowedOrigins {
		if originValue == strings.TrimRight(strings.TrimSpace(allowed), "/") {
			return true
		}
	}
	// Same-site can still be cross-origin (including HTTP to HTTPS). Only an
	// explicitly configured origin may override that browser evidence.
	return site != "same-site" && strings.EqualFold(origin.Host, request.Host)
}

func validBrowserOriginURL(origin *url.URL) bool {
	if origin == nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Opaque != "" || origin.User != nil || origin.Hostname() == "" || origin.Fragment != "" || strings.ContainsAny(origin.Host, "\\, \t\r\n") || strings.HasSuffix(origin.Host, ":") {
		return false
	}
	if port := origin.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
	}
	if strings.HasPrefix(origin.Host, "[") {
		if !strings.Contains(origin.Hostname(), ":") || net.ParseIP(origin.Hostname()) == nil {
			return false
		}
	} else if strings.Contains(origin.Hostname(), ":") {
		return false
	}
	return true
}
