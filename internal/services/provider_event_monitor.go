package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

const (
	providerEventConnectionRefresh = 30 * time.Second
	providerEventPollInterval      = 15 * time.Second
	providerEventMaxPagesPerPoll   = 5
)

type providerEventWorker struct {
	cancel context.CancelFunc
}

// ProviderEventMonitor owns one lightweight polling loop per enabled provider
// connection. These loops are independent from the persistent Job scheduler.
type ProviderEventMonitor struct {
	db          *gorm.DB
	connections *ConnectionService
	events      *ProviderEventService
	log         zerolog.Logger

	refreshInterval time.Duration
	pollInterval    time.Duration
	maxPages        int

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	workers map[uint]providerEventWorker
	wg      sync.WaitGroup
	started bool
	closed  bool
}

func NewProviderEventMonitor(db *gorm.DB, connections *ConnectionService, events *ProviderEventService, log zerolog.Logger) *ProviderEventMonitor {
	return &ProviderEventMonitor{db: db, connections: connections, events: events, log: log, refreshInterval: providerEventConnectionRefresh, pollInterval: providerEventPollInterval, maxPages: providerEventMaxPagesPerPoll, workers: map[uint]providerEventWorker{}}
}

func (m *ProviderEventMonitor) Start(ctx context.Context) error {
	if m == nil || m.db == nil || m.connections == nil || m.events == nil {
		return errors.New("provider event monitor is not configured")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("provider event monitor is closed")
	}
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.started = true
	m.mu.Unlock()
	if err := m.reconcile(); err != nil {
		serverlog.OperationProviderLifeEvent.Event(m.log.Error()).Str("error_code", "provider_event_connection_refresh_failed").Msg(serverlog.OperationProviderLifeEvent.Message("监听器启动失败"))
		m.cancel()
		m.mu.Lock()
		m.started = false
		m.mu.Unlock()
		return err
	}
	serverlog.RecordBusiness(ctx, m.log, serverlog.OperationProviderLifeEvent, serverlog.BusinessEvent{Code: "provider.listener.start", Stage: "listen", Outcome: "started", Message: "115 生活事件监听器已启动，将按已配置连接拉取并分发新增事件"})
	m.wg.Add(1)
	go m.runCoordinator()
	return nil
}

func (m *ProviderEventMonitor) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	serverlog.RecordBusiness(context.Background(), m.log, serverlog.OperationProviderLifeEvent, serverlog.BusinessEvent{Code: "provider.listener.stop", Stage: "listen", Outcome: "finished", Message: "115 生活事件监听器已停止，现有事件及任务状态保留"})
}

func (m *ProviderEventMonitor) runCoordinator() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if err := m.reconcile(); err != nil {
				serverlog.OperationProviderLifeEvent.Event(m.log.Error()).Uint("connection_id", 0).Str("error_code", "provider_event_connection_refresh_failed").Int("event_count", 0).Int64("duration_ms", 0).Msg(serverlog.OperationProviderLifeEvent.Message("连接列表刷新失败"))
			}
		}
	}
}

func (m *ProviderEventMonitor) reconcile() error {
	var records []models.Connection
	if err := m.db.WithContext(m.ctx).Select("id").Where("provider = ? AND enabled = ?", cloudpkg.ProviderPan115, true).Find(&records).Error; err != nil {
		return err
	}
	desired := make(map[uint]struct{}, len(records))
	for _, record := range records {
		desired[record.ID] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return nil
	}
	for id, worker := range m.workers {
		if _, ok := desired[id]; !ok {
			worker.cancel()
			delete(m.workers, id)
			serverlog.OperationProviderLifeEvent.Event(m.log.Info()).Uint("connection_id", id).Msg(serverlog.OperationProviderLifeEvent.Message("连接监听已停止"))
		}
	}
	for id := range desired {
		if _, exists := m.workers[id]; exists {
			continue
		}
		workerCtx, cancel := context.WithCancel(m.ctx)
		m.workers[id] = providerEventWorker{cancel: cancel}
		m.wg.Add(1)
		go m.runConnection(workerCtx, id)
		serverlog.OperationProviderLifeEvent.Event(m.log.Info()).Uint("connection_id", id).Msg(serverlog.OperationProviderLifeEvent.Message("连接监听已启动"))
	}
	return nil
}

func (m *ProviderEventMonitor) runConnection(ctx context.Context, connectionID uint) {
	defer m.wg.Done()
	m.poll(ctx, connectionID)
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(ctx, connectionID)
		}
	}
}

func (m *ProviderEventMonitor) poll(ctx context.Context, connectionID uint) {
	started := time.Now()
	inserted, processed := 0, 0
	errorCode := ""
	record, driver, err := m.connections.driver(connectionID)
	stage := "connect"
	if err == nil {
		source, ok := driver.(cloudpkg.ChangeSource)
		if !ok || !driver.Capabilities().ChangeCursor {
			stage = "validate"
			err = errors.New("provider change cursor is unavailable")
			errorCode = "provider_event_unsupported"
		} else {
			stage = "poll"
			for page := 0; page < m.maxPages; page++ {
				var count int
				var more bool
				count, more, err = m.events.IngestOnce(ctx, connectionID, source)
				inserted += count
				if err != nil || !more {
					break
				}
			}
		}
	}
	if err != nil && errorCode == "" {
		errorCode, _ = cloudpkg.ErrorInfo(err)
	}
	if count, processErr := m.events.ProcessPending(ctx, connectionID); processErr != nil {
		if err == nil {
			err = processErr
			errorCode = "provider_event_notification_failed"
			stage = "dispatch"
		}
	} else {
		processed = count
	}
	fields := m.log.With().Uint("connection_id", connectionID).Str("error_code", errorCode).Int("received_events", inserted).Int("processed_events", processed).Int("event_count", inserted+processed).Int64("duration_ms", time.Since(started).Milliseconds()).Logger()
	if err != nil {
		if errorCode == "" {
			errorCode = "provider_event_unavailable"
		}
		serverlog.RecordBusiness(ctx, m.log, serverlog.OperationProviderLifeEvent, serverlog.BusinessEvent{Code: "provider.poll.failed", Stage: stage, Outcome: "failed", Message: "115 连接“" + serverlog.BusinessDisplayName(record.Name) + "”生活事件拉取或分发未完成：" + remainingFailureReason(errorCode) + "；已持久化与已处理数量分别保留，按原周期重试", Fields: map[string]any{"connection_id": connectionID, "connection_name": record.Name, "created_count": inserted, "processed_count": processed, "duration_ms": time.Since(started).Milliseconds(), "error_code": errorCode}})
	} else if inserted+processed > 0 {
		serverlog.RecordBusiness(ctx, m.log, serverlog.OperationProviderLifeEvent, serverlog.BusinessEvent{Code: "provider.poll.finish", Stage: "dispatch", Outcome: "finished", Message: fmt.Sprintf("115 连接“%s”本轮持久化 %d 条新生活事件，已分发并确认处理 %d 条；后续扫描、下载或整理以实际业务任务为准", serverlog.BusinessDisplayName(record.Name), inserted, processed), Fields: map[string]any{"connection_id": connectionID, "connection_name": record.Name, "created_count": inserted, "processed_count": processed, "duration_ms": time.Since(started).Milliseconds()}})
	} else {
		serverlog.OperationProviderLifeEvent.Event(fields.Debug()).Msg(serverlog.OperationProviderLifeEvent.Message("本轮没有新事件"))
	}
}
