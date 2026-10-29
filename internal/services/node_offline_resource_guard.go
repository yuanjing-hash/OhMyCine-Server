package services

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

func nodeOfflineAccess(taskID, operationKey, uri string) cloud.OfflineAccess {
	return cloud.OfflineAccess{OperationID: "node:" + taskID + ":" + operationKey, URI: uri, Remote: true}
}
func credentialOfflineGuard(db *gorm.DB, secrets *credential.Store, connection models.Connection, cookie string, facts cloud.OfflineFactReader) (cloud.OfflineAccessGuard, error) {
	account, err := pan115.CredentialResourceAccount(cookie)
	if err != nil {
		return nil, appError(CodeConnectionUnavailable, "115 凭据的资源范围不可用", nil)
	}
	if connection.AccountID != "" && strings.TrimSpace(connection.AccountID) != account {
		return nil, appError(CodeConnectionUnavailable, "115 已确认账号与当前凭据不一致", nil)
	}
	store := NewProviderOperationStore(db, secrets, connection.ID, connection.Provider, account).(cloud.ResourceOperationStore)
	return pan115.NewOfflineAccessCoordinator(store, account, facts), nil
}

func (s *DownloaderService) completeNodeOfflineAccess(ctx context.Context, storage models.Storage, taskID, operationKey, uri string) error {
	var operation models.RemoteOperation
	if err := s.db.WithContext(ctx).First(&operation, "operation_key=? AND task_id=? AND status=?", operationKey, taskID, nodeprotocol.OperationCompleted).Error; err != nil {
		return err
	}
	if storage.ConnectionID == nil {
		return appError(CodeConnectionUnavailable, "115 来源范围不可用", nil)
	}
	var current models.Storage
	if err := s.db.WithContext(ctx).First(&current, storage.ID).Error; err != nil || !current.Enabled || current.ConnectionID == nil || *current.ConnectionID != *storage.ConnectionID {
		return appError(CodeConnectionUnavailable, "115 来源范围已变化", err)
	}
	connection, driver, err := s.connections.driver(*storage.ConnectionID)
	if err != nil {
		return err
	}
	facts, ok := driver.(cloud.OfflineFactReader)
	if !ok {
		return appError(CodeConnectionUnavailable, "115 来源缺少原任务只读核对能力", nil)
	}
	cookie, err := s.credentials.Decrypt(connectionPurpose(connection.ID, connection.Provider), connection.CredentialCiphertext)
	if err != nil {
		return err
	}
	guard, err := credentialOfflineGuard(s.db, s.credentials, connection, cookie, facts)
	if err != nil {
		return err
	}
	return guard.CompleteOfflineAccess(ctx, nodeOfflineAccess(taskID, operationKey, uri))
}
