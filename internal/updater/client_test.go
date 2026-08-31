package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func response(request *http.Request, status int, payload []byte) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload)), Request: request, Header: make(http.Header)}
}

func TestGitHubClientUsesFixedEndpointAndRejectsTrailingJSON(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != releasesEndpoint {
			t.Fatalf("unexpected release endpoint: %s", request.URL)
		}
		return response(request, http.StatusOK, []byte(`[] {}`)), nil
	})
	client := NewGitHubClient(&http.Client{Transport: transport})
	if _, err := client.ListReleases(context.Background()); ErrorCode(err) != CodeResponseTooLarge {
		t.Fatalf("expected trailing JSON rejection, got %v", err)
	}
}

func TestTrustedRedirectPolicyAllowsBoundedSignedAssetQueryOnly(t *testing.T) {
	client := NewGitHubClient(nil)
	assetURL, _ := url.Parse("https://release-assets.githubusercontent.com/github-production-release-asset/1/file?sp=r&sig=abc")
	if err := client.http.CheckRedirect(&http.Request{URL: assetURL}, []*http.Request{{}}); err != nil {
		t.Fatalf("signed asset redirect was rejected: %v", err)
	}
	githubQuery, _ := url.Parse("https://github.com/yuanjing-hash/OhMyCine-Server/releases/download/server-v1.2.3/file.zip?sig=bad")
	if err := client.http.CheckRedirect(&http.Request{URL: githubQuery}, []*http.Request{{}}); ErrorCode(err) != CodeUntrustedSource {
		t.Fatalf("github query should be rejected, got %v", err)
	}
	evil, _ := url.Parse("https://example.com/file")
	if err := client.http.CheckRedirect(&http.Request{URL: evil}, []*http.Request{{}}); ErrorCode(err) != CodeUntrustedSource {
		t.Fatalf("untrusted redirect should be rejected, got %v", err)
	}
	trusted, _ := url.Parse("https://github.com/file")
	if err := client.http.CheckRedirect(&http.Request{URL: trusted}, make([]*http.Request, maxRedirects)); err == nil {
		t.Fatal("redirect limit was not enforced")
	}
}

func TestDownloadAndVerifyStreamsExactChecksum(t *testing.T) {
	archive := []byte("verified archive payload")
	digest := sha256.Sum256(archive)
	release := fixtureRelease("1.2.3", false, false)
	release.Assets[0].Size = int64(len(archive))
	selected, err := ValidateRelease(release, "windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(fmt.Sprintf("%x  %s\n", digest, selected.Archive.Name))
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, selected.Checksum.Name) {
			return response(request, http.StatusOK, manifest), nil
		}
		if strings.HasSuffix(request.URL.Path, selected.Archive.Name) {
			return response(request, http.StatusOK, archive), nil
		}
		return nil, fmt.Errorf("unexpected request")
	})
	client := NewGitHubClient(&http.Client{Transport: transport})
	destination := filepath.Join(t.TempDir(), "archive.zip")
	if err := client.DownloadAndVerify(context.Background(), selected, destination); err != nil {
		t.Fatal(err)
	}
	if payload, _ := os.ReadFile(destination); !bytes.Equal(payload, archive) {
		t.Fatalf("unexpected archive: %q", payload)
	}
}

func TestDownloadRejectsChecksumMismatchAndUntrustedInitialAsset(t *testing.T) {
	archive := []byte("archive")
	release := fixtureRelease("1.2.3", false, false)
	release.Assets[0].Size = int64(len(archive))
	selected, _ := ValidateRelease(release, "windows", "amd64")
	manifest := []byte(strings.Repeat("0", 64) + "  " + selected.Archive.Name + "\n")
	client := NewGitHubClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, selected.Checksum.Name) {
			return response(request, http.StatusOK, manifest), nil
		}
		return response(request, http.StatusOK, archive), nil
	})})
	destination := filepath.Join(t.TempDir(), "archive.zip")
	if err := client.DownloadAndVerify(context.Background(), selected, destination); ErrorCode(err) != CodeChecksumMismatch {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed download should be removed, stat err=%v", err)
	}
	selected.Archive.DownloadURL = "https://release-assets.githubusercontent.com/arbitrary/file"
	if err := client.DownloadAndVerify(context.Background(), selected, filepath.Join(t.TempDir(), "archive.zip")); ErrorCode(err) != CodeUntrustedSource {
		t.Fatalf("expected initial asset rejection, got %v", err)
	}
}

func TestChecksumParserRequiresUniqueExactEntry(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for name, payload := range map[string][]byte{
		"duplicate": []byte(digest + "  server.zip\n" + digest + "  server.zip\n"),
		"short":     []byte("abcd  server.zip\n"),
		"missing":   []byte(digest + "  another.zip\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseChecksum(payload, "server.zip"); ErrorCode(err) != CodeChecksumInvalid {
				t.Fatalf("expected invalid checksum, got %v", err)
			}
		})
	}
}
