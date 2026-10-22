//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// Pure subprocess fixture: no cloud, local state or media networking.
func TestQuarkLibmpvCurlProcessFixture(t *testing.T) {
	mode := os.Getenv("OMC_LIBMPV_CURL_SYNTHETIC_CHILD")
	if mode == "" {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, (16<<10)+1))
	var in quarkLibmpvCurlInput
	if len(raw) > 16<<10 || json.Unmarshal(raw, &in) != nil || in.URL != "https://cdn.invalid/original?token=synthetic-private" || in.ExpectedSize != quarkLibmpvExpectedSize {
		os.Exit(3)
	}
	fmt.Fprintln(os.Stderr, "https://cdn.invalid/?token=synthetic-private must never be returned")
	size, zero := quarkLibmpvExpectedSize, 0
	out := quarkLibmpvCurlOutput{Version: 1, DLLSHA256: quarkLibmpvDLLSHA, Outcome: "completed", ElapsedMS: 12, LoadResult: &zero, FileLoaded: true, PlayedAny: true, ReportedFileSize: &size, SizeMatchesExpected: true, PreSeekSamples: 1, NativeDestroyed: true}
	exitCode := 0
	switch mode {
	case "oversized":
		fmt.Print(strings.Repeat("x", 17<<10))
		os.Exit(1)
	case "timeout":
		out.Outcome, out.NativeDestroyed, exitCode = "native_timeout", false, 1
	case "native_error":
		out.Outcome, exitCode = "native_error", 1
	case "invalid_input":
		out.Outcome, exitCode = "invalid_input", 2
	case "dll_mismatch":
		out.Outcome, exitCode = "dll_mismatch", 2
	case "secret-outcome":
		out.Outcome = "synthetic-private"
	case "wrong-hash":
		out.DLLSHA256 = "synthetic-private"
	case "claimed-bytes":
		out.TotalReceivedBytesKnown = true
	case "claimed-verified":
		out.OriginalContentVerified = true
	case "wrong-size":
		wrongSize := int64(100)
		out.ReportedFileSize = &wrongSize
	case "unclosed":
		out.NativeDestroyed = false
	case "bad-exit":
		exitCode = 4
	case "hang":
		time.Sleep(10 * time.Second)
	}
	encoded, _ := json.Marshal(out)
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	switch mode {
	case "extra-field":
		fields["url"] = in.URL
	case "missing-field":
		delete(fields, "request_count_known")
	case "null-flag":
		fields["request_count_known"] = nil
	}
	_ = json.NewEncoder(os.Stdout).Encode(fields)
	if mode == "trailing-output" {
		fmt.Print("synthetic-private")
	}
	os.Exit(exitCode)
}

func TestQuarkLibmpvCurlProcessContract(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal("test executable unavailable")
	}
	for _, tc := range []struct{ mode, code string }{
		{"complete", ""}, {"timeout", ""}, {"native_error", ""}, {"invalid_input", ""}, {"dll_mismatch", ""},
		{"oversized", "libmpv_curl_output_invalid"}, {"secret-outcome", "libmpv_curl_output_invalid"},
		{"wrong-hash", "libmpv_curl_output_invalid"}, {"claimed-bytes", "libmpv_curl_output_invalid"},
		{"claimed-verified", "libmpv_curl_output_invalid"}, {"wrong-size", "libmpv_curl_output_invalid"},
		{"unclosed", "libmpv_curl_output_invalid"}, {"extra-field", "libmpv_curl_output_invalid"},
		{"missing-field", "libmpv_curl_output_invalid"}, {"null-flag", "libmpv_curl_output_invalid"},
		{"trailing-output", "libmpv_curl_output_invalid"}, {"bad-exit", "libmpv_curl_child_failed"},
		{"hang", "libmpv_curl_child_timeout"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx := context.Background()
			if tc.mode == "hang" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			result, err := quarkRunLibmpvCurl(ctx, quarkLibmpvCurlInput{Version: 1, URL: "https://cdn.invalid/original?token=synthetic-private", ExpectedSize: quarkLibmpvExpectedSize}, func(ctx context.Context) *exec.Cmd {
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestQuarkLibmpvCurlProcessFixture$")
				cmd.Env = append(os.Environ(), "OMC_LIBMPV_CURL_SYNTHETIC_CHILD="+tc.mode)
				return cmd
			})
			if tc.code != "" {
				if err == nil || err.Error() != tc.code {
					t.Fatalf("expected safe code %s", tc.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "synthetic-private") || result.RequestCountKnown || result.TotalReceivedBytesKnown || result.OriginalContentVerified {
				t.Fatal("secret or unsupported measurement escaped")
			}
		})
	}
}

func TestQuarkLibmpvCurlInvalidInputNeverStartsChild(t *testing.T) {
	for _, input := range []quarkLibmpvCurlInput{
		{Version: 1, URL: "https://cdn.invalid/file", ExpectedSize: 1},
		{Version: 2, URL: "https://cdn.invalid/file", ExpectedSize: quarkLibmpvExpectedSize},
		{Version: 1, URL: "https://user:secret@cdn.invalid/file", ExpectedSize: quarkLibmpvExpectedSize},
		{Version: 1, URL: "file:///private", ExpectedSize: quarkLibmpvExpectedSize},
		{Version: 1, URL: "https://cdn.invalid/file#secret", ExpectedSize: quarkLibmpvExpectedSize},
		{Version: 1, URL: "https://cdn.invalid/" + strings.Repeat("x", 16<<10), ExpectedSize: quarkLibmpvExpectedSize},
	} {
		_, err := quarkRunLibmpvCurl(context.Background(), input, func(context.Context) *exec.Cmd {
			t.Fatal("invalid input reached child")
			return nil
		})
		if err == nil || err.Error() != "libmpv_curl_input_invalid" {
			t.Fatal("invalid input not rejected safely")
		}
	}
	if !quarkLibmpvInputValid(quarkLibmpvCurlInput{Version: 1, URL: "http://cdn.invalid/file", ExpectedSize: quarkLibmpvExpectedSize}) {
		t.Fatal("HTTP was rejected only for its scheme")
	}
}

func TestQuarkLibmpvCurlPythonSchemaWithoutNetwork(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("bundled Python is unavailable")
	}
	python := filepath.Join(home, ".cache", "codex-runtimes", "codex-primary-runtime", "dependencies", "python", "python.exe")
	probe, err := filepath.Abs("../../../validation/libmpv-http-probe/live_curl_probe.py")
	if err != nil {
		t.Fatal("local helper path unavailable")
	}
	for _, local := range []string{python, probe} {
		if info, err := os.Stat(local); err != nil || !info.Mode().IsRegular() {
			t.Skip("local Python/schema helper unavailable")
		}
	}
	// This deliberately unsupported argument selects Python's immediate fixed
	// invalid_input result, before stdin, DLL loading, child creation or networking.
	out, err := quarkRunLibmpvCurl(context.Background(), quarkLibmpvCurlInput{Version: 1, URL: "https://cdn.invalid/original?token=synthetic-private", ExpectedSize: quarkLibmpvExpectedSize}, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, python, "-I", "-B", probe, "--synthetic-reject-arguments")
	})
	if err != nil || out.Outcome != "invalid_input" || out.FileLoaded || out.NativeDestroyed {
		t.Fatal("actual Python safe-rejection schema disagrees with parent")
	}
}

func TestQuarkLibmpvCurlAuthorizedSample(t *testing.T) {
	for _, mode := range []string{"valid", "wrong-size", "duplicate", "partial", "other-path", "deep-tree"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			path := quarkLibmpvSamplePath
			if mode == "other-path" {
				path = "/unapproved"
			}
			sample, err := quarkLibmpvFindSample(context.Background(), path, func(_ context.Context, parent string, request cloud.PageRequest) (cloud.Page, error) {
				calls++
				if request.Limit != 100 || request.Offset != 0 {
					t.Fatal("unbounded directory listing")
				}
				switch parent {
				case "0":
					return cloud.Page{Items: []cloud.Item{{ID: "video", Name: "视频", IsDir: true}}}, nil
				case "video":
					return cloud.Page{Items: []cloud.Item{{ID: "approved", Name: "假如 漫威宇宙系列 1080 中英双字", IsDir: true}}}, nil
				}
				if mode == "deep-tree" {
					return cloud.Page{Items: []cloud.Item{{ID: fmt.Sprintf("child-%d", calls), Name: "subdir", IsDir: true}}}, nil
				}
				item := cloud.Item{ID: "sample", Name: "original.mkv", Size: quarkLibmpvExpectedSize}
				if mode == "wrong-size" {
					item.Size--
				}
				page := cloud.Page{Items: []cloud.Item{item}, HasMore: mode == "partial"}
				if mode == "duplicate" {
					page.Items = append(page.Items, cloud.Item{ID: "other", Name: "other.mkv", Size: quarkLibmpvExpectedSize})
				}
				return page, nil
			})
			if mode == "valid" {
				if err != nil || sample.ID != "sample" || calls != 3 {
					t.Fatal("exact approved sample not resolved")
				}
			} else if err == nil || mode == "other-path" && calls != 0 || calls > 5 {
				t.Fatal("unsafe or excessive sample lookup accepted")
			}
		})
	}
}
