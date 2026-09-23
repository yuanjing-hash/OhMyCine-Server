package pan115

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type shareTestSDK struct {
	*bulkSDK
	snapshot       *pan115sdk.ShareSnapResp
	snapshotErr    error
	pages          map[string]*pan115sdk.ShareSnapResp
	usedUA         string
	receivedCode   string
	receivedSecret string
	receivedIDs    []string
	receivedParent string
}

func (s *shareTestSDK) GetShareSnapWithUA(ua string, _ string, _ string, dir string, _ ...pan115sdk.Query) (*pan115sdk.ShareSnapResp, error) {
	s.usedUA = ua
	if s.pages != nil {
		return s.pages[dir], s.snapshotErr
	}
	return s.snapshot, s.snapshotErr
}

func (s *shareTestSDK) ReceiveShare(code, secret string, ids []string, parent string) error {
	s.receivedCode = code
	s.receivedSecret = secret
	s.receivedIDs = append([]string(nil), ids...)
	s.receivedParent = parent
	return nil
}

func shareSnapshot(count int, files ...pan115sdk.ShareFile) *pan115sdk.ShareSnapResp {
	response := &pan115sdk.ShareSnapResp{}
	response.Data.Count = count
	response.Data.List = files
	response.Data.Shareinfo.ShareTitle = "Seven Samurai"
	return response
}

func TestParseShareLinkAcceptsAllowlistedHostsAndQuerySecrets(t *testing.T) {
	for _, raw := range []string{
		"https://115.com/s/share-code?password=abcd",
		"https://share.115.com/s/share_code?pwd=1234",
		"https://115cdn.com/s/share-code?receive_code=xy-z",
	} {
		code, secret, err := parseShareLink(raw)
		if err != nil || code == "" || secret == "" {
			t.Fatalf("parseShareLink(%q) code=%q secret=%q err=%v", raw, code, secret, err)
		}
	}
	for _, raw := range []string{
		"http://115.com/s/share-code?password=abcd",
		"https://115.com.evil.invalid/s/share-code?password=abcd",
		"https://115.com/s/share-code",
		"https://115.com/other/share-code?password=abcd",
	} {
		if _, _, err := parseShareLink(raw); err == nil {
			t.Fatalf("parseShareLink accepted %q", raw)
		}
	}
}

func TestInspectAndReceiveShareKeepProviderSecretsInsideDriver(t *testing.T) {
	sdk := &shareTestSDK{bulkSDK: &bulkSDK{}, snapshot: shareSnapshot(2,
		pan115sdk.ShareFile{FileID: "movie", FileName: "Seven.Samurai.1954.mkv", IsFile: 1},
		pan115sdk.ShareFile{FileID: "subtitle", FileName: "Seven.Samurai.1954.ass", IsFile: 1},
	)}
	client := newOfflineTestClient(sdk)
	snapshot, err := client.InspectShare(context.Background(), "https://115.com/s/share-code?password=abcd")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ShareCode != "share-code" || snapshot.ReceiveCode != "abcd" || snapshot.Title != "Seven Samurai" || len(snapshot.Items) != 2 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if err := client.ReceiveShare(context.Background(), snapshot, "intake-task-root"); err != nil {
		t.Fatal(err)
	}
	if sdk.receivedCode != "share-code" || sdk.receivedSecret != "abcd" || sdk.receivedParent != "intake-task-root" || !reflect.DeepEqual(sdk.receivedIDs, []string{"movie", "subtitle"}) {
		t.Fatalf("receive call code=%q secret=%q parent=%q ids=%v", sdk.receivedCode, sdk.receivedSecret, sdk.receivedParent, sdk.receivedIDs)
	}
}

func TestInspectShareRejectsOversizedProviderCountBeforePartialReceive(t *testing.T) {
	sdk := &shareTestSDK{bulkSDK: &bulkSDK{}, snapshot: shareSnapshot(maxShareTopLevelItems+1,
		pan115sdk.ShareFile{FileID: "first", FileName: "first.mkv", IsFile: 1},
	)}
	_, err := newOfflineTestClient(sdk).InspectShare(context.Background(), "https://115.com/s/share-code?password=abcd")
	if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeShareTooLarge || retryable {
		t.Fatalf("error code=%q retryable=%t err=%v", code, retryable, err)
	}
}

func TestShareSnapshotRejectsDuplicateOrDelimitedProviderIdentities(t *testing.T) {
	for _, files := range [][]pan115sdk.ShareFile{
		{{FileID: "duplicate", FileName: "one.mkv", IsFile: 1}, {FileID: "duplicate", FileName: "two.mkv", IsFile: 1}},
		{{FileID: "first,second", FileName: "movie.mkv", IsFile: 1}},
	} {
		sdk := &shareTestSDK{bulkSDK: &bulkSDK{}, snapshot: shareSnapshot(len(files), files...)}
		_, err := newOfflineTestClient(sdk).InspectShare(context.Background(), "https://115.com/s/share-code?password=abcd")
		if code, retryable := cloud.ErrorInfo(err); code != cloud.CodeResponseInvalid || retryable {
			t.Fatalf("files=%+v code=%q retryable=%t err=%v", files, code, retryable, err)
		}
	}
}

func TestInspectShareMapsInvalidProviderResponseWithoutLeakingInput(t *testing.T) {
	sdk := &shareTestSDK{bulkSDK: &bulkSDK{}, snapshotErr: errors.New("provider rejected share")}
	raw := "https://115.com/s/private-share?password=private-code"
	_, err := newOfflineTestClient(sdk).InspectShare(context.Background(), raw)
	if err == nil || err.Error() == raw {
		t.Fatalf("unexpected error=%v", err)
	}
}

// Based on the real share/snap field shapes: directory cid, file fid + parent cid.
func TestShareReadRealResponseTreeAndFailures(t *testing.T) {
	root, err := decodeShareResponse(200, strings.NewReader(`{"state":true,"errno":0,"data":{"count":1,"list":[{"cid":"123","pid":"0","n":"Movie","s":68571790801,"fc":0,"t":"2025"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	child, err := decodeShareResponse(200, strings.NewReader(`{"state":true,"errno":0,"data":{"count":2,"list":[{"fid":"456","cid":123,"n":"Movie.mkv","s":68496540644,"fc":1},{"fid":"789","cid":"123","n":"Movie.sup","s":"75250157","fc":1}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	sdk := &shareTestSDK{bulkSDK: &bulkSDK{}, pages: map[string]*pan115sdk.ShareSnapResp{"0": root, "123": child}}
	_, tree, err := cloud.InspectShareTree(context.Background(), newOfflineTestClient(sdk), "https://115.com/s/example?password=abcd")
	if err != nil || len(tree) != 3 || sdk.usedUA != downloadBrowserUserAgent {
		t.Fatalf("tree=%v err=%v ua=%q", tree, err, sdk.usedUA)
	}
	var total int64
	for _, item := range tree {
		if !item.IsDir {
			total += item.Size
			if item.ID == "123" {
				t.Fatal("file used parent cid")
			}
		}
	}
	if total != 68571790801 {
		t.Fatal(total)
	}
	for _, tc := range []struct {
		status     int
		body, code string
		provider   int
	}{
		{200, `{"state":false,"errno":4100010,"error":"secret URL password must not escape"}`, cloud.CodeShareExpired, 4100010},
		{200, `{"state":false,"errno":4100008}`, cloud.CodeSharePassword, 4100008},
		{200, `{"state":false,"errno":99}`, cloud.CodeAuthExpired, 99},
		{200, `{"state":false,"errno":123456}`, cloud.CodeUnavailable, 123456},
		{405, `<html>secret</html>`, cloud.CodeRateLimited, 0},
		{200, `<html>secret</html>`, cloud.CodeResponseInvalid, 0},
	} {
		_, err := decodeShareResponse(tc.status, strings.NewReader(tc.body))
		code, _ := cloud.ErrorInfo(err)
		status, provider, _ := ShareReadDiagnostics(err)
		if code != tc.code || status != tc.status || provider != tc.provider || strings.Contains(err.Error(), "secret") {
			t.Fatalf("code=%s status=%d provider=%d", code, status, provider)
		}
	}
	client := newOfflineTestClient(sdk)
	for i := 0; i < 3; i++ {
		client.recordOutcome(cloud.Error(cloud.CodeRateLimited, true, &ShareReadFailure{HTTPStatus: 405}))
	}
	_, err = client.InspectShare(context.Background(), "https://115.com/s/example?password=abcd")
	_, _, cooling := ShareReadDiagnostics(err)
	if !cooling {
		t.Fatalf("shared cooldown not retained: %v", err)
	}
	for _, raw := range []string{"https://115cdn.com/s/example?password=abcd&amp;#", "https://115cdn.com/s/example?password=abcd&amp;amp;#"} {
		normalized, _, err := NormalizeShareLink(raw, "")
		if err != nil || normalized != "https://115.com/s/example?password=abcd" {
			t.Fatalf("normalization=%q err=%v", normalized, err)
		}
	}
	if _, _, err := NormalizeShareLink("https://115.com.evil.test/s/example?password=abcd", ""); err == nil {
		t.Fatal("non-115 host accepted")
	}
}

// Test-only transport keeps the real SDK request builder without remote access.
type shareReadTransport func(*http.Request) (*http.Response, error)

func (f shareReadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestShareReadAdapterUsesSafeRequestAndTypedErrors(t *testing.T) {
	sdk := pan115sdk.New()
	sdk.Client.SetTransport(shareReadTransport(func(r *http.Request) (*http.Response, error) {
		if r.UserAgent() != downloadBrowserUserAgent || r.URL.Query().Get("cid") != "0" || r.URL.Query().Get("share_code") != "example" {
			t.Error("incorrect request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"state":false,"errno":4100010,"error":"private upstream body"}`)), Request: r}, nil
	}))
	_, err := (&sdkAdapter{sdk}).GetShareSnapWithUA(downloadBrowserUserAgent, "example", "abcd", "0")
	if code, _ := cloud.ErrorInfo(err); code != cloud.CodeShareExpired {
		t.Fatal(err)
	}
}
