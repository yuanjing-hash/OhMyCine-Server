package pan115

import (
	"context"
	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"io"
	"net/http"
	"strings"
	"testing"
)

type selectedRoundTrip func(*http.Request) (*http.Response, error)

func (f selectedRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestSelectedCookieWireUsesDocumentedEndpointsAndEncryptedSingleSubmit(t *testing.T) {
	calls := 0
	transport := selectedRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "clouddownload.115.com" || r.Header.Get("User-Agent") != sdk.UA115Browser {
			t.Fatal("wrong auth route")
		}
		body := ""
		switch r.URL.Path {
		case "/web/":
			if r.Method != "GET" || r.URL.Query().Get("ac") != "torrent" || r.URL.Query().Get("sha1") != strings.Repeat("b", 40) {
				t.Fatal("wrong metadata request")
			}
			body = `{"state":true,"file_count":2,"info_hash":"` + strings.Repeat("a", 40) + `","torrent_name":"Show","torrent_filelist":[{"path":"Show/b.mkv","size":"20"},{"path":"Show/a.mkv","size":10}]}`
		case "/lixianssp/":
			if r.Method != "POST" || r.URL.Query().Get("ac") != "add_task_bt" {
				t.Fatal("wrong mutation request")
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.PostForm.Get("data") == "" || len(r.PostForm) != 1 || strings.Contains(r.PostForm.Get("data"), "wanted") {
				t.Fatal("wanted request not encrypted")
			}
			body = `{"state":true}`
		default:
			t.Fatal("unplanned endpoint")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	client := sdk.New(sdk.WithClient(&http.Client{Transport: transport}), sdk.UA(sdk.UA115Browser))
	client.UserID = 1
	adapter := &sdkAdapter{client}
	info, err := adapter.ParseOfflineTorrent(context.Background(), strings.Repeat("b", 40))
	if err != nil || len(info.Files) != 2 || info.Files[1].Size != 10 {
		t.Fatal("table", err)
	}
	input := selectedWantedRequest{InfoHash: strings.Repeat("a", 40), DirectoryID: "opaque-id", SavePath: "Show", Wanted: []int{1}}
	if err = adapter.AddOfflineWanted(context.Background(), input); err != nil || calls != 2 {
		t.Fatal("wanted", err, calls)
	}
	client.Client.SetRetryCount(2)
	if err = adapter.AddOfflineWanted(context.Background(), input); err == nil || calls != 2 {
		t.Fatal("retry-capable transport dispatched")
	}
}
func TestSelectedCookieResponseBoundAndHTTPRisk(t *testing.T) {
	for _, status := range []int{200, 302, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			response := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxSelectiveResponseBytes+1)))}
			_, err := readSelectedResponse(response)
			if err == nil {
				t.Fatal("oversized/error response accepted")
			}
			if status == 429 {
				if code, _ := cloud.ErrorInfo(mapError(err)); code != cloud.CodeRateLimited {
					t.Fatal("rate response lost", code)
				}
			}
		})
	}
}
