package nodeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestTaskDownloaderClientMapsCategoryPathsAndBindsRequests(t *testing.T) {
	requests := make([]nodeprotocol.DownloaderActionRequest, 0, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/node/v1/credential-grants/grant-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/node/v1/downloader/actions":
			var input nodeprotocol.DownloaderActionRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			requests = append(requests, input)
			response := nodeprotocol.DownloaderActionResponse{RequestID: input.RequestID}
			switch input.Action {
			case nodeprotocol.DownloaderActionCategories:
				response.Categories = []nodeprotocol.DownloaderCategory{
					{Name: "电视剧", SavePath: "/qb-downloads/task-42/电视剧"},
					{Name: "other", SavePath: "/qb-downloads/another-task/other"},
				}
			case nodeprotocol.DownloaderActionGet:
				response.Task = &nodeprotocol.DownloaderTask{ID: input.ProviderTaskID, Status: "downloading"}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	localRoot := t.TempDir()
	node := &Client{baseURL: server.URL, identity: Identity{ServerID: "server-1"}, http: server.Client()}
	client, err := NewDownloaderClient(node, "downloader-1", "/qb-downloads", localRoot, "task-42", func(context.Context, string, string, string) (nodeprotocol.CredentialGrantEnvelope, error) {
		return nodeprotocol.CredentialGrantEnvelope{GrantID: "grant-1"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	categories, err := client.Categories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(categories) != 2 || categories[0].SavePath != filepath.Join(localRoot, "电视剧") || categories[1].SavePath != "/qb-downloads/another-task/other" {
		t.Fatalf("categories=%+v", categories)
	}
	if _, err := client.Categories(context.Background()); err != nil {
		t.Fatal(err)
	}

	categoryPath := filepath.Join(localRoot, "电视剧")
	if err := client.EnsureCategory(context.Background(), "电视剧", categoryPath); err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureCategory(context.Background(), "电视剧", categoryPath); err != nil {
		t.Fatal(err)
	}
	if err := client.SetCategory(context.Background(), "provider-task-1", "电视剧", categoryPath); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(context.Background(), "provider-task-1"); err != nil {
		t.Fatal(err)
	}

	var ensures []nodeprotocol.DownloaderActionRequest
	var categoryReads []nodeprotocol.DownloaderActionRequest
	for _, request := range requests {
		if request.TaskID != "task-42" {
			t.Fatalf("action %q escaped frozen task binding: %+v", request.Action, request)
		}
		if request.Action == nodeprotocol.DownloaderActionEnsureCategory {
			ensures = append(ensures, request)
		}
		if request.Action == nodeprotocol.DownloaderActionCategories {
			categoryReads = append(categoryReads, request)
		}
	}
	if len(categoryReads) != 2 || categoryReads[0].RequestID == categoryReads[1].RequestID {
		t.Fatalf("category reads were not issued as fresh requests: %+v", categoryReads)
	}
	if len(ensures) != 2 || ensures[0].OperationKey != ensures[1].OperationKey {
		t.Fatalf("ensure category retry was not idempotent: %+v", ensures)
	}
	if ensures[0].SavePath != "/qb-downloads/task-42/电视剧" {
		t.Fatalf("remote category path=%q", ensures[0].SavePath)
	}
}

func TestTaskDownloaderClientRejectsPathAndTaskDrift(t *testing.T) {
	called := false
	node := &Client{baseURL: "http://unused.invalid", identity: Identity{ServerID: "server-1"}, http: http.DefaultClient}
	localRoot := t.TempDir()
	client, err := NewDownloaderClient(node, "downloader-1", "/qb-downloads", localRoot, "task-42", func(context.Context, string, string, string) (nodeprotocol.CredentialGrantEnvelope, error) {
		called = true
		return nodeprotocol.CredentialGrantEnvelope{GrantID: "grant-1"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = client.EnsureCategory(context.Background(), "电视剧", filepath.Join(localRoot, "..", "outside"))
	if code, _ := downloadpkg.ErrorInfo(err); code != nodeprotocol.ErrorPathMappingInvalid {
		t.Fatalf("outside path error=%v code=%q", err, code)
	}
	if called {
		t.Fatal("outside path reached credential or Node boundary")
	}

	_, err = client.Submit(context.Background(), downloadpkg.SubmitRequest{Tag: "omc-another-task"})
	if code, _ := downloadpkg.ErrorInfo(err); code != nodeprotocol.ErrorPlanConflict {
		t.Fatalf("task drift error=%v code=%q", err, code)
	}
	if called {
		t.Fatal("mismatched task tag reached credential or Node boundary")
	}
}
