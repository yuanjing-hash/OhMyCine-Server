package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestUploadHashesUsePrepaddingSHA1State(t *testing.T) {
	for _, size := range []int{32, 64, 130, 1024} {
		data := bytes.Repeat([]byte("z"), size)
		m, s, z, h, e := uploadHashes(context.Background(), bytes.NewReader(data), int64(size))
		md, sh, sha := md5.Sum(data), sha1.Sum(data), sha256.Sum256(data)
		if e != nil || m != hex.EncodeToString(md[:]) || s != hex.EncodeToString(sh[:]) || z != hex.EncodeToString(sha[:]) {
			t.Fatal("hash mismatch")
		}
		if size == 32 && h != [5]uint32{0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476, 0xc3d2e1f0} {
			t.Fatal("final digest used as SHA1 chaining state")
		}
	}
}

func TestMultipartUploadAcknowledgementsAndNoReplay(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 130)
	pre, finish, put := 0, 0, 0
	uploaded := false
	lostPart := true
	finishToken := "agent-access-secret"
	parts := map[int]string{}
	d, store := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/file/info":
			if r.URL.Query().Get("fid") == "A" {
				return envelope(r, agentFile("A", "0", "library", true, 0)), nil
			}
			return envelope(r, agentFile("created", "A", "f.mkv", false, 130)), nil
		case "/open/v1/file/list":
			entries := []any{}
			if uploaded {
				entries = append(entries, agentFile("created", "A", "f.mkv", false, 130))
			}
			return envelope(r, map[string]any{"last_page": true, "file_list": entries}), nil
		case "/open/v1/file/upload_pre":
			pre++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["proof_version"] != "v1" || body["proof_code1"] == nil || body["sha1"] == "" {
				t.Fatal("proof missing")
			}
			return envelope(r, map[string]any{"task_id": "upload-task", "part_size": 64, "upload_urls": []any{}, "common_headers": map[string]string{"Content-Type": "application/octet-stream"}}), nil
		case "/open/v1/file/get_upload_urls":
			var body struct {
				Parts []struct {
					Number  int `json:"part_number"`
					Context *struct {
						Offset int64    `json:"part_offset"`
						H      []uint32 `json:"h"`
					} `json:"parallel_sha1_ctx"`
				} `json:"part_info_list"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			n := body.Parts[0].Number
			if n > 1 && (body.Parts[0].Context == nil || body.Parts[0].Context.Offset != int64(n-1)*64 || len(body.Parts[0].Context.H) != 5) {
				t.Fatal("part chaining state missing")
			}
			return envelope(r, map[string]any{"upload_urls": []any{map[string]any{"part_number": n, "upload_url": "https://objects.example/part/" + strconv.Itoa(n), "signature_info": map[string]string{"signature": "object-signature"}}}}), nil
		case "/open/v1/file/list_uploaded_parts":
			return envelope(r, map[string]any{"uploaded_parts": []uploadPart{{Number: 1, ETag: parts[1]}}}), nil
		case "/open/v1/file/upload_finish":
			finish++
			if r.URL.Query().Get("access_token") != finishToken || r.Header.Get("Authorization") != "Bearer "+finishToken {
				t.Fatal("finish used grant captured before PUTs")
			}
			uploaded = true
			return envelope(r, map[string]any{"fid": "created", "finish": true}), nil
		}
		t.Fatalf("unexpected %s", r.URL.Path)
		return nil, nil
	})
	d.credentials.TV.RefreshToken = ""
	d.credentials.Agent.RefreshToken = ""
	current := d.Credentials()
	if err := d.SetRestrictedCredentialLoader(func(context.Context) (json.RawMessage, error) { return json.Marshal(current) }); err != nil {
		t.Fatal(err)
	}
	d.mediaTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		put++
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/part/"))
		content, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || r.Header.Get("Authorization") != "object-signature" || r.Header.Get("X-Pan-Token") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("wrong upload credentials")
		}
		want := 64
		if n == 3 {
			want = 2
		}
		if len(content) != want {
			t.Fatal("wrong part size")
		}
		parts[n] = "etag-" + strconv.Itoa(n)
		if n == 3 {
			finishToken = "renewed-grant-after-last-put"
			current.Agent.AccessToken = finishToken
			current.Revision++
		}
		if lostPart {
			lostPart = false
			return nil, errors.New("lost ACK")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": {parts[n]}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	request := cloud.UploadRequest{ParentID: QualifyID(RouteAgent, "agent-account", "A"), Name: "f.mkv", Size: 130, Reader: bytes.NewReader(data)}
	ctx := WithOperationID(context.Background(), "job/upload")
	_, e := d.Upload(ctx, request)
	errorCode(t, e, cloud.CodeMutationUnknown)
	if store.receipts["job/upload"].State != "unknown" {
		t.Fatal("lost part state missing")
	}
	request.Reader = bytes.NewReader(data)
	item, e := d.Upload(ctx, request)
	if e != nil || item.Size != 130 || pre != 1 || finish != 1 || put != 3 {
		t.Fatalf("recovery failed %v pre=%d finish=%d put=%d", e, pre, finish, put)
	}
	request.Reader = bytes.NewReader(data)
	_, e = d.Upload(ctx, request)
	if e != nil || pre != 1 || finish != 1 || put != 3 {
		t.Fatal("confirmed upload replayed")
	}
	request.Reader = bytes.NewReader(bytes.Repeat([]byte("y"), 130))
	_, e = d.Upload(ctx, request)
	errorCode(t, e, CodeIdentity)
}

func TestCommonSpaceProofIndependentReadAndRetry(t *testing.T) {
	var content []byte
	name := ""
	uploaded := false
	puts := 0
	pre := 0
	gets := 0
	rootHandle := "initial|root"
	d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/file":
			q := r.URL.Query()
			if q.Get("method") == "download" {
				if q.Get("fid") != "tv-created" {
					t.Fatal("Agent ID used for TV original")
				}
				return envelope(r, map[string]any{"fid": "tv-created", "file_name": name, "size": 32, "download_url": "https://objects.example/read"}), nil
			}
			if q.Get("parent_fid") == "0" {
				return envelope(r, map[string]any{"total_count": 1, "files": []any{map[string]any{"fid": "T", "filename": "library", "size": 0, "isdir": 1}}}), nil
			}
			files := []any{}
			if uploaded {
				files = append(files, map[string]any{"fid": "tv-created", "filename": name, "size": 32, "isdir": 0})
			}
			return envelope(r, map[string]any{"total_count": len(files), "files": files}), nil
		case "/open/v1/file/list":
			var body struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			files := []any{}
			if body.Parent == "0" {
				files = append(files, agentFile(rootHandle, "0", "library", true, 0))
			} else if sameAgent(body.Parent, "initial|root") && uploaded {
				// Actual Agent upload ACKs and listing handles can occupy distinct
				// identity domains. Bootstrap cannot assume their raw IDs match.
				files = append(files, agentFile("listed|created", body.Parent, name, false, 32))
			}
			return envelope(r, map[string]any{"last_page": true, "file_list": files}), nil
		case "/open/v1/file/upload_pre":
			pre++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			name = body["file_name"].(string)
			return envelope(r, map[string]any{"task_id": "proof-upload", "part_size": 64, "common_headers": map[string]string{}, "upload_urls": []any{map[string]any{"part_number": 1, "upload_url": "https://objects.example/put", "signature_info": map[string]string{"signature": "object-token"}}}}), nil
		case "/open/v1/file/upload_finish":
			uploaded = true
			return envelope(r, map[string]any{"fid": "agent-created", "finish": true}), nil
		case "/open/v1/file/info":
			return envelope(r, agentFile("agent-created", "initial|root", name, false, 32)), nil
		}
		t.Fatalf("unexpected %s", r.URL.Path)
		return nil, nil
	})
	store := &memoryOperations{receipts: map[string]cloud.OperationReceipt{}}
	d.config.Operations = store
	current := d.Credentials()
	d.config.LoadCredentials = func(context.Context) (json.RawMessage, error) { return json.Marshal(current) }
	persistErr := errors.New("synthetic private credential storage failure")
	failProofSave := true
	d.config.PersistCredentials = func(_ context.Context, expected uint64, raw json.RawMessage) error {
		if current.Revision != expected {
			t.Fatal("proof CAS")
		}
		if failProofSave {
			return persistErr
		}
		return json.Unmarshal(raw, &current)
	}
	d.mediaTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PUT" {
			puts++
			content, _ = io.ReadAll(r.Body)
			return &http.Response{StatusCode: 200, Header: http.Header{"Etag": {"one"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		gets++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("proof read not anonymous")
		}
		return &http.Response{StatusCode: 200, ContentLength: 32, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(content))}, nil
	})
	proof, e := d.VerifyCommonSpace(context.Background(), "proof-attempt", "/library")
	if !errors.Is(e, persistErr) || current.HasProof(proof.TVRoot, proof.AgentRoot) || store.receipts["proof-attempt"].State != "confirmed" {
		t.Fatal("credential save failure did not retain the confirmed proof")
	}
	var phase *OperationError
	if !errors.As(e, &phase) || phase.Stage != "proof_credentials_save" {
		t.Fatal("credential persistence failure lost its phase")
	}
	failProofSave = false
	proof, e = d.VerifyCommonSpace(context.Background(), "proof-attempt", "/library")
	if e != nil || !current.HasProof(proof.TVRoot, proof.AgentRoot) || len(content) != 32 || pre != 1 || puts != 1 || gets != 2 {
		t.Fatalf("proof failed %v pre=%d puts=%d gets=%d", e, pre, puts, gets)
	}
	rootHandle = "renewed-prefix|root"
	again, e := d.VerifyCommonSpace(context.Background(), "proof-attempt", "/library")
	if e != nil || again.ReceiptID != proof.ReceiptID || pre != 1 || puts != 1 || gets != 2 {
		t.Fatal("proof retry recreated file")
	}
	if proof.TVAccount == proof.AgentAccount {
		t.Fatal("proof falsely conflated application IDs")
	}
}

func TestUnknownUploadFinishNotSubmittedTwice(t *testing.T) {
	pre, finish := 0, 0
	d, _ := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/file/info":
			return envelope(r, agentFile("A", "0", "library", true, 0)), nil
		case "/open/v1/file/list":
			return envelope(r, map[string]any{"last_page": true}), nil
		case "/open/v1/file/upload_pre":
			pre++
			return envelope(r, map[string]any{"task_id": "t", "part_size": 64, "upload_urls": []any{map[string]any{"part_number": 1, "upload_url": "https://objects.example/put", "signature_info": map[string]string{"signature": "object-token"}}}, "common_headers": map[string]string{}}), nil
		case "/open/v1/file/upload_finish":
			finish++
			return nil, errors.New("lost finish")
		}
		t.Fatal("unexpected request")
		return nil, nil
	})
	d.mediaTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, r.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": {"etag"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx := WithOperationID(context.Background(), "upload")
	for n := 0; n < 2; n++ {
		_, e := d.Upload(ctx, cloud.UploadRequest{ParentID: QualifyID(RouteAgent, "agent-account", "A"), Name: "file", Size: 3, Reader: bytes.NewReader([]byte("abc"))})
		errorCode(t, e, cloud.CodeMutationUnknown)
	}
	if pre != 1 || finish != 1 {
		t.Fatal("unknown finish replayed")
	}
}

func TestPreparedUploadRestartCannotPanicOrWrite(t *testing.T) {
	d, store := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/file/info" {
			return envelope(r, agentFile("A", "0", "library", true, 0)), nil
		}
		t.Fatal("prepared upload restart sent a mutation")
		return nil, nil
	})
	md, sh, sha, _, _ := uploadHashes(context.Background(), bytes.NewReader([]byte("abc")), 3)
	p := uploadPayload{Parent: "A", Name: "file", Size: 3, MD5: md, SHA1: sh, SHA256: sha, Phase: "prepare"}
	raw, _ := json.Marshal(p)
	store.receipts["interrupted"] = cloud.OperationReceipt{ID: "interrupted", Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: "agent-account", Kind: "upload", Revision: 1, State: "prepared", Payload: raw}
	_, e := d.Upload(WithOperationID(context.Background(), "interrupted"), cloud.UploadRequest{ParentID: QualifyID(RouteAgent, "agent-account", "A"), Name: "file", Size: 3, Reader: bytes.NewReader([]byte("abc"))})
	errorCode(t, e, cloud.CodeMutationUnknown)
	_, e = d.QueryOperation(context.Background(), "interrupted")
	errorCode(t, e, cloud.CodeMutationUnknown)
}
