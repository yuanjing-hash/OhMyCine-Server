package services

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
)

// These helpers only observe an existing call result. They must never probe,
// retry, change a durable state or include an upstream error's text.
func remainingBusinessResult(ctx context.Context, logger zerolog.Logger, operation serverlog.Operation, code, stage, successMessage, failureMessage string, started time.Time, fields map[string]any, err error) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["duration_ms"] = time.Since(started).Milliseconds()
	outcome, message := "finished", successMessage
	if err != nil {
		outcome = "failed"
		stableCode, _ := fields["error_code"].(string)
		if stableCode == "" {
			stableCode = strings.ToLower(ErrorCode(err))
		}
		if stableCode == "" {
			stableCode = "operation_failed"
		}
		fields["error_code"] = stableCode
		message = failureMessage + "：" + remainingFailureReason(stableCode)
	}
	serverlog.RecordBusiness(ctx, logger, operation, serverlog.BusinessEvent{Code: code, Stage: stage, Outcome: outcome, Message: message, Fields: fields})
}

func remainingFailureReason(code string) string {
	code = strings.ToLower(code)
	switch code {
	case CodePluginPermissionChanged:
		return "插件权限清单已变化，需要重新确认安装权限"
	case CodePluginSourceChange:
		return "安装来源与已安装版本不一致，不能静默切换仓库"
	case CodePluginPreviewExpired:
		return "安装确认已过期，需要重新准备安装包"
	case CodePluginAlreadyInstalled:
		return "该版本已安装，或普通更新的目标版本不高于当前版本"
	case CodePluginRuntimeStartFailed:
		return "插件运行代次未能启动，旧版本处理结果见补偿记录"
	case CodePluginRuntimeStateFailed:
		return "插件运行状态未能保存，恢复结果见补偿记录"
	}
	switch {
	case strings.Contains(code, "conflict"), strings.Contains(code, "revision"), strings.Contains(code, "changed"):
		return "配置或执行状态已经变化"
	case strings.Contains(code, "permission"), strings.Contains(code, "forbidden"):
		return "当前权限不允许执行"
	case strings.Contains(code, "auth"), strings.Contains(code, "credential"):
		return "登录凭据无效或已过期"
	case strings.Contains(code, "timeout"), strings.Contains(code, "deadline"), strings.Contains(code, "runtime_exceeded"):
		return "操作超过允许的等待时间"
	case strings.Contains(code, "rate_limit"), strings.Contains(code, "cooldown"):
		return "操作过于频繁，请等待后再试"
	case strings.Contains(code, "digest"), strings.Contains(code, "signature"), strings.Contains(code, "manifest"), strings.Contains(code, "package_invalid"):
		return "发布包或清单未通过校验"
	case strings.Contains(code, "not_found"), strings.Contains(code, "missing"):
		return "所需对象已不存在"
	case strings.Contains(code, "unavailable"), strings.Contains(code, "network"):
		return "服务或连接暂不可用"
	default:
		return "操作未能完成，具体分类见错误代码"
	}
}

func pluginBusinessState(status string) string {
	switch status {
	case "enabled", "running":
		return "已启用"
	case "disabled", "stopped":
		return "已停用"
	case "failed", "error":
		return "运行故障"
	case "starting":
		return "正在启动"
	default:
		return "以插件管理页当前记录为准"
	}
}
