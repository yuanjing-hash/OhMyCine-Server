package services

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
)

// Legacy bindings can be recovered locally even when the old credential has
// expired. The UID account portion is numeric; device/session suffixes are not
// account identity. Never use the new, unverified Cookie to establish the old
// connection's namespace.
func pan115CookieAccountID(value string) string {
	cookie, err := pan115.ParseCookie(value)
	if err != nil {
		return ""
	}
	account, _, _ := strings.Cut(cookie.UID, "_")
	return canonicalPan115AccountID(account)
}

func canonicalPan115AccountID(value string) string {
	if value == "" || len(value) > 19 {
		return ""
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return ""
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func (s *ConnectionService) probePan115Rotation(ctx context.Context, previous models.Connection, oldCookie, nextCookie string) (cloud.Account, error) {
	expected := canonicalPan115AccountID(previous.AccountID)
	if previous.AccountID == "" {
		expected = pan115CookieAccountID(oldCookie)
	}
	if expected == "" {
		return cloud.Account{}, appError(CodeConflict, "无法确认原 115 账号，请保留此连接并新建连接验证账号后再迁移媒体库", nil)
	}
	if candidate := pan115CookieAccountID(nextCookie); candidate == "" || candidate != expected {
		return cloud.Account{}, appError(CodeConflict, "新 Cookie 不属于原 115 账号；请使用原账号的 Cookie，更换账号请新建连接", nil)
	}
	driver, err := s.registry.Build(cloud.ProviderPan115, cloud.Config{ConnectionID: previous.ID, Cookie: nextCookie})
	if err != nil {
		return cloud.Account{}, connectionProviderError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	account, err := driver.Probe(ctx)
	if err != nil {
		code, _ := cloud.ErrorInfo(err)
		// Do not retain arbitrary provider causes: adapters may include upstream
		// content. The stable diagnostic is sufficient for this save operation.
		return cloud.Account{}, appError(CodeConnectionUnavailable, connectionTestMessage(cloud.ProviderPan115, code), nil)
	}
	if canonicalPan115AccountID(account.ID) != expected {
		return cloud.Account{}, appError(CodeConflict, "新 Cookie 的 115 账号校验不一致，原凭据未修改；请检查登录账号", nil)
	}
	account.ID = expected
	return account, nil
}

func setConnectionAccount(record *models.Connection, account cloud.Account) {
	record.AccountID, record.AccountName, record.AccountVIP = safeLabel(account.ID, 128), safeLabel(account.Name, 256), account.VIP
	record.QuotaUsedBytes, record.QuotaTotalBytes = account.UsedBytes, account.TotalBytes
}
