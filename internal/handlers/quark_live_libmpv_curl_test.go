//go:build quark_live

package handlers_test

// This diagnostic exercises the actual DLL's curl backend. It has a child
// lifetime deadline and an 8 MiB per-request Range setting, NOT an aggregate
// media-request or network-byte cap. No product service is started.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const quarkLibmpvExpectedSize int64 = 1031623085
const quarkLibmpvSamplePath = "/视频/假如 漫威宇宙系列 1080 中英双字"
const quarkLibmpvDLLSHA = "7d79f8fb29fc1111e9ed986486916ebc4cb8cdf4449a10c674285e1aad8d0f5b"

type quarkLibmpvCurlInput struct {
	Version      int    `json:"version"`
	URL          string `json:"url"`
	ExpectedSize int64  `json:"expected_size"`
}

type quarkLibmpvCurlOutput struct {
	Version                 int      `json:"version"`
	DLLSHA256               string   `json:"dll_sha256"`
	Outcome                 string   `json:"outcome"`
	ElapsedMS               int64    `json:"elapsed_ms"`
	LoadResult              *int     `json:"load_result"`
	FileLoaded              bool     `json:"file_loaded"`
	PlayedAny               bool     `json:"played_any"`
	SeekRequested           bool     `json:"seek_requested"`
	SeekResult              *int     `json:"seek_result"`
	TimePosBeforeSeek       *float64 `json:"time_pos_before_seek"`
	TimePosAfterSeekMax     *float64 `json:"time_pos_after_seek_max"`
	ReportedFileSize        *int64   `json:"reported_file_size"`
	SizeMatchesExpected     bool     `json:"size_matches_expected"`
	PreSeekSamples          int      `json:"pre_seek_samples"`
	PostSeekSamples         int      `json:"post_seek_samples"`
	CacheSpeedPreMax        *float64 `json:"cache_speed_pre_max"`
	CacheSpeedPostMax       *float64 `json:"cache_speed_post_max"`
	BufferingSamples        int      `json:"buffering_samples"`
	EndReason               *int     `json:"end_reason"`
	EndError                *int     `json:"end_error"`
	NativeDestroyed         bool     `json:"native_destroyed"`
	RequestCountKnown       bool     `json:"request_count_known"`
	TotalReceivedBytesKnown bool     `json:"total_received_bytes_known"`
	OriginalContentVerified bool     `json:"original_content_verified"`
}

func quarkLibmpvInputValid(in quarkLibmpvCurlInput) bool {
	u, err := url.Parse(in.URL)
	return in.Version == 1 && in.ExpectedSize == quarkLibmpvExpectedSize && err == nil &&
		(u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil &&
		u.Fragment == "" && u.Opaque == "" && !strings.ContainsAny(in.URL, "\\\r\n\x00")
}

func quarkLibmpvOutputValid(out quarkLibmpvCurlOutput) bool {
	return quarkLibmpvOutputValidProfile(out, false)
}

func quarkLibmpvOutputValidProfile(out quarkLibmpvCurlOutput, bridge bool) bool {
	maximumElapsed := int64(25000)
	if bridge {
		maximumElapsed = 35000
	}
	allowed := map[string]bool{"completed": true, "invalid_input": true, "dll_mismatch": true, "native_error": true, "native_timeout": true}
	if out.Version != 1 || out.DLLSHA256 != quarkLibmpvDLLSHA || !allowed[out.Outcome] ||
		out.ElapsedMS < 0 || out.ElapsedMS > maximumElapsed || out.PreSeekSamples < 0 || out.PostSeekSamples < 0 ||
		out.PreSeekSamples+out.PostSeekSamples > 400 || out.BufferingSamples < 0 ||
		out.BufferingSamples > out.PreSeekSamples+out.PostSeekSamples || out.RequestCountKnown || out.TotalReceivedBytesKnown || out.OriginalContentVerified {
		return false
	}
	for _, value := range []*float64{out.TimePosBeforeSeek, out.TimePosAfterSeekMax, out.CacheSpeedPreMax, out.CacheSpeedPostMax} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1<<40) {
			return false
		}
	}
	for _, value := range []*int{out.LoadResult, out.SeekResult, out.EndError} {
		if value != nil && (*value < -1024 || *value > 0) {
			return false
		}
	}
	if out.EndReason != nil && (*out.EndReason < 0 || *out.EndReason > 5) || (out.EndReason == nil) != (out.EndError == nil) ||
		!out.SeekRequested && (out.SeekResult != nil || out.PostSeekSamples != 0 || out.TimePosAfterSeekMax != nil) {
		return false
	}
	if out.ReportedFileSize != nil && *out.ReportedFileSize < 0 || out.SizeMatchesExpected != (out.ReportedFileSize != nil && *out.ReportedFileSize == quarkLibmpvExpectedSize) {
		return false
	}
	return out.Outcome != "completed" || out.NativeDestroyed
}

// Only validated typed output is returned. Raw stdout/stderr, OS errors and
// decoder errors may contain a signed URL and are never forwarded.
func quarkRunLibmpvCurl(ctx context.Context, input quarkLibmpvCurlInput, start func(context.Context) *exec.Cmd) (quarkLibmpvCurlOutput, error) {
	var out quarkLibmpvCurlOutput
	payload, err := json.Marshal(input)
	if err != nil || len(payload) > 16<<10 || !quarkLibmpvInputValid(input) {
		return out, errors.New("libmpv_curl_input_invalid")
	}
	return quarkRunLibmpvCurlPayload(ctx, payload, 25*time.Second, start)
}

func quarkRunLibmpvCurlPayload(ctx context.Context, payload []byte, limit time.Duration, start func(context.Context) *exec.Cmd) (quarkLibmpvCurlOutput, error) {
	var out quarkLibmpvCurlOutput
	if len(payload) == 0 || len(payload) > 16<<10 || limit <= 0 || limit > 50*time.Second {
		return out, errors.New("libmpv_curl_input_invalid")
	}
	childContext, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := start(childContext)
	cmd.Stdin, cmd.Stderr = bytes.NewReader(payload), io.Discard
	var captured quarkNativeBoundedOutput
	cmd.Stdout = &captured
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	if childContext.Err() != nil {
		return out, errors.New("libmpv_curl_child_timeout")
	}
	if captured.overflow {
		return out, errors.New("libmpv_curl_output_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(captured.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&out) != nil || decoder.Decode(new(any)) != io.EOF || !quarkLibmpvOutputValidProfile(out, limit == 50*time.Second) {
		return quarkLibmpvCurlOutput{}, errors.New("libmpv_curl_output_invalid")
	}
	// All fields, including null measurements, are required. Missing false-valued
	// boundary flags must not silently become asserted measurement semantics.
	var fields map[string]json.RawMessage
	encoded, _ := json.Marshal(out)
	var required map[string]json.RawMessage
	if json.Unmarshal(captured.Bytes(), &fields) != nil || json.Unmarshal(encoded, &required) != nil || len(fields) != len(required) {
		return quarkLibmpvCurlOutput{}, errors.New("libmpv_curl_output_invalid")
	}
	nullable := map[string]bool{"load_result": true, "seek_result": true, "time_pos_before_seek": true, "time_pos_after_seek_max": true, "reported_file_size": true, "cache_speed_pre_max": true, "cache_speed_post_max": true, "end_reason": true, "end_error": true}
	for key, value := range fields {
		if !nullable[key] && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return quarkLibmpvCurlOutput{}, errors.New("libmpv_curl_output_invalid")
		}
	}
	expectedExit := 0
	if out.Outcome == "native_error" || out.Outcome == "native_timeout" {
		expectedExit = 1
	} else if out.Outcome != "completed" {
		expectedExit = 2
	}
	actualExit := 0
	if runErr != nil {
		var exit *exec.ExitError
		if !errors.As(runErr, &exit) {
			return quarkLibmpvCurlOutput{}, errors.New("libmpv_curl_child_failed")
		}
		actualExit = exit.ExitCode()
	}
	if actualExit != expectedExit {
		return quarkLibmpvCurlOutput{}, errors.New("libmpv_curl_child_failed")
	}
	return out, nil
}

func quarkLibmpvFindSample(ctx context.Context, samplePath string, list func(context.Context, string, cloud.PageRequest) (cloud.Page, error)) (cloud.Item, error) {
	if samplePath != quarkLibmpvSamplePath {
		return cloud.Item{}, errors.New("libmpv_curl_authorized_sample_required")
	}
	parent := "0"
	for _, wanted := range strings.Split(strings.Trim(samplePath, "/"), "/") {
		page, err := list(ctx, parent, cloud.PageRequest{Limit: 100})
		if err != nil || page.HasMore || page.Offset != 0 || len(page.Items) > 100 {
			return cloud.Item{}, errors.New("libmpv_curl_directory_incomplete")
		}
		matches := 0
		for _, item := range page.Items {
			if item.IsDir && item.Name == wanted && item.ID != "" {
				parent, matches = item.ID, matches+1
			}
		}
		if matches != 1 {
			return cloud.Item{}, errors.New("libmpv_curl_directory_not_unique")
		}
	}
	for depth := 0; depth < 3; depth++ {
		page, err := list(ctx, parent, cloud.PageRequest{Limit: 100})
		if err != nil || page.HasMore || page.Offset != 0 || len(page.Items) > 100 {
			return cloud.Item{}, errors.New("libmpv_curl_sample_incomplete")
		}
		var sample cloud.Item
		var children []cloud.Item
		matches := 0
		for _, item := range page.Items {
			if item.IsDir {
				children = append(children, item)
			} else if item.ID != "" && item.Size == quarkLibmpvExpectedSize && strings.EqualFold(filepath.Ext(item.Name), ".mkv") {
				sample, matches = item, matches+1
			}
		}
		if matches == 1 {
			return sample, nil
		}
		if matches > 1 || len(children) != 1 || children[0].ID == "" {
			return cloud.Item{}, errors.New("libmpv_curl_sample_not_unique")
		}
		parent = children[0].ID
	}
	return cloud.Item{}, errors.New("libmpv_curl_sample_missing")
}

func TestQuarkLiveLibmpvCurl(t *testing.T) {
	if os.Getenv("OMC_QUARK_LIBMPV_CURL_ENABLED") != "1" {
		t.Skip("explicit time-limited actual DLL curl diagnostic required")
	}
	dir, samplePath := os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_LIVE_SAMPLE_PATH")
	python, probe := os.Getenv("OMC_QUARK_LIBMPV_PYTHON"), os.Getenv("OMC_QUARK_LIBMPV_PROBE")
	if dir == "" || samplePath != quarkLibmpvSamplePath || filepath.Base(probe) != "live_curl_probe.py" {
		t.Fatal("libmpv_curl_setup_invalid")
	}
	for _, local := range []string{python, probe} {
		info, err := os.Stat(local)
		if !filepath.IsAbs(local) || err != nil || !info.Mode().IsRegular() {
			t.Fatal("libmpv_curl_local_helper_unavailable")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	requests := 0
	var mu sync.Mutex
	guard := func(callContext context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if requests >= 40 {
			return errors.New("live request budget exceeded")
		}
		requests++
		select {
		case <-callContext.Done():
			return callContext.Err()
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
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal("libmpv_curl_credentials_invalid")
	}
	driver, err := quark.NewClient(cloud.Config{Credentials: raw, BeforeCall: guard})
	if err != nil {
		t.Fatal("libmpv_curl_driver_invalid")
	}
	sample, err := quarkLibmpvFindSample(ctx, samplePath, driver.List)
	if err != nil {
		t.Fatal(err.Error()) // fixed local enums, never the underlying error
	}
	link, err := driver.DirectURL(ctx, cloud.DirectURLRequest{FileID: sample.ID})
	if err != nil || len(link.Headers) != 0 {
		t.Fatal("libmpv_curl_original_link_unavailable")
	}
	t.Logf("libmpv_curl_setup api_requests=%d renewed_or_recovered=%t file_size=%d child_deadline_seconds=18 range_request_size=8388608 aggregate_media_count_known=false aggregate_media_bytes_known=false", requests, renewed, sample.Size)
	result, err := quarkRunLibmpvCurl(ctx, quarkLibmpvCurlInput{Version: 1, URL: link.URL, ExpectedSize: sample.Size}, func(childContext context.Context) *exec.Cmd {
		return exec.CommandContext(childContext, python, "-I", "-B", probe)
	})
	if err != nil {
		t.Fatal(err.Error())
	}
	encoded, _ := json.Marshal(result)
	t.Logf("libmpv_curl_result %s", encoded)
	t.Logf("libmpv_curl_finished api_requests=%d media_sessions=1 aggregate_media_requests_and_bytes_unmeasured=true", requests)
	if result.Outcome != "completed" {
		t.Fatal("libmpv_curl_probe_incomplete")
	}
}
