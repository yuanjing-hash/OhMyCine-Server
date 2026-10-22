package quark

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type unreadablePreflightStore struct{ *memoryOperations }

func (s unreadablePreflightStore) Load(context.Context, string) (cloud.OperationReceipt, error) {
	return cloud.OperationReceipt{}, errors.New("synthetic storage failure")
}

func TestOperationPreflightConflictHasPositiveNoDispatchEvidence(t *testing.T) {
	for _, absent := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed-absence", false: "unreadable-store"}[absent], func(t *testing.T) {
			writes := 0
			d, store := managedFixture(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/open/v1/file/info":
					if r.URL.Query().Get("fid") == "A" {
						return envelope(r, agentFile("A", "0", "library", true, 0)), nil
					}
					return envelope(r, agentFile("f", "A", "old.mkv", false, 9)), nil
				case "/open/v1/file/list":
					return envelope(r, map[string]any{"last_page": true, "file_list": []any{agentFile("f", "A", "old.mkv", false, 9), agentFile("other", "A", "new.mkv", false, 7)}}), nil
				default:
					writes++
					return nil, errors.New("unexpected mutation")
				}
			})
			if !absent {
				d.config.Operations = unreadablePreflightStore{store}
			}
			_, err := d.BeginOperation(context.Background(), renameContractRequest("preflight-conflict"))
			errorCode(t, err, cloud.CodeConflict)
			var proof *OperationNotSubmittedError
			if errors.As(err, &proof) != absent {
				t.Fatal("no-dispatch evidence confused missing receipt and unreadable store")
			}
			if writes != 0 || len(store.receipts) != 0 {
				t.Fatal("preflight dispatched or created receipt")
			}
		})
	}
}
