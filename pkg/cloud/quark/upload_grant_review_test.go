package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestUploadGrantFailureBeforePreLeavesNoSubmissionReceipt(t *testing.T) {
	denyAfterListing, denied, uploaded := true, false, false
	pre := 0
	d, store := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/file/info":
			if r.URL.Query().Get("fid") == "A" {
				return envelope(r, agentFile("A", "0", "library", true, 0)), nil
			}
			return envelope(r, agentFile("created", "A", "file", false, 3)), nil
		case "/open/v1/file/list":
			if denyAfterListing {
				denyAfterListing, denied = false, true
			}
			files := []any{}
			if uploaded {
				files = append(files, agentFile("created", "A", "file", false, 3))
			}
			return envelope(r, map[string]any{"last_page": true, "file_list": files}), nil
		case "/open/v1/file/upload_pre":
			pre++
			if denied || r.URL.Query().Get("access_token") != "new-valid-grant" {
				t.Fatal("pre dispatched before a valid fresh grant")
			}
			uploaded = true
			return envelope(r, map[string]any{"fid": "created", "finish": true}), nil
		}
		t.Fatalf("unexpected request: %s", r.URL.Path)
		return nil, nil
	})
	d.credentials.TV.RefreshToken, d.credentials.Agent.RefreshToken = "", ""
	current := d.Credentials()
	grantErr := errors.New("grant unavailable")
	if err := d.SetRestrictedCredentialLoader(func(context.Context) (json.RawMessage, error) {
		if denied {
			return nil, grantErr
		}
		return json.Marshal(current)
	}); err != nil {
		t.Fatal(err)
	}
	ctx := WithOperationID(context.Background(), "grant-before-pre")
	request := func() cloud.UploadRequest {
		return cloud.UploadRequest{ParentID: QualifyID(RouteAgent, "agent-account", "A"), Name: "file", Size: 3, Reader: bytes.NewReader([]byte("abc"))}
	}
	if _, err := d.Upload(ctx, request()); !errors.Is(err, grantErr) {
		t.Fatalf("grant failure lost: %v", err)
	}
	if pre != 0 || len(store.receipts) != 0 {
		t.Fatal("authorization failure created a stranded submission receipt")
	}
	denied = false
	current.Revision++
	current.Agent.AccessToken = "new-valid-grant"
	if item, err := d.Upload(ctx, request()); err != nil || item.Size != 3 || pre != 1 {
		t.Fatalf("same operation could not recover after grant renewal: %v", err)
	}
	if _, err := d.Upload(ctx, request()); err != nil || pre != 1 {
		t.Fatal("confirmed original operation repeated pre")
	}
}
