//go:build quark_live

package handlers_test

import (
	"encoding/json"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// All receipt and immutable-evidence values are synthetic; this test exercises
// the binder, not the separate service's authenticated immutable DB reader.
func TestQuarkObservationCreationIdentityIndependentSameNamespaceUpload(t *testing.T) {
	parent := followIndependentParent(t)
	baseline, _ := json.Marshal(parent.store.state)
	for _, mode := range []string{"valid", "same_namespace_directory", "missing_evidence", "extra_evidence", "missing_output", "unconfirmed_phase", "unknown_receipt", "wrong_ack", "wrong_output_parent", "wrong_output_name", "wrong_output_size", "output_is_directory", "output_tv_namespace", "output_other_account", "evidence_other_account", "evidence_agent_namespace", "evidence_wrong_parent", "evidence_wrong_name", "evidence_wrong_size", "evidence_directory", "changed_target_path", "invalid_mapping_cannot_fallback"} {
		t.Run(mode, func(t *testing.T) {
			var st observationState
			if json.Unmarshal(baseline, &st) != nil {
				t.Fatal("synthetic state clone")
			}
			r := st.Operations[st.UploadID]
			var original struct {
				Parent, Name, Handle, SHA256 string
				Size                         int64
				Target                       *followTarget
				Mapping                      *followMapping
			}
			if json.Unmarshal(r.Payload, &original) != nil || original.Mapping == nil {
				t.Fatal("synthetic upload baseline")
			}
			evidence := original.Mapping.TV
			output := cloud.Item{ID: quark.QualifyID(quark.RouteAgent, st.AgentAccount, original.Handle), ParentID: quark.QualifyID(quark.RouteAgent, st.AgentAccount, original.Parent), Name: st.UploadName, Size: observationFixtureSize}
			payload := map[string]any{"parent": original.Parent, "name": original.Name, "handle": original.Handle, "sha256": original.SHA256, "size": original.Size, "target": original.Target, "phase": "metadata_confirmed"}
			proofs := []cloud.Item{evidence}
			switch mode {
			case "missing_evidence":
				proofs = nil
			case "extra_evidence":
				proofs = append(proofs, evidence)
			case "unconfirmed_phase":
				payload["phase"] = "upload_finished"
			case "unknown_receipt":
				r.State = "unknown"
			case "wrong_ack":
				payload["handle"] = "different-ack"
			case "wrong_output_parent":
				output.ParentID = quark.QualifyID(quark.RouteAgent, st.AgentAccount, "other-parent")
			case "wrong_output_name":
				output.Name = "another.mp4"
			case "wrong_output_size":
				output.Size++
			case "output_is_directory":
				output.IsDir = true
			case "output_tv_namespace":
				output.ID = quark.QualifyID(quark.RouteTV, st.TVAccount, original.Handle)
			case "output_other_account":
				output.ID = quark.QualifyID(quark.RouteAgent, "different-account", original.Handle)
			case "evidence_other_account":
				proofs[0].ID = quark.QualifyID(quark.RouteTV, "different-account", "owned-sample")
			case "evidence_agent_namespace":
				proofs[0].ID = output.ID
			case "evidence_wrong_parent":
				proofs[0].ParentID = parent.identity.Proof.TVRoot
			case "evidence_wrong_name":
				proofs[0].Name = "different.mp4"
			case "evidence_wrong_size":
				proofs[0].Size++
			case "evidence_directory":
				proofs[0].IsDir = true
			case "changed_target_path":
				original.Target.Path += "/different"
			case "invalid_mapping_cannot_fallback":
				original.Mapping.ReceiptRevision++
				payload["mapping"] = original.Mapping
			case "same_namespace_directory":
				dr := st.Operations[st.DirectoryID]
				var directory followOperation
				_ = json.Unmarshal(dr.Payload, &directory)
				directory.Mapping = nil
				directory.Handle = original.Parent
				directory.Request = cloud.OperationRequest{ID: dr.ID, Kind: "mkdir", ParentID: parent.identity.Proof.AgentRoot, Name: st.DirectoryName}
				directory.Outputs = []cloud.Item{{ID: quark.QualifyID(quark.RouteAgent, st.AgentAccount, original.Parent), ParentID: parent.identity.Proof.AgentRoot, Name: st.DirectoryName, IsDir: true}}
				dr.Payload, _ = json.Marshal(directory)
				st.Operations[dr.ID] = dr
			}
			if mode != "missing_output" {
				payload["output"] = output
			}
			r.Payload, _ = json.Marshal(payload)
			st.Operations[r.ID] = r
			got, err := followIdentityFromState(st, []byte("synthetic-journal"), []byte("synthetic-key"), proofs...)
			valid := mode == "valid" || mode == "same_namespace_directory"
			if (err == nil) != valid {
				t.Fatalf("unexpected acceptance for %s", mode)
			}
			if valid && (got.Sample.ID != evidence.ID || got.SampleAgent != original.Handle || got.Sample.ParentID != parent.identity.Root.ID) {
				t.Fatal("binder converted namespaces instead of retaining independent evidence")
			}
		})
	}
	if parent.unchanged() != nil {
		t.Fatal("binding changed original encrypted receipts")
	}
}
