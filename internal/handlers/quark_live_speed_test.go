//go:build quark_live

package handlers_test

// An explicit, bounded, media-read-only throughput diagnostic. This is not a
// product playback proxy and does not claim native Player/Emby acceptance.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkSpeedResult struct {
	Variant              string  `json:"variant"`
	Route                string  `json:"route"`
	ProxyMode            string  `json:"proxy_mode"`
	EnvironmentProxyUsed bool    `json:"environment_proxy_selected"`
	Status               int     `json:"status"`
	RangeStart           int64   `json:"range_start"`
	RangeEnd             int64   `json:"range_end"`
	FileSize             int64   `json:"file_size"`
	RequestedBytes       int64   `json:"read_cap_bytes"`
	RangeSpanBytes       int64   `json:"range_span_bytes"`
	Bytes                int64   `json:"body_bytes"`
	ElapsedMS            float64 `json:"elapsed_ms"`
	TTFBMS               float64 `json:"response_ttfb_ms"`
	FirstBodyMS          float64 `json:"first_body_ms"`
	KiBPerSecond         float64 `json:"kib_per_second"`
	BodyKiBPerSecond     float64 `json:"body_kib_per_second"`
	Complete             bool    `json:"sample_complete"`
	WholeRangeConsumed   bool    `json:"whole_range_consumed"`
	Outcome              string  `json:"outcome"`
	SHA256               string  `json:"sha256,omitempty"`
}

type quarkSpeedOptions struct {
	bytes       int64
	timeout     time.Duration
	variants    []string
	playerUA    string
	proxyMode   string
	nativeProbe string
}

func quarkSpeedOptionsFromEnv() (quarkSpeedOptions, error) {
	out := quarkSpeedOptions{bytes: 32 << 20, timeout: 20 * time.Second, variants: []string{"go-head"}, proxyMode: "environment"}
	for _, field := range []struct {
		name string
		max  int64
		set  func(int64)
	}{
		{"OMC_QUARK_SPEED_MIB", 32, func(n int64) { out.bytes = n << 20 }},
		{"OMC_QUARK_SPEED_SECONDS", 60, func(n int64) { out.timeout = time.Duration(n) * time.Second }},
	} {
		if value := os.Getenv(field.name); value != "" {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 1 || n > field.max {
				return out, errors.New("speed_limit_invalid")
			}
			field.set(n)
		}
	}
	if value := os.Getenv("OMC_QUARK_SPEED_VARIANTS"); value != "" {
		out.variants = strings.Split(value, ",")
	}
	if len(out.variants) == 0 || len(out.variants) > 4 {
		return out, errors.New("speed_variants_invalid")
	}
	seen := map[string]bool{}
	for _, variant := range out.variants {
		if seen[variant] || variant != "go-head" && variant != "go-middle" && variant != "go-open-head" && variant != "go-open-middle" && variant != "go-explicit-eof-head" && variant != "player-head" && variant != "player-middle" && variant != "native-reqwest-head" && variant != "native-bridge-head" && variant != "native-reqwest-open-head" && variant != "native-bridge-open-head" {
			return out, errors.New("speed_variants_invalid")
		}
		seen[variant] = true
	}
	out.playerUA = os.Getenv("OMC_QUARK_SPEED_PLAYER_UA")
	if seen["player-head"] || seen["player-middle"] {
		if out.playerUA == "" || len(out.playerUA) > 256 {
			return out, errors.New("speed_player_ua_required")
		}
		for _, c := range out.playerUA {
			if c < 32 || c > 126 {
				return out, errors.New("speed_player_ua_invalid")
			}
		}
	}
	if value := os.Getenv("OMC_QUARK_SPEED_PROXY_MODE"); value != "" {
		out.proxyMode = value
	}
	if out.proxyMode != "environment" && out.proxyMode != "direct" {
		return out, errors.New("speed_proxy_mode_invalid")
	}
	out.nativeProbe = os.Getenv("OMC_QUARK_SPEED_NATIVE_PROBE")
	for _, variant := range out.variants {
		if !quarkNativeVariant(variant) {
			continue
		}
		info, err := os.Stat(out.nativeProbe)
		if err != nil || !info.Mode().IsRegular() || !filepath.IsAbs(out.nativeProbe) || out.timeout > 20*time.Second || out.proxyMode != "environment" {
			return out, errors.New("speed_native_probe_invalid")
		}
	}
	return out, nil
}

// Requests have only Range, identity encoding and the explicitly selected UA.
// There is no cookie jar, Authorization, Referer or provider-response echo.
// Redirects are rejected rather than granting another host access to the URL.
func quarkSpeedClient(mode string) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.MaxConnsPerHost = 1
	transport.MaxIdleConnsPerHost = 1
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.Proxy = http.ProxyFromEnvironment
	if mode == "direct" {
		transport.Proxy = nil
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func quarkSpeedMeasure(ctx context.Context, client *http.Client, rawURL, variant, ua, proxyMode string, start, count, size int64, timeout time.Duration) (out quarkSpeedResult) {
	out = quarkSpeedResult{Variant: variant, Route: "production_tv_original_direct", ProxyMode: proxyMode, RangeStart: start, RangeEnd: start + count - 1, FileSize: size, RequestedBytes: count, RangeSpanBytes: count, Outcome: "request_invalid"}
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || strings.ContainsAny(rawURL, "\\\r\n") || count < 1 || count > 32<<20 || start < 0 || start > size-count || timeout <= 0 || timeout > time.Minute {
		return out
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	var firstResponse time.Time
	requestContext = httptrace.WithClientTrace(requestContext, &httptrace.ClientTrace{GotFirstResponseByte: func() { firstResponse = time.Now() }})
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, rawURL, nil)
	if err != nil {
		return out
	}
	openEnded := variant == "go-open-head" || variant == "go-open-middle"
	if openEnded || variant == "go-explicit-eof-head" {
		// Compare the same full remaining range in open and explicit-EOF forms.
		// Both retain the exact body-read/time ceiling; neither consumes the file.
		out.RangeEnd, out.RangeSpanBytes = size-1, size-start
	}
	if openEnded {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	} else {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, out.RangeEnd))
	}
	req.Header.Set("Accept-Encoding", "identity")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if proxyMode == "environment" {
		proxy, proxyErr := http.ProxyFromEnvironment(req)
		out.EnvironmentProxyUsed = proxyErr == nil && proxy != nil
	}
	defer func() {
		elapsed := time.Since(started)
		out.ElapsedMS = float64(elapsed) / float64(time.Millisecond)
		if !firstResponse.IsZero() {
			out.TTFBMS = float64(firstResponse.Sub(started)) / float64(time.Millisecond)
		}
		if elapsed > 0 {
			out.KiBPerSecond = float64(out.Bytes) / 1024 / elapsed.Seconds()
		}
		bodySeconds := elapsed.Seconds() - out.FirstBodyMS/1000
		if out.FirstBodyMS > 0 && bodySeconds > 0 {
			out.BodyKiBPerSecond = float64(out.Bytes) / 1024 / bodySeconds
		}
	}()
	response, err := client.Do(req)
	if err != nil {
		out.Outcome = "transport_failed"
		if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			out.Outcome = "request_timeout"
		}
		return out
	}
	defer response.Body.Close()
	out.Status = response.StatusCode
	if response.StatusCode == 401 || response.StatusCode == 402 || response.StatusCode == 403 || response.StatusCode == 451 {
		out.Outcome = "access_rejected"
		return out
	}
	if response.StatusCode == 429 {
		out.Outcome = "rate_limited"
		return out
	}
	if response.StatusCode != http.StatusPartialContent {
		out.Outcome = "range_rejected"
		return out
	}
	if response.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, out.RangeEnd, size) || response.ContentLength >= 0 && response.ContentLength != out.RangeSpanBytes || response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		out.Outcome = "range_metadata_invalid"
		return out
	}
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	for out.Bytes < count {
		readSize := min(int64(len(buffer)), count-out.Bytes)
		n, readErr := response.Body.Read(buffer[:readSize])
		if n > 0 {
			if out.FirstBodyMS == 0 {
				out.FirstBodyMS = float64(time.Since(started)) / float64(time.Millisecond)
			}
			_, _ = hash.Write(buffer[:n])
			out.Bytes += int64(n)
		}
		if out.Bytes == count {
			out.Complete, out.Outcome, out.SHA256 = true, "complete", hex.EncodeToString(hash.Sum(nil))
			out.WholeRangeConsumed = out.Bytes == out.RangeSpanBytes
			return out
		}
		if readErr != nil {
			out.Outcome = "body_incomplete"
			if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
				out.Outcome = "body_timeout"
			} else if requestContext.Err() != nil {
				out.Outcome = "cancelled"
			} else if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
				out.Outcome = "body_read_failed"
			}
			return out
		}
	}
	return out
}

func TestQuarkLiveSpeed(t *testing.T) {
	// A distinct explicit switch prevents broad quark_live test invocations from
	// unexpectedly reading many MiB when only the 64 KiB acceptance was intended.
	if os.Getenv("OMC_QUARK_SPEED_ENABLED") != "1" {
		t.Skip("explicit bounded speed diagnostic required")
	}
	opts, err := quarkSpeedOptionsFromEnv()
	if err != nil {
		t.Fatal(err.Error()) // fixed local codes only
	}
	dir, samplePath := os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_LIVE_SAMPLE_PATH")
	if dir == "" || !strings.HasPrefix(samplePath, "/") || strings.ContainsAny(samplePath, "\\\r\n") {
		t.Fatal("speed_authorized_sample_required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second+time.Duration(len(opts.variants))*opts.timeout)
	defer cancel()
	requests := 0
	var mu sync.Mutex
	guard := func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if requests >= 40 {
			return errors.New("live request budget exceeded")
		}
		requests++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	}
	state, err := openLiveTVStore(dir)
	if err != nil {
		t.Fatal(liveTVMessage(err))
	}
	defer state.lock.Close()
	defer func() {
		if err := state.unchanged(); err != nil {
			t.Error(liveTVMessage(err))
		}
	}()
	bundle, renewed, err := state.ready(ctx, quark.NewAuthenticator(cloud.Config{BeforeCall: guard}))
	if err != nil {
		t.Fatal(liveTVMessage(err))
	}
	raw, _ := json.Marshal(bundle)
	driver, err := quark.NewClient(cloud.Config{Credentials: raw, BeforeCall: guard})
	if err != nil {
		t.Fatal("speed_driver_invalid")
	}
	parent := "0"
	var root, sample cloud.Item
	for _, wanted := range strings.Split(strings.Trim(samplePath, "/"), "/") {
		if wanted == "" || wanted == "." || wanted == ".." {
			t.Fatal("speed_sample_path_invalid")
		}
		page, listErr := driver.List(ctx, parent, cloud.PageRequest{Limit: 100})
		if listErr != nil || page.HasMore {
			t.Fatal("speed_sample_lookup_incomplete")
		}
		matches := 0
		for _, item := range page.Items {
			if item.IsDir && item.Name == wanted {
				root, matches = item, matches+1
			}
		}
		if matches != 1 {
			t.Fatal("speed_sample_directory_not_unique")
		}
		parent = root.ID
	}
	for depth := 0; depth < 3; depth++ {
		page, listErr := driver.List(ctx, root.ID, cloud.PageRequest{Limit: 100})
		if listErr != nil || page.HasMore {
			t.Fatal("speed_sample_listing_incomplete")
		}
		var children []cloud.Item
		for _, item := range page.Items {
			if item.IsDir {
				children = append(children, item)
			} else if sample.ID == "" && strings.EqualFold(filepath.Ext(item.Name), ".mkv") {
				sample = item
			}
		}
		if sample.ID != "" {
			break
		}
		if len(children) != 1 {
			t.Fatal("speed_sample_descent_not_unique")
		}
		root = children[0]
	}
	if sample.ID == "" || sample.Size < 131072 {
		t.Fatal("speed_sample_unavailable")
	}
	link, err := driver.DirectURL(ctx, cloud.DirectURLRequest{FileID: sample.ID})
	if err != nil || len(link.Headers) != 0 {
		t.Fatal("speed_original_link_unavailable")
	}
	client := quarkSpeedClient(opts.proxyMode)
	defer client.CloseIdleConnections()
	t.Logf("speed_setup api_requests=%d renewed_or_recovered=%t variants=%d max_bytes_per_request=%d max_seconds_per_request=%g", requests, renewed, len(opts.variants), opts.bytes, opts.timeout.Seconds())
	for _, variant := range opts.variants {
		start := int64(0)
		if strings.HasSuffix(variant, "-middle") {
			start = sample.Size / 2
		}
		count := min(opts.bytes, sample.Size-start)
		if quarkNativeVariant(variant) {
			input := quarkNativeInput(variant, link.URL, start, count, sample.Size, opts.timeout)
			result, nativeErr := quarkRunNativeSpeed(ctx, input, func(childContext context.Context) *exec.Cmd {
				return exec.CommandContext(childContext, opts.nativeProbe)
			})
			if nativeErr != nil {
				t.Fatal(nativeErr.Error()) // fixed local errors only
			}
			encoded, _ := json.Marshal(result)
			t.Logf("native_speed_result variant=%s %s", variant, encoded)
			if result.Status == 401 || result.Status == 402 || result.Status == 403 || result.Status == 451 || result.Status == 429 {
				t.Fatal("speed_stopped_provider_rejection")
			}
			if !result.Completed && result.Error != "timeout" && result.Error != "short_body" {
				t.Fatal("speed_stopped_invalid_response")
			}
			continue
		}
		ua := "" // actual Go HTTP default, not a made-up Player version
		if strings.HasPrefix(variant, "player-") {
			ua = opts.playerUA
		}
		result := quarkSpeedMeasure(ctx, client, link.URL, variant, ua, opts.proxyMode, start, count, sample.Size, opts.timeout)
		encoded, _ := json.Marshal(result)
		t.Logf("speed_result %s", encoded)
		if result.Outcome == "access_rejected" || result.Outcome == "rate_limited" {
			t.Fatal("speed_stopped_provider_rejection")
		}
		if result.Outcome != "complete" && result.Outcome != "body_timeout" && result.Outcome != "body_incomplete" {
			t.Fatal("speed_stopped_invalid_response")
		}
	}
	t.Logf("speed_finished api_requests=%d media_sample_count=%d concurrency=1", requests, len(opts.variants))
}
