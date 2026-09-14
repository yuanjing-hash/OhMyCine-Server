package nodeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestStorageSourceClientLostAcknowledgementKeepsIdempotencyIdentity(t *testing.T) {
	operation := testStorageSourceOperation(t, time.Now().UTC().Add(5*time.Minute))
	var actions []nodeprotocol.StorageSourceActionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/node/v1/operations/"):
			writeStorageSourceOperation(t, w, r, nodeprotocol.OperationPending)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/node/v1/storage-source/actions/"):
			writeNodeError(w, http.StatusNotFound, "node_action_not_found")
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/node/v1/credential-grants/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/node/v1/storage-source/actions":
			var request nodeprotocol.StorageSourceActionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode action: %v", err)
			}
			actions = append(actions, request)
			if len(actions) == 1 {
				// The Node may have committed the provider operation before the
				// response was lost. A retry must use the same idempotency identity.
				writeNodeError(w, http.StatusInternalServerError, nodeprotocol.ErrorNodeOffline)
				return
			}
			_ = json.NewEncoder(w).Encode(testStorageSourceResponse(operation, nodeprotocol.StorageSourceAccepted))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testStorageSourceClient(t, server, operation, func(call int) nodeprotocol.CredentialGrantEnvelope {
		return nodeprotocol.CredentialGrantEnvelope{GrantID: "grant-" + strconv.Itoa(call)}
	})
	source := downloadpkg.Source{Kind: downloadpkg.SourceURL, URL: testStorageSourceMagnet}
	if _, err := client.Submit(context.Background(), downloadpkg.SubmitRequest{Source: source}); err == nil {
		t.Fatal("lost acknowledgement unexpectedly succeeded")
	}
	if _, err := client.Submit(context.Background(), downloadpkg.SubmitRequest{Source: source}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("action calls=%d, want 2 transport deliveries", len(actions))
	}
	firstDigest, err := actions[0].Digest()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := actions[1].Digest()
	if err != nil {
		t.Fatal(err)
	}
	if actions[0].RequestID != operation.Plan.OperationKey || actions[1].RequestID != actions[0].RequestID || firstDigest != secondDigest {
		t.Fatalf("lost-ACK retry drifted: first=%+v second=%+v", actions[0], actions[1])
	}
}

func TestStorageSourceClientReissuesCredentialAfterExpiry(t *testing.T) {
	operation := testStorageSourceOperation(t, time.Now().UTC().Add(5*time.Minute))
	grantCalls := 0
	var actionGrantIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/node/v1/operations/"):
			writeStorageSourceOperation(t, w, r, nodeprotocol.OperationRunning)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/node/v1/storage-source/actions/"):
			response := testStorageSourceResponse(operation, nodeprotocol.StorageSourceWaitingCredentials)
			response.ErrorCode = nodeprotocol.ErrorCredentialExpired
			_ = json.NewEncoder(w).Encode(response)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/node/v1/credential-grants/"):
			grantCalls++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/node/v1/storage-source/actions":
			var request nodeprotocol.StorageSourceActionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode action: %v", err)
			}
			actionGrantIDs = append(actionGrantIDs, request.GrantID)
			response := testStorageSourceResponse(operation, nodeprotocol.StorageSourceWaitingCredentials)
			response.ErrorCode = nodeprotocol.ErrorCredentialExpired
			_ = json.NewEncoder(w).Encode(response)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testStorageSourceClient(t, server, operation, func(call int) nodeprotocol.CredentialGrantEnvelope {
		return nodeprotocol.CredentialGrantEnvelope{GrantID: "rotated-grant-" + strconv.Itoa(call)}
	})
	for range 2 {
		if _, err := client.Get(context.Background(), operation.Plan.OperationKey); err != nil {
			t.Fatalf("credential refresh: %v", err)
		}
	}
	if grantCalls != 2 || len(actionGrantIDs) != 2 || actionGrantIDs[0] == actionGrantIDs[1] {
		t.Fatalf("credential was not reissued: puts=%d action grants=%v", grantCalls, actionGrantIDs)
	}
}

func TestStorageSourceClientRenewsLeaseBeforeAction(t *testing.T) {
	operation := testStorageSourceOperation(t, time.Now().UTC().Add(30*time.Second))
	leaseCalls := 0
	var renewed nodeprotocol.LeaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/lease"):
			leaseCalls++
			if err := json.NewDecoder(r.Body).Decode(&renewed); err != nil {
				t.Fatalf("decode lease: %v", err)
			}
			_ = json.NewEncoder(w).Encode(nodeprotocol.OperationResponse{OperationKey: operation.Plan.OperationKey, PlanDigest: operation.PlanDigest, Status: nodeprotocol.OperationRunning, LeaseEpoch: renewed.LeaseEpoch, LeaseExpiresAt: renewed.LeaseExpiresAt})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/node/v1/operations/"):
			writeStorageSourceOperation(t, w, r, nodeprotocol.OperationRunning)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/node/v1/storage-source/actions/"):
			_ = json.NewEncoder(w).Encode(testStorageSourceResponse(operation, nodeprotocol.StorageSourceAccepted))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testStorageSourceClient(t, server, operation, nil)
	if _, err := client.Get(context.Background(), operation.Plan.OperationKey); err != nil {
		t.Fatal(err)
	}
	if leaseCalls != 1 || renewed.LeaseEpoch != operation.Plan.LeaseEpoch+1 || !renewed.LeaseExpiresAt.After(operation.Plan.LeaseExpiresAt) {
		t.Fatalf("lease was not renewed: calls=%d lease=%+v", leaseCalls, renewed)
	}
}

func TestStorageSourceClientReadsCompletedFileExport(t *testing.T) {
	operation := testStorageSourceOperation(t, time.Now().UTC().Add(5*time.Minute))
	now := time.Now().UTC()
	fileDigest := strings.Repeat("c", 64)
	summary := nodeprotocol.FileExportSummary{OperationKey: operation.Plan.OperationKey, TaskID: operation.Plan.TaskID, Name: "Movie", ManifestDigest: strings.Repeat("d", 64), TotalFiles: 1, TotalBytes: 10, ChunkSize: nodeprotocol.FileChunkSize, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	actionCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/node/v1/operations/"):
			writeStorageSourceOperation(t, w, r, nodeprotocol.OperationCompleted)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/node/v1/storage-source/actions/"):
			response := testStorageSourceResponse(operation, nodeprotocol.StorageSourceCompleted)
			response.TotalFiles, response.DownloadedFiles = 1, 1
			response.TotalBytes, response.DownloadedBytes = 10, 10
			response.FileExport = &summary
			_ = json.NewEncoder(w).Encode(response)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(nodeprotocol.FileExportManifestPage{Summary: summary, Files: []nodeprotocol.FileExportFile{{FileToken: "file:movie", RelativePath: "Movie.mkv", Size: 10, SHA256: fileDigest, ChunkCount: 1}}, Page: 1, PageSize: nodeprotocol.MaxManifestPageSize})
		case r.Method == http.MethodPost && r.URL.Path == "/node/v1/storage-source/actions":
			actionCalls++
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testStorageSourceClient(t, server, operation, nil)
	manifest, err := client.Manifest(context.Background(), operation.Plan.OperationKey)
	if err != nil {
		t.Fatal(err)
	}
	if actionCalls != 0 || !manifest.Complete || len(manifest.Files) != 1 || manifest.Files[0].RelativePath != "Movie.mkv" || manifest.RemoteExportOperationKey != operation.Plan.OperationKey {
		t.Fatalf("manifest=%+v action calls=%d", manifest, actionCalls)
	}
}

func TestStorageSourceClientCancelAlsoCleansManagedSource(t *testing.T) {
	operation := testStorageSourceOperation(t, time.Now().UTC().Add(5*time.Minute))
	cancelCalls, cleanupCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			cancelCalls++
			_ = json.NewEncoder(w).Encode(nodeprotocol.OperationResponse{OperationKey: operation.Plan.OperationKey, PlanDigest: operation.PlanDigest, Status: nodeprotocol.OperationCancelled, LeaseEpoch: operation.Plan.LeaseEpoch, LeaseExpiresAt: operation.Plan.LeaseExpiresAt})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cleanup"):
			cleanupCalls++
			var request nodeprotocol.StorageSourceCleanupRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode cleanup: %v", err)
			}
			_ = json.NewEncoder(w).Encode(nodeprotocol.StorageSourceCleanupResponse{RequestID: request.RequestID, OperationKey: request.OperationKey, TaskID: request.TaskID, Status: "completed", Cleaned: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testStorageSourceClient(t, server, operation, nil)
	if err := client.Cancel(context.Background(), operation.Plan.OperationKey, false); err != nil {
		t.Fatal(err)
	}
	if cancelCalls != 1 || cleanupCalls != 1 {
		t.Fatalf("cancel=%d cleanup=%d", cancelCalls, cleanupCalls)
	}
}

func TestStorageSourceCleanupRetryDoesNotReplayMaterialization(t *testing.T) {
	operation := testStorageSourceOperation(t, time.Now().UTC().Add(5*time.Minute))
	cleanupCalls, materializeCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cleanup"):
			cleanupCalls++
			var request nodeprotocol.StorageSourceCleanupRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode cleanup: %v", err)
			}
			if cleanupCalls == 1 {
				writeNodeError(w, http.StatusServiceUnavailable, nodeprotocol.ErrorNodeOffline)
				return
			}
			_ = json.NewEncoder(w).Encode(nodeprotocol.StorageSourceCleanupResponse{RequestID: request.RequestID, OperationKey: request.OperationKey, TaskID: request.TaskID, Status: "completed", Cleaned: true})
		case r.URL.Path == "/node/v1/storage-source/actions" || strings.HasPrefix(r.URL.Path, "/node/v1/operations/") && r.Method == http.MethodPut:
			materializeCalls++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	operation.Completed = true
	client := testStorageSourceClient(t, server, operation, nil)
	if err := client.CleanupManagedSource(context.Background()); err == nil {
		t.Fatal("first cleanup unexpectedly succeeded")
	}
	if err := client.CleanupManagedSource(context.Background()); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if cleanupCalls != 2 || materializeCalls != 0 {
		t.Fatalf("cleanup calls=%d materialization calls=%d", cleanupCalls, materializeCalls)
	}
}

func TestStorageSourceClientRejectsPlanAndHTTPSourceDrift(t *testing.T) {
	operation := testStorageSourceOperation(t, time.Now().UTC().Add(5*time.Minute))
	grantCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/node/v1/operations/") {
			response := nodeprotocol.OperationResponse{OperationKey: operation.Plan.OperationKey, PlanDigest: strings.Repeat("f", 64), Status: nodeprotocol.OperationPending, LeaseEpoch: operation.Plan.LeaseEpoch, LeaseExpiresAt: operation.Plan.LeaseExpiresAt}
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := testStorageSourceClient(t, server, operation, func(int) nodeprotocol.CredentialGrantEnvelope {
		grantCalls++
		return nodeprotocol.CredentialGrantEnvelope{GrantID: "grant"}
	})
	_, err := client.Submit(context.Background(), downloadpkg.SubmitRequest{Source: downloadpkg.Source{Kind: downloadpkg.SourceURL, URL: testStorageSourceMagnet}})
	if code, _ := downloadpkg.ErrorInfo(err); code != nodeprotocol.ErrorPlanConflict {
		t.Fatalf("plan drift err=%v code=%q", err, code)
	}
	if grantCalls != 0 {
		t.Fatal("plan drift reached credential boundary")
	}

	basePlan := nodeprotocol.StorageSourcePlan{StorageID: "1", ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: nodeprotocol.StorageSourceKindPan115OfflineURL, TargetKind: nodeprotocol.StorageSourceTargetLocal, SourceIdentityDigest: strings.Repeat("a", 64), SourceContentDigest: strings.Repeat("b", 64), OfflineDestinationID: "dir-1"}
	if _, err := NewStorageSourceClient(&Client{}, "task-1", "1", nodeprotocol.StorageSourceKindPan115OfflineURL, "https://example.test/movie", basePlan, nil, nil, nil); err == nil || err.Error() != "node_storage_source_config_invalid" {
		t.Fatalf("ordinary HTTP source err=%v", err)
	}
}

const testStorageSourceMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"

func testStorageSourceOperation(t *testing.T, leaseExpiresAt time.Time) StorageSourceOperation {
	t.Helper()
	payload := nodeprotocol.StorageSourcePlan{StorageID: "1", ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: nodeprotocol.StorageSourceKindPan115OfflineMagnet, TargetKind: nodeprotocol.StorageSourceTargetLocal, SourceIdentityDigest: strings.Repeat("a", 64), SourceContentDigest: strings.Repeat("b", 64), OfflineDestinationID: "dir-1"}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	plan := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: StorageSourceOperationKey("task-1"), TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: leaseExpiresAt, Payload: raw}
	return StorageSourceOperation{Plan: plan, PlanDigest: strings.Repeat("e", 64)}
}

func testStorageSourceClient(t *testing.T, server *httptest.Server, operation StorageSourceOperation, envelope func(int) nodeprotocol.CredentialGrantEnvelope) *StorageSourceClient {
	t.Helper()
	var payload nodeprotocol.StorageSourcePlan
	if err := json.Unmarshal(operation.Plan.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	grantCall := 0
	if envelope == nil {
		envelope = func(int) nodeprotocol.CredentialGrantEnvelope {
			return nodeprotocol.CredentialGrantEnvelope{GrantID: "grant"}
		}
	}
	node := &Client{baseURL: server.URL, identity: Identity{ServerID: "server-1"}, http: server.Client()}
	client, err := NewStorageSourceClient(node, operation.Plan.TaskID, payload.StorageID, payload.SourceKind, testStorageSourceMagnet, payload,
		func(context.Context, nodeprotocol.StorageSourcePlan) (StorageSourceOperation, error) {
			return operation, nil
		},
		func(context.Context, string, []string, string) (nodeprotocol.CredentialGrantEnvelope, error) {
			grantCall++
			return envelope(grantCall), nil
		},
		func(context.Context, nodeprotocol.OperationResponse, *nodeprotocol.StorageSourceActionResponse) error {
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeStorageSourceOperation(t *testing.T, w http.ResponseWriter, r *http.Request, status string) {
	t.Helper()
	var request nodeprotocol.PutOperationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatalf("decode operation: %v", err)
	}
	_ = json.NewEncoder(w).Encode(nodeprotocol.OperationResponse{OperationKey: request.Plan.OperationKey, PlanDigest: request.PlanDigest, Status: status, LeaseEpoch: request.Plan.LeaseEpoch, LeaseExpiresAt: request.Plan.LeaseExpiresAt})
}

func testStorageSourceResponse(operation StorageSourceOperation, status string) nodeprotocol.StorageSourceActionResponse {
	return nodeprotocol.StorageSourceActionResponse{RequestID: operation.Plan.OperationKey, OperationKey: operation.Plan.OperationKey, PlanDigest: operation.PlanDigest, Status: status}
}

func writeNodeError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(nodeprotocol.ErrorResponse{Code: code, Message: code})
}
