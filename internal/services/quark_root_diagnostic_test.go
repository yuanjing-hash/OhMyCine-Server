package services

import (
	"bytes"

	"encoding/json"

	"testing"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkRootDiagnosticSurvivesProductionLogManager(t *testing.T) {
	var output bytes.Buffer
	manager, err := serverlog.NewManager(t.TempDir(), "test", &output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	s := &ConnectionService{log: zerolog.New(manager)}
	_ = s.quarkRootFailure(2, "challenge_upload", &quark.OperationError{Stage: "upload_prepare", Cause: &quark.Failure{Code: cloud.CodeMutationUnknown, UnknownOutcome: true, HTTPStatus: 400, Errno: 23018}})
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatal(err)
	}
	if event["reason"] != "connection.quark.root.verify.failed" || event["phase"] != "upload_prepare" || event["provider_http_status"] != float64(400) || event["provider_errno"] != float64(23018) || event["error_code"] != cloud.CodeMutationUnknown || event["event_code"] != nil {
		t.Fatalf("production normalization lost the safe ordinary diagnostic: %s", output.String())
	}
}
