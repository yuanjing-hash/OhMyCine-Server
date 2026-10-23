package quark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestCommonSpaceRecoveryInspectionIsLocalAndRequiresPrivatePair(t *testing.T) {
	for _, condition := range []string{"unknown", "absent", "confirmed", "wrong_account", "wrong_root", "wrong_name", "wrong_bytes", "ordinary_target", "missing_proof", "corrupt", "store_failure"} {
		t.Run(condition, func(t *testing.T) {
			d := fixtureClient(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("local recovery inspection dispatched provider")
				return nil, nil
			})
			store := &memoryOperations{receipts: map[string]cloud.OperationReceipt{}}
			d.config.Operations = store
			data := bytes.Repeat([]byte{1}, 32)
			hash := sha256.Sum256(data)
			root := QualifyID(RouteTV, "tv-account", "T")
			p := proofPayload{Path: "/library", TVRoot: root, AgentRoot: QualifyID(RouteAgent, "agent-account", "A"), TVAccount: "tv-account", Name: ".omc-verify-private", Bytes: data}
			u := uploadPayload{Parent: "A", Name: p.Name, Size: 32, SHA256: hex.EncodeToString(hash[:]), Phase: "put_inflight", InFlightPart: 1}
			proof := cloud.OperationReceipt{ID: "proof", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "agent-account", Kind: "space_proof", State: "prepared", Revision: 1}
			upload := cloud.OperationReceipt{ID: "proof/upload", Provider: cloud.ProviderQuark, Route: "agent", AccountID: "agent-account", Kind: "upload", State: "unknown", Revision: 5}
			switch condition {
			case "confirmed":
				upload.State = "confirmed"
				u.Handle = "created"
			case "wrong_account":
				upload.AccountID = "another-account"
			case "wrong_root":
				p.TVRoot = QualifyID(RouteTV, "tv-account", "other")
			case "wrong_name":
				u.Name = "another-name"
			case "wrong_bytes":
				u.SHA256 = "another-hash"
			case "ordinary_target":
				u.Target = &operationTarget{Challenge: false}
			}
			proof.Payload, _ = json.Marshal(p)
			upload.Payload, _ = json.Marshal(u)
			store.receipts[proof.ID] = proof
			store.receipts[upload.ID] = upload
			if condition == "missing_proof" {
				delete(store.receipts, proof.ID)
			}
			if condition == "absent" {
				store.receipts = map[string]cloud.OperationReceipt{}
			}
			if condition == "corrupt" {
				proof.Payload = json.RawMessage(`{`)
				store.receipts[proof.ID] = proof
			}
			if condition == "store_failure" {
				d.config.Operations = &unreadableCreationReceiptStore{memoryOperations: store, err: errors.New("private read failure")}
			}
			before, _ := json.Marshal(store.receipts)
			available, version, err := d.CommonSpaceRecoveryState(context.Background(), "proof", "/library", root)
			if condition == "unknown" {
				if err != nil || !available || len(version) != 64 {
					t.Fatalf("private unknown not eligible: %v", err)
				}
			} else if available || version != "" {
				t.Fatal("unproved/ordinary operation offered independent attempt")
			}
			after, _ := json.Marshal(store.receipts)
			if !bytes.Equal(before, after) {
				t.Fatal("inspection changed original receipt")
			}
		})
	}
}

func TestCommonSpaceGuardBlocksProviderAndObjectWrite(t *testing.T) {
	guardErr := errors.New("replaced identity")
	d := fixtureClient(t, func(*http.Request) (*http.Response, error) { t.Fatal("stale proof called API"); return nil, nil })
	d.mediaTransport = transportFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("stale proof called object transport")
		return nil, nil
	})
	ctx := WithCommonSpaceGuard(context.Background(), func(context.Context) error { return guardErr })
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example/test", nil)
	if _, err := d.api.call(ctx, request); !errors.Is(err, guardErr) {
		t.Fatal("API lost stale guard")
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodPut, "https://object.example/test", bytes.NewReader([]byte("x")))
	if _, err := d.sendUploadPart(ctx, request); !errors.Is(err, guardErr) {
		t.Fatal("object write lost stale guard")
	}
	if err := d.persistProof(ctx, SpaceProof{}); !errors.Is(err, guardErr) {
		t.Fatal("proof persisted after identity replacement")
	}
}

func TestCommonSpaceGuardRechecksAfterAdmissionBeforeTransport(t *testing.T) {
	d := fixtureClient(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("replaced proof reached Do after admission")
		return nil, nil
	})
	valid := true
	guardErr := errors.New("identity changed during admission")
	ctx := WithCommonSpaceGuard(context.Background(), func(context.Context) error {
		if !valid {
			return guardErr
		}
		return nil
	})
	d.api.config.BeforeCall = func(context.Context) error { valid = false; return nil }
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.example/upload", nil)
	if _, err := d.api.call(ctx, request); !errors.Is(err, guardErr) {
		t.Fatal("late replacement did not revoke provider dispatch")
	}
}
