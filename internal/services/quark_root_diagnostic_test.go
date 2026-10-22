package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkRootFailureSafeStageDiagnostics(t *testing.T) {
	private := "https://private.example/path?access_token=do-not-print"
	tests := []struct {
		name, fallback, phase, code, label string
		err                                error
		status, errno                      int
	}{
		{"provider", "verification_input", "management_directory", quark.CodeUnavailable, "管理授权下的目录", &quark.OperationError{Stage: "management_directory", Cause: &quark.Failure{Code: quark.CodeUnavailable, HTTPStatus: 400, Errno: 23018}}, 400, 23018},
		{"transport", "playback_directory", "playback_directory", quark.CodeUnavailable, "播放授权下的目录", cloud.Error(quark.CodeUnavailable, true, errors.New(private)), 0, 0},
		{"receipt", "verification_input", "upload_receipt_load", "quark_local_state_unavailable", "本地验证记录", &quark.OperationError{Stage: "upload_receipt_load", Cause: errors.New(private)}, 0, 0},
		{"credentials", "verification_input", "proof_credentials_save", "quark_local_state_unavailable", "本地验证记录", &quark.OperationError{Stage: "proof_credentials_save", Cause: errors.New(private)}, 0, 0},
		{"unknown_write", "verification_input", "upload_prepare", cloud.CodeMutationUnknown, "不会重复创建验证文件", &quark.OperationError{Stage: "upload_prepare", Cause: &quark.Failure{Code: cloud.CodeMutationUnknown, HTTPStatus: 403, Errno: 23018, UnknownOutcome: true}}, 403, 23018},
		{"incomplete_listing", "management_directory", "management_directory", quark.CodeIncomplete, "目录读取不完整", cloud.Error(quark.CodeIncomplete, false, errors.New(private)), 0, 0},
		{"deadline", "challenge_readback", "challenge_readback", "quark_request_timeout", "等待超时", context.DeadlineExceeded, 0, 0},
		{"cancelled_receipt", "proof_receipt_load", "proof_receipt_load", "quark_request_canceled", "验证已取消", context.Canceled, 0, 0},
		{"untrusted_fields", private, "verification_input", quark.CodeUnavailable, "未获得可用", &quark.OperationError{Stage: private, Cause: &quark.Failure{Code: private, HTTPStatus: 1234567, Errno: 1234567890}}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &ConnectionService{log: zerolog.New(&logs)}
			err := s.quarkRootFailure(42, tt.fallback, tt.err)
			if ErrorCode(err) != CodeConnectionUnavailable || !strings.Contains(ErrorMessage(err), tt.label) {
				t.Fatalf("unexpected public diagnostic: %v", err)
			}
			for _, text := range []string{logs.String(), ErrorMessage(err), err.Error()} {
				for _, secret := range []string{private, "private.example", "access_token", "do-not-print", "1234567", "1234567890"} {
					if strings.Contains(text, secret) {
						t.Fatal("private or unbounded diagnostic escaped")
					}
				}
			}
			var event map[string]any
			if json.Unmarshal(logs.Bytes(), &event) != nil || event["phase"] != tt.phase || event["error_code"] != tt.code || event["connection_id"] != float64(42) || event["reason"] != "connection.quark.root.verify.failed" {
				t.Fatalf("missing structured diagnostic: %s", logs.String())
			}
			for key, want := range map[string]int{"provider_http_status": tt.status, "provider_errno": tt.errno} {
				if want == 0 && event[key] != nil || want != 0 && event[key] != float64(want) {
					t.Fatalf("bad bounded %s", key)
				}
			}
		})
	}
}

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

func TestQuarkRootFailurePreservesIdentityConflict(t *testing.T) {
	s := &ConnectionService{log: zerolog.Nop()}
	err := s.quarkRootFailure(42, "proof_credentials_save", appError(CodeConflict, "private context", errors.New("private cause")))
	if ErrorCode(err) != CodeConflict || strings.Contains(ErrorMessage(err), "private") {
		t.Fatal("credential CAS conflict lost its safe semantics")
	}
}
