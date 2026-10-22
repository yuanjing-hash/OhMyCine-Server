package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type unreadableCreationReceiptStore struct {
	*memoryOperations
	err     error
	creates int
}

func (s *unreadableCreationReceiptStore) Load(context.Context, string) (cloud.OperationReceipt, error) {
	return cloud.OperationReceipt{}, s.err
}

func (s *unreadableCreationReceiptStore) Create(context.Context, cloud.OperationReceipt) error {
	s.creates++
	return errors.New("must not replace an unreadable receipt")
}

func TestCommonSpaceUnreadableReceiptDoesNotCreateNewChallenge(t *testing.T) {
	for _, loadErr := range []error{errors.New("private receipt could not be decrypted"), context.Canceled} {
		d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/file":
				return envelope(r, map[string]any{"total_count": 1, "files": []any{map[string]any{"fid": "T", "filename": "library", "size": 0, "isdir": 1}}}), nil
			case "/open/v1/file/list":
				return envelope(r, map[string]any{"last_page": true, "file_list": []any{agentFile("A", "0", "library", true, 0)}}), nil
			default:
				t.Fatalf("unreadable proof must not dispatch %s", r.URL.Path)
				return nil, nil
			}
		})
		store := &unreadableCreationReceiptStore{memoryOperations: &memoryOperations{}, err: loadErr}
		d.config.Operations = store
		d.config.PersistCredentials = func(context.Context, uint64, json.RawMessage) error {
			t.Fatal("unreadable receipt must not save credentials")
			return nil
		}
		_, err := d.VerifyCommonSpace(context.Background(), "original-proof", "/library")
		if store.creates != 0 || !errors.Is(err, loadErr) {
			t.Fatalf("load failure was treated as absence: create=%d err=%v", store.creates, err)
		}
		var phase *OperationError
		if !errors.As(err, &phase) || phase.Stage != "proof_receipt_load" {
			t.Fatal("proof checkpoint failure lost its phase")
		}
	}
}

func TestUploadUnreadableReceiptDoesNotPrepareAnotherUpload(t *testing.T) {
	loadErr := errors.New("private receipt could not be read")
	d, memory := managedFixture(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/file/info" {
			return envelope(r, agentFile("A", "0", "library", true, 0)), nil
		}
		t.Fatalf("unreadable upload must not dispatch %s", r.URL.Path)
		return nil, nil
	})
	store := &unreadableCreationReceiptStore{memoryOperations: memory, err: loadErr}
	d.config.Operations = store
	_, err := d.Upload(WithOperationID(context.Background(), "original-upload"), cloud.UploadRequest{ParentID: QualifyID(RouteAgent, "agent-account", "A"), Name: "sample", Size: 3, Reader: bytes.NewReader([]byte("abc"))})
	if store.creates != 0 || !errors.Is(err, loadErr) {
		t.Fatalf("load failure was treated as absence: create=%d err=%v", store.creates, err)
	}
	var phase *OperationError
	if !errors.As(err, &phase) || phase.Stage != "upload_receipt_load" {
		t.Fatal("upload checkpoint failure lost its phase")
	}
}

func TestCommonSpaceUploadRejectionIsDiagnosableWithoutReplay(t *testing.T) {
	pre := 0
	d := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/file":
			return envelope(r, map[string]any{"total_count": 1, "files": []any{map[string]any{"fid": "T", "filename": "library", "size": 0, "isdir": 1}}}), nil
		case "/open/v1/file/list":
			var input struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			files := []any{}
			if input.Parent == "0" {
				files = append(files, agentFile("A", "0", "library", true, 0))
			}
			return envelope(r, map[string]any{"last_page": true, "file_list": files}), nil
		case "/open/v1/file/upload_pre":
			pre++
			return response(r, 400, map[string]any{"status": -1, "errno": 23018, "message": "private URL or token must not escape"}), nil
		default:
			t.Fatalf("unexpected write after rejected preflight: %s", r.URL.Path)
			return nil, nil
		}
	})
	store := &memoryOperations{receipts: map[string]cloud.OperationReceipt{}}
	d.config.Operations = store
	d.config.PersistCredentials = func(context.Context, uint64, json.RawMessage) error {
		t.Fatal("rejected upload cannot establish a proof")
		return nil
	}
	_, err := d.VerifyCommonSpace(context.Background(), "original-proof", "/library")
	errorCode(t, err, cloud.CodeMutationUnknown)
	var phase *OperationError
	var provider *Failure
	if !errors.As(err, &phase) || phase.Stage != "upload_prepare" || !errors.As(err, &provider) || provider.HTTPStatus != 400 || provider.Errno != 23018 || !provider.UnknownOutcome || strings.Contains(err.Error(), "private") {
		t.Fatal("safe upload status or uncertainty was lost")
	}
	original := store.receipts["original-proof"]
	_, err = d.VerifyCommonSpace(context.Background(), "original-proof", "/library")
	errorCode(t, err, cloud.CodeMutationUnknown)
	if pre != 1 || !bytes.Equal(original.Payload, store.receipts["original-proof"].Payload) || len(store.receipts) != 2 {
		t.Fatal("unconfirmed upload created a replacement challenge")
	}
}
