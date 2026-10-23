package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

// These labels, error codes and bounded numbers are the entire public/logging
// diagnostic vocabulary. Never include the cause's Error, URL, body or payload.
var quarkRootStages = map[string]string{
	"verification_input":         "检查验证参数",
	"playback_authorization":     "校验播放授权",
	"management_authorization":   "校验文件管理授权",
	"playback_directory":         "定位播放授权下的目录",
	"management_directory":       "定位管理授权下的目录",
	"proof_receipt_load":         "读取目录验证记录",
	"proof_challenge_prepare":    "准备目录验证文件",
	"proof_receipt_create":       "保存目录验证记录",
	"proof_receipt_save":         "更新目录验证记录",
	"upload_receipt_load":        "读取验证文件上传记录",
	"upload_receipt_create":      "保存验证文件上传记录",
	"upload_receipt_save":        "更新验证文件上传记录",
	"challenge_upload":           "上传目录验证文件",
	"upload_prepare":             "申请验证文件上传",
	"upload_part":                "写入验证文件内容",
	"upload_part_prepare":        "准备验证文件内容请求",
	"upload_task_query":          "读取原验证文件上传任务",
	"upload_part_recovery":       "核对原验证文件分片",
	"upload_legacy_recovery":     "恢复原验证文件范围",
	"upload_finish":              "确认验证文件上传",
	"upload_finish_response":     "读取验证文件上传完成响应",
	"upload_output_confirmation": "核对验证文件上传结果",
	"challenge_directory":        "从播放授权定位验证文件",
	"challenge_readback":         "从播放授权读取验证文件",
	"proof_credentials_save":     "保存共同访问验证结果",
	"challenge_cleanup":          "清理目录验证文件",
}

func (s *ConnectionService) quarkRootFailure(id uint, fallback string, err error) error {
	stage := fallback
	var operation *quark.OperationError
	if errors.As(err, &operation) {
		stage = operation.Stage
	}
	label, ok := quarkRootStages[stage]
	if !ok {
		stage, label = "verification_input", quarkRootStages["verification_input"]
	}
	status, errno := 0, 0
	var failure *quark.Failure
	if errors.As(err, &failure) {
		if failure.HTTPStatus >= 100 && failure.HTTPStatus <= 599 {
			status = failure.HTTPStatus
		}
		if failure.Errno >= -999999999 && failure.Errno <= 999999999 {
			errno = failure.Errno
		}
	}
	code, message, appCode := quark.CodeUnavailable, "未获得可用的夸克响应，请稍后重试", CodeConnectionUnavailable
	var provider *cloud.ProviderError
	switch {
	case ErrorCode(err) == CodeConflict:
		code, message, appCode = CodeConflict, "连接或授权已更新，请重新读取状态", CodeConflict
	case errors.As(err, &provider):
		switch provider.Code {
		case quark.CodeAuth, quark.CodeLimited, quark.CodeIdentity, quark.CodeProof, quark.CodePending, quark.CodeUnsupported:
			code, message = provider.Code, quarkErrorMessage(provider.Code)
		case quark.CodeInvalid:
			code, message = quark.CodeInvalid, "夸克返回的数据不符合接口预期"
		case quark.CodeIncomplete:
			code, message = quark.CodeIncomplete, "目录读取不完整，尚不能确认验证结果，请稍后重试"
		case cloud.CodeMutationUnknown:
			code, message = cloud.CodeMutationUnknown, "提交结果尚未确认，请稍后重试以核对原操作；不会重复创建验证文件"
		case cloud.CodeConflict:
			code, message, appCode = cloud.CodeConflict, "目录或操作记录发生冲突，请重新读取状态", CodeConflict
		case quark.CodeUnavailable:
			if status != 0 || errno != 0 {
				message = "夸克未接受请求或未返回可用结果"
			}
		}
	case errors.Is(err, context.DeadlineExceeded):
		code, message = "quark_request_timeout", "本次验证等待超时，请稍后重试以核对原操作"
	case errors.Is(err, context.Canceled):
		code, message = "quark_request_canceled", "本次验证已取消，再次验证会继续核对原操作"
	case stage == "proof_challenge_prepare" || stage == "proof_credentials_save" || strings.Contains(stage, "_receipt_"):
		code, message = "quark_local_state_unavailable", "Server 无法读取或保存本地验证记录；已保留原操作，请检查服务器存储后重试"
	}
	message = "夸克目录验证在“" + label + "”阶段未完成：" + message
	if status != 0 {
		message += fmt.Sprintf("（HTTP %d）", status)
	}
	if errno != 0 {
		message += fmt.Sprintf("（提供方错误码 %d）", errno)
	}
	event := serverlog.OperationConnectionManagement.Event(s.log.Warn()).
		Str("reason", "connection.quark.root.verify.failed").
		Uint("connection_id", id).Str("phase", stage).Str("error_code", code)
	if status != 0 {
		event = event.Int("provider_http_status", status)
	}
	if errno != 0 {
		event = event.Int("provider_errno", errno)
	}
	if upload, ok := quark.UploadDiagnosticFromError(err); ok {
		event = event.Str("upload_phase", upload.Phase).Str("upload_receipt_state", upload.State).
			Bool("upload_has_task", upload.HasTask).Bool("upload_has_output", upload.HasOutput).
			Bool("upload_target_recorded", upload.TargetRecorded).Bool("upload_not_dispatched", upload.NotDispatched).
			Int("upload_inflight_part", upload.InFlightPart).Int("upload_confirmed_parts", upload.ConfirmedParts).
			Int("upload_task_pages", upload.TaskPages).Bool("upload_task_listing_complete", upload.TaskListingComplete)
		if upload.SubmitReason != "" {
			event = event.Str("upload_submit_reason", upload.SubmitReason)
		}
		if upload.TaskFinishStatus != nil {
			event = event.Int("upload_task_finish_status", *upload.TaskFinishStatus)
		}
		if upload.TaskQueryReason != "" {
			event = event.Str("upload_task_query_reason", upload.TaskQueryReason)
		}
		if upload.TaskResponseField != "" {
			event = event.Str("upload_task_response_field", upload.TaskResponseField)
		}
		if upload.TaskResponseType != "" {
			event = event.Str("upload_task_response_type", upload.TaskResponseType)
		}
	}
	event.Msg(serverlog.OperationConnectionManagement.Message(message))
	return appError(appCode, message, nil)
}
