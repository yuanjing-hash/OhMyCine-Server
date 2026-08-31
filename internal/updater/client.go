package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

const (
	releasesEndpoint  = "https://api.github.com/repos/yuanjing-hash/OhMyCine-Server/releases?per_page=100"
	maxReleaseJSON    = 2 << 20
	maxChecksumBytes  = 1 << 20
	MaxArchiveBytes   = int64(512 << 20)
	MaxCandidateBytes = int64(256 << 20)
	maxRedirects      = 5
)

var trustedHosts = map[string]struct{}{
	"api.github.com":                        {},
	"github.com":                            {},
	"objects.githubusercontent.com":         {},
	"release-assets.githubusercontent.com":  {},
	"github-releases.githubusercontent.com": {},
}

type GitHubClient struct {
	http *http.Client
}

func NewGitHubClient(client *http.Client) *GitHubClient {
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}}
	} else {
		clone := *client
		client = &clone
	}
	if client.Timeout == 0 || client.Timeout > 15*time.Minute {
		client.Timeout = 15 * time.Minute
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		return validateTrustedURL(request.URL)
	}
	return &GitHubClient{http: client}
}

func validateTrustedURL(candidate *url.URL) error {
	if candidate == nil || candidate.Scheme != "https" || candidate.User != nil || candidate.Fragment != "" {
		return coded(CodeUntrustedSource, errors.New("update URL is not trusted"))
	}
	if candidate.Port() != "" {
		return coded(CodeUntrustedSource, errors.New("update URL uses a non-default port"))
	}
	host := strings.ToLower(candidate.Hostname())
	if _, ok := trustedHosts[host]; !ok {
		return coded(CodeUntrustedSource, errors.New("update host is not trusted"))
	}
	if len(candidate.RawQuery) > 8192 {
		return coded(CodeUntrustedSource, errors.New("update URL query exceeds limit"))
	}
	if candidate.RawQuery != "" && host != "api.github.com" && host != "objects.githubusercontent.com" && host != "release-assets.githubusercontent.com" && host != "github-releases.githubusercontent.com" {
		return coded(CodeUntrustedSource, errors.New("update URL query is not allowed on this host"))
	}
	return nil
}

func validateAssetURL(raw, tag, asset string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || validateTrustedURL(parsed) != nil {
		return nil, coded(CodeUntrustedSource, errors.New("release asset URL is not trusted"))
	}
	if parsed.Hostname() != "github.com" {
		return nil, coded(CodeUntrustedSource, errors.New("initial release asset host is not exact"))
	}
	expected := "/yuanjing-hash/OhMyCine-Server/releases/download/" + tag + "/" + asset
	if parsed.EscapedPath() != expected || parsed.RawQuery != "" {
		return nil, coded(CodeUntrustedSource, errors.New("release asset path is not exact"))
	}
	return parsed, nil
}

func (c *GitHubClient) ListReleases(ctx context.Context) ([]Release, error) {
	requestContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, releasesEndpoint, nil)
	if err != nil {
		return nil, coded(CodeNetwork, err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "OhMyCine-Server-Updater")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, coded(CodeNetwork, err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := validateTrustedURL(response.Request.URL); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, coded(CodeNetwork, fmt.Errorf("release endpoint returned status %d", response.StatusCode))
	}
	if response.ContentLength > maxReleaseJSON {
		return nil, coded(CodeResponseTooLarge, errors.New("release response exceeds limit"))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseJSON+1))
	if err != nil {
		return nil, coded(CodeNetwork, err)
	}
	if len(payload) > maxReleaseJSON {
		return nil, coded(CodeResponseTooLarge, errors.New("release response exceeds limit"))
	}
	var releases []Release
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	if err := decoder.Decode(&releases); err != nil {
		return nil, coded(CodeNetwork, errors.New("release response is invalid"))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF || len(releases) > 100 {
		return nil, coded(CodeResponseTooLarge, errors.New("release response exceeds item limit"))
	}
	return releases, nil
}

func (c *GitHubClient) Latest(ctx context.Context, channel Channel, goos, goarch string) (SelectedRelease, error) {
	releases, err := c.ListReleases(ctx)
	if err != nil {
		return SelectedRelease{}, err
	}
	return SelectLatest(releases, channel, goos, goarch)
}

func (c *GitHubClient) downloadBytes(ctx context.Context, asset Asset, tag string, limit int64) ([]byte, error) {
	assetURL, err := validateAssetURL(asset.DownloadURL, tag, asset.Name)
	if err != nil {
		return nil, err
	}
	if asset.Size <= 0 || asset.Size > limit {
		return nil, coded(CodeResponseTooLarge, errors.New("release asset exceeds limit"))
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "OhMyCine-Server-Updater")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, coded(CodeNetwork, err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := validateTrustedURL(response.Request.URL); err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, coded(CodeNetwork, fmt.Errorf("asset endpoint returned status %d", response.StatusCode))
	}
	if response.ContentLength > limit {
		return nil, coded(CodeResponseTooLarge, errors.New("asset response exceeds limit"))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, coded(CodeNetwork, err)
	}
	if int64(len(payload)) > limit {
		return nil, coded(CodeResponseTooLarge, errors.New("asset response exceeds limit"))
	}
	return payload, nil
}

func parseChecksum(payload []byte, assetName string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	found := 0
	scanner := bufio.NewScanner(strings.NewReader(string(payload)))
	scanner.Buffer(make([]byte, 4096), 4096)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return result, coded(CodeChecksumInvalid, errors.New("checksum manifest has an invalid line"))
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != assetName {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size || len(fields[0]) != sha256.Size*2 {
			return result, coded(CodeChecksumInvalid, errors.New("checksum digest is invalid"))
		}
		found++
		copy(result[:], decoded)
	}
	if err := scanner.Err(); err != nil || found != 1 {
		return result, coded(CodeChecksumInvalid, errors.New("checksum entry is missing or duplicated"))
	}
	return result, nil
}

func (c *GitHubClient) DownloadAndVerify(ctx context.Context, release SelectedRelease, destination string) error {
	checksumPayload, err := c.downloadBytes(ctx, release.Checksum, release.TagName, maxChecksumBytes)
	if err != nil {
		return err
	}
	expected, err := parseChecksum(checksumPayload, release.Archive.Name)
	if err != nil {
		return err
	}
	assetURL, err := validateAssetURL(release.Archive.DownloadURL, release.TagName, release.Archive.Name)
	if err != nil {
		return err
	}
	if release.Archive.Size <= 0 || release.Archive.Size > MaxArchiveBytes {
		return coded(CodeResponseTooLarge, errors.New("archive exceeds size limit"))
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, assetURL.String(), nil)
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "OhMyCine-Server-Updater")
	response, err := c.http.Do(request)
	if err != nil {
		return coded(CodeNetwork, err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := validateTrustedURL(response.Request.URL); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return coded(CodeNetwork, fmt.Errorf("archive endpoint returned status %d", response.StatusCode))
	}
	if response.ContentLength > MaxArchiveBytes {
		return coded(CodeResponseTooLarge, errors.New("archive response exceeds limit"))
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return coded(CodePersistence, err)
	}
	succeeded := false
	defer func() {
		_ = file.Close()
		if !succeeded {
			_ = os.Remove(destination)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, MaxArchiveBytes+1))
	if err != nil {
		return coded(CodeNetwork, err)
	}
	if written > MaxArchiveBytes {
		return coded(CodeResponseTooLarge, errors.New("archive response exceeds limit"))
	}
	if release.Archive.Size > 0 && written != release.Archive.Size {
		return coded(CodeNetwork, errors.New("archive response size does not match release metadata"))
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), hex.EncodeToString(expected[:])) {
		return coded(CodeChecksumMismatch, errors.New("archive checksum mismatch"))
	}
	if err := file.Sync(); err != nil {
		return coded(CodePersistence, err)
	}
	if err := file.Close(); err != nil {
		return coded(CodePersistence, err)
	}
	succeeded = true
	return nil
}

func cleanArchivePath(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, ":") || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return "", coded(CodeArchiveInvalid, errors.New("archive path is unsafe"))
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", coded(CodeArchiveInvalid, errors.New("archive path is unsafe"))
		}
	}
	return name, nil
}
