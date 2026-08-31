package services

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/buildinfo"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
)

const (
	updateManagedDeployment = "deployment_managed"
	updateManagedContainer  = "container"
	updateManagedPlatform   = "unsupported_platform"
	updateManagedDevBuild   = "development_build"
	updateManagedExecutable = "unreplaceable_executable"
)

type UpdateStatus struct {
	CurrentVersion    string        `json:"current_version"`
	CurrentCommit     string        `json:"current_commit"`
	OfficialBuild     bool          `json:"official_build"`
	Comparable        bool          `json:"comparable"`
	Channel           string        `json:"channel"`
	Revision          uint64        `json:"revision"`
	Phase             updater.Phase `json:"phase"`
	LatestVersion     string        `json:"latest_version,omitempty"`
	TargetVersion     string        `json:"target_version,omitempty"`
	UpdateAvailable   bool          `json:"update_available"`
	InstallEnabled    bool          `json:"install_enabled"`
	DeploymentManaged bool          `json:"deployment_managed"`
	ManagedReason     string        `json:"managed_reason,omitempty"`
	ErrorCode         string        `json:"error_code,omitempty"`
	LastCheckedAt     *time.Time    `json:"last_checked_at,omitempty"`
}

type UpdateSettingsInput struct {
	Channel  string
	Revision uint64
}

type UpdateInstallInput struct {
	TargetVersion string
}

type updateReleaseClient interface {
	Latest(context.Context, updater.Channel, string, string) (updater.SelectedRelease, error)
	Prepare(context.Context, updater.SelectedRelease, *updater.Store, updater.PrepareRequest) (updater.PreparedUpdate, error)
}

type updateServiceOptions struct {
	store             *updater.Store
	client            updateReleaseClient
	info              buildinfo.Info
	executable        string
	healthURL         string
	parentPID         int
	originalArgs      []string
	goos              string
	goarch            string
	updateMode        string
	container         func() bool
	launchHelper      func(string, string) error
	requestShutdown   func()
	now               func() time.Time
	backgroundTimeout time.Duration
}

type UpdateService struct {
	store           *updater.Store
	client          updateReleaseClient
	audit           *AuditService
	log             zerolog.Logger
	info            buildinfo.Info
	executable      string
	healthURL       string
	parentPID       int
	originalArgs    []string
	goos            string
	goarch          string
	updateMode      string
	container       func() bool
	launchHelper    func(string, string) error
	requestShutdown func()
	now             func() time.Time
	timeout         time.Duration
	mu              sync.Mutex
	reconcileMu     sync.Mutex
	busy            bool
	busyKind        string
}

func NewUpdateService(runtimeDirectory, healthURL string, audit *AuditService, log zerolog.Logger, requestShutdown func()) (*UpdateService, error) {
	if requestShutdown == nil {
		return nil, appError(CodeInvalidRequest, "更新停机协调器未配置", nil)
	}
	store, err := updater.NewStore(runtimeDirectory)
	if err != nil {
		return nil, updateAppError(err, "无法初始化更新目录")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, updateAppError(err, "无法确认 Server 可执行文件")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, updateAppError(err, "无法确认 Server 可执行文件")
	}
	service := newUpdateService(audit, log, updateServiceOptions{
		store: store, client: updater.NewGitHubClient(nil), info: buildinfo.Current(), executable: executable,
		healthURL: healthURL, parentPID: os.Getpid(), originalArgs: append([]string(nil), os.Args[1:]...),
		goos: runtime.GOOS, goarch: runtime.GOARCH, updateMode: os.Getenv("OMC_UPDATE_MODE"),
		container: defaultContainerDetector, launchHelper: defaultUpdateHelperLauncher,
		requestShutdown: requestShutdown, now: func() time.Time { return time.Now().UTC() }, backgroundTimeout: 30 * time.Minute,
	})
	service.reconcileTerminalState()
	service.watchTerminalState()
	return service, nil
}

func newUpdateService(audit *AuditService, log zerolog.Logger, options updateServiceOptions) *UpdateService {
	return &UpdateService{
		store: options.store, client: options.client, audit: audit, log: log, info: options.info,
		executable: options.executable, healthURL: options.healthURL, parentPID: options.parentPID,
		originalArgs: append([]string(nil), options.originalArgs...), goos: options.goos, goarch: options.goarch,
		updateMode: options.updateMode, container: options.container, launchHelper: options.launchHelper,
		requestShutdown: options.requestShutdown, now: options.now, timeout: options.backgroundTimeout,
	}
}

func (s *UpdateService) Status(actor Actor) (UpdateStatus, error) {
	if err := s.authorize(actor); err != nil {
		return UpdateStatus{}, err
	}
	s.reconcileTerminalState()
	return s.status()
}

func (s *UpdateService) Check(ctx context.Context, actor Actor, request RequestContext) (UpdateStatus, error) {
	if err := s.authorize(actor); err != nil {
		s.auditDenied(actor, "server.update.check", request)
		return UpdateStatus{}, err
	}
	if err := s.acquire("check"); err != nil {
		return UpdateStatus{}, err
	}
	defer s.release()
	settings, err := s.store.LoadSettings()
	if err != nil {
		return UpdateStatus{}, updateAppError(err, "无法读取更新设置")
	}
	now := s.now()
	state := updater.State{Phase: updater.PhaseChecking, CurrentVersion: s.info.Version, Channel: settings.Channel, StartedAt: &now}
	if err := s.store.SaveState(state); err != nil {
		return UpdateStatus{}, updateAppError(err, "无法保存更新状态")
	}
	serverlog.OperationServerUpdate.Event(s.log.Info()).Str("channel", string(settings.Channel)).Msg(serverlog.OperationServerUpdate.Message("开始检查官方版本"))
	release, latestErr := s.client.Latest(ctx, settings.Channel, s.goos, s.goarch)
	completed := s.now()
	state.CompletedAt = &completed
	state.LastCheckedAt = &completed
	if latestErr != nil {
		code := updater.ErrorCode(latestErr)
		state.Phase = updater.PhaseFailed
		state.ErrorCode = code
		if code == updater.CodeNoRelease {
			state.Phase = updater.PhaseIdle
		}
		_ = s.store.SaveState(state)
		s.record(actor, "server.update.check", "failed", map[string]any{"channel": string(settings.Channel), "error_code": code}, request)
		serverlog.OperationServerUpdate.Event(s.log.Warn()).Str("channel", string(settings.Channel)).Str("error_code", code).Msg(serverlog.OperationServerUpdate.Message("检查官方版本失败"))
		if code == updater.CodeNoRelease {
			return s.status()
		}
		return UpdateStatus{}, updateAppError(latestErr, "检查官方版本失败")
	}
	state.TargetVersion = release.Version.String()
	state.Phase = updater.PhaseIdle
	if current, parseErr := buildinfo.ParseVersion(s.info.Version); parseErr == nil && release.Version.Compare(current) > 0 {
		state.Phase = updater.PhaseAvailable
	}
	if err := s.store.SaveState(state); err != nil {
		return UpdateStatus{}, updateAppError(err, "无法保存更新状态")
	}
	s.record(actor, "server.update.check", "success", map[string]any{"channel": string(settings.Channel), "update_available": state.Phase == updater.PhaseAvailable, "target_version": state.TargetVersion}, request)
	serverlog.OperationServerUpdate.Event(s.log.Info()).Str("channel", string(settings.Channel)).Str("target_version", state.TargetVersion).Bool("update_available", state.Phase == updater.PhaseAvailable).Msg(serverlog.OperationServerUpdate.Message("官方版本检查完成"))
	return s.status()
}

func (s *UpdateService) UpdateSettings(actor Actor, input UpdateSettingsInput, request RequestContext) (UpdateStatus, error) {
	if err := s.authorize(actor); err != nil {
		s.auditDenied(actor, "server.update.settings", request)
		return UpdateStatus{}, err
	}
	if err := s.acquire("settings"); err != nil {
		return UpdateStatus{}, err
	}
	defer s.release()
	channel := updater.Channel(strings.ToLower(strings.TrimSpace(input.Channel)))
	if !channel.Valid() {
		return UpdateStatus{}, &AppError{Code: updater.CodeInvalidChannel, Message: "更新通道无效"}
	}
	settings, err := s.store.UpdateSettings(channel, input.Revision)
	if err != nil {
		if updater.ErrorCode(err) == updater.CodeRevisionConflict {
			return UpdateStatus{}, appError(CodeConflict, "更新设置已变化，请刷新后重试", err)
		}
		return UpdateStatus{}, updateAppError(err, "无法保存更新设置")
	}
	previous, _ := s.store.LoadState()
	state := updater.State{Phase: updater.PhaseIdle, CurrentVersion: s.info.Version, Channel: settings.Channel, LastCheckedAt: previous.LastCheckedAt}
	if err := s.store.SaveState(state); err != nil {
		return UpdateStatus{}, updateAppError(err, "无法保存更新状态")
	}
	s.record(actor, "server.update.settings", "success", map[string]any{"channel": string(channel), "revision": settings.Revision}, request)
	serverlog.OperationServerUpdate.Event(s.log.Info()).Str("channel", string(channel)).Msg(serverlog.OperationServerUpdate.Message("更新通道已修改"))
	return s.status()
}

func (s *UpdateService) Install(actor Actor, input UpdateInstallInput, request RequestContext) (UpdateStatus, error) {
	if err := s.authorize(actor); err != nil {
		s.auditDenied(actor, "server.update.install", request)
		return UpdateStatus{}, err
	}
	target := strings.TrimSpace(input.TargetVersion)
	if _, err := buildinfo.ParseVersion(target); err != nil {
		return UpdateStatus{}, &AppError{Code: updater.CodeInvalidRelease, Message: "目标版本无效"}
	}
	if !s.info.Official || !s.info.Comparable {
		return UpdateStatus{}, appError(CodeConflict, "当前构建不能执行自更新", nil)
	}
	managed, reason := s.deploymentManaged()
	if managed {
		return UpdateStatus{}, appError(CodeConflict, managedMessage(reason), nil)
	}
	if err := s.acquire("install"); err != nil {
		return UpdateStatus{}, err
	}
	settings, err := s.store.LoadSettings()
	if err != nil {
		s.release()
		return UpdateStatus{}, updateAppError(err, "无法读取更新设置")
	}
	state, err := s.store.LoadState()
	if err != nil {
		s.release()
		return UpdateStatus{}, updateAppError(err, "无法读取更新状态")
	}
	current, _ := buildinfo.ParseVersion(s.info.Version)
	targetVersion, _ := buildinfo.ParseVersion(target)
	if state.Phase != updater.PhaseAvailable || state.TargetVersion != target || targetVersion.Compare(current) <= 0 || state.Channel != settings.Channel {
		s.release()
		return UpdateStatus{}, appError(CodeConflict, "可安装版本已变化，请重新检查", nil)
	}
	now := s.now()
	state = updater.State{Phase: updater.PhaseDownloading, CurrentVersion: s.info.Version, TargetVersion: target, Channel: settings.Channel, StartedAt: &now, LastCheckedAt: state.LastCheckedAt}
	if err := s.store.SaveState(state); err != nil {
		s.release()
		return UpdateStatus{}, updateAppError(err, "无法保存更新状态")
	}
	s.record(actor, "server.update.install", "requested", map[string]any{"channel": string(settings.Channel), "target_version": target}, request)
	serverlog.OperationServerUpdate.Event(s.log.Info()).Str("channel", string(settings.Channel)).Str("target_version", target).Msg(serverlog.OperationServerUpdate.Message("更新安装已请求"))
	go s.prepareAndHandoff(actor.User.ID, request, settings.Channel, target, now)
	return s.status()
}

func (s *UpdateService) prepareAndHandoff(actorID uint, request RequestContext, channel updater.Channel, target string, started time.Time) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	release, err := s.client.Latest(ctx, channel, s.goos, s.goarch)
	if err != nil || release.Version.String() != target {
		if err == nil {
			err = &updater.Error{Code: updater.CodeInvalidRelease, Err: errors.New("selected release changed")}
		}
		s.failInstall(actorID, request, channel, target, started, err)
		return
	}
	prepared, err := s.client.Prepare(ctx, release, s.store, updater.PrepareRequest{CurrentExecutable: s.executable, ParentPID: s.parentPID, OriginalArgs: s.originalArgs, HealthURL: s.healthURL})
	if err != nil {
		s.failInstall(actorID, request, channel, target, started, err)
		return
	}
	previous, _ := s.store.LoadState()
	ready := updater.State{Phase: updater.PhaseReady, CurrentVersion: s.info.Version, TargetVersion: target, Channel: channel, OperationID: prepared.OperationID, StartedAt: &started, LastCheckedAt: previous.LastCheckedAt}
	if err := s.store.SaveState(ready); err != nil {
		s.failInstall(actorID, request, channel, target, started, err)
		return
	}
	if err := s.launchHelper(prepared.HelperExecutable, prepared.PlanPath); err != nil {
		s.failInstall(actorID, request, channel, target, started, &updater.Error{Code: updater.CodeRestartFailed, Err: err})
		return
	}
	waiting := ready
	waiting.Phase = updater.PhaseWaitingForExit
	_ = s.store.SaveState(waiting)
	serverlog.OperationServerUpdate.Event(s.log.Info()).Str("target_version", target).Msg(serverlog.OperationServerUpdate.Message("更新包已校验，准备重启"))
	if s.requestShutdown != nil {
		s.requestShutdown()
	}
}

func (s *UpdateService) failInstall(actorID uint, request RequestContext, channel updater.Channel, target string, started time.Time, err error) {
	completed := s.now()
	code := updater.ErrorCode(err)
	previous, _ := s.store.LoadState()
	_ = s.store.SaveState(updater.State{Phase: updater.PhaseFailed, CurrentVersion: s.info.Version, TargetVersion: target, Channel: channel, ErrorCode: code, StartedAt: &started, CompletedAt: &completed, LastCheckedAt: previous.LastCheckedAt})
	s.recordID(&actorID, "server.update.install", target, "failed", map[string]any{"channel": string(channel), "target_version": target, "error_code": code}, request)
	serverlog.OperationServerUpdate.Event(s.log.Error()).Str("target_version", target).Str("error_code", code).Msg(serverlog.OperationServerUpdate.Message("更新安装失败"))
	s.release()
}

func (s *UpdateService) status() (UpdateStatus, error) {
	settings, err := s.store.LoadSettings()
	if err != nil {
		return UpdateStatus{}, updateAppError(err, "无法读取更新设置")
	}
	state, err := s.store.LoadState()
	if err != nil {
		return UpdateStatus{}, updateAppError(err, "无法读取更新状态")
	}
	managed, reason := s.deploymentManaged()
	status := UpdateStatus{
		CurrentVersion: s.info.Version, CurrentCommit: s.info.Commit, OfficialBuild: s.info.Official, Comparable: s.info.Comparable,
		Channel: string(settings.Channel), Revision: settings.Revision, Phase: state.Phase, LatestVersion: state.TargetVersion,
		TargetVersion: state.TargetVersion, DeploymentManaged: managed, ManagedReason: reason, ErrorCode: state.ErrorCode, LastCheckedAt: state.LastCheckedAt,
	}
	if s.info.Comparable && state.TargetVersion != "" {
		current, currentErr := buildinfo.ParseVersion(s.info.Version)
		latest, latestErr := buildinfo.ParseVersion(state.TargetVersion)
		status.UpdateAvailable = currentErr == nil && latestErr == nil && latest.Compare(current) > 0
	}
	s.mu.Lock()
	installBusy := s.busy && s.busyKind == "install"
	s.mu.Unlock()
	status.InstallEnabled = status.UpdateAvailable && s.info.Official && !managed && !installBusy && state.Phase == updater.PhaseAvailable
	if !s.info.Official && reason == "" {
		status.ManagedReason = updateManagedDevBuild
	}
	return status, nil
}

func (s *UpdateService) authorize(actor Actor) error {
	if !actor.IsSystemAdmin() {
		return appError(CodePermissionDenied, "仅管理员可以管理 Server 更新", nil)
	}
	return nil
}

func (s *UpdateService) acquire(kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return appError(CodeConflict, "已有更新操作正在执行", nil)
	}
	state, err := s.store.LoadState()
	if err != nil {
		return updateAppError(err, "无法读取更新状态")
	}
	if updatePhaseActive(state.Phase) {
		return appError(CodeConflict, "已有更新操作正在执行", nil)
	}
	s.busy, s.busyKind = true, kind
	return nil
}

func (s *UpdateService) release() {
	s.mu.Lock()
	s.busy, s.busyKind = false, ""
	s.mu.Unlock()
}

func (s *UpdateService) deploymentManaged() (bool, string) {
	if strings.EqualFold(strings.TrimSpace(s.updateMode), "managed") {
		return true, updateManagedDeployment
	}
	if s.container != nil && s.container() {
		return true, updateManagedContainer
	}
	if s.goarch != "amd64" || (s.goos != "windows" && s.goos != "linux") {
		return true, updateManagedPlatform
	}
	info, err := os.Stat(s.executable)
	if err != nil || !info.Mode().IsRegular() || (s.goos != "windows" && info.Mode().Perm()&0o200 == 0) {
		return true, updateManagedExecutable
	}
	return false, ""
}

func (s *UpdateService) auditDenied(actor Actor, action string, request RequestContext) {
	if s.audit != nil && actor.User.ID != 0 {
		_ = s.audit.Record(nil, &actor.User.ID, action, "server_update", "settings", "denied", map[string]any{"error_code": CodePermissionDenied}, request)
	}
}

func (s *UpdateService) record(actor Actor, action, outcome string, metadata map[string]any, request RequestContext) {
	if actor.User.ID == 0 {
		return
	}
	s.recordID(&actor.User.ID, action, "settings", outcome, metadata, request)
}

func (s *UpdateService) recordID(actorID *uint, action, targetID, outcome string, metadata map[string]any, request RequestContext) {
	if s.audit != nil {
		_ = s.audit.Record(nil, actorID, action, "server_update", targetID, outcome, metadata, request)
	}
}

func (s *UpdateService) reconcileTerminalState() {
	if s.audit == nil {
		return
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	state, err := s.store.LoadState()
	if err != nil || state.OperationID == "" || (state.Phase != updater.PhaseSucceeded && state.Phase != updater.PhaseFailed && state.Phase != updater.PhaseRolledBack) {
		return
	}
	var count int64
	if s.audit.db.Model(&models.AuditLog{}).Where("action = ? AND target_type = ? AND target_id = ?", "server.update.result", "server_update", state.OperationID).Count(&count).Error != nil || count != 0 {
		return
	}
	outcome := string(state.Phase)
	metadata := map[string]any{"target_version": state.TargetVersion, "phase": string(state.Phase)}
	if state.ErrorCode != "" {
		metadata["error_code"] = state.ErrorCode
	}
	if s.audit.Record(nil, nil, "server.update.result", "server_update", state.OperationID, outcome, metadata, RequestContext{}) == nil {
		event := serverlog.OperationServerUpdate.Event(s.log.Info()).Str("target_version", state.TargetVersion).Str("status", string(state.Phase))
		if state.ErrorCode != "" {
			event = event.Str("error_code", state.ErrorCode)
		}
		event.Msg(serverlog.OperationServerUpdate.Message("更新结果已确认"))
	}
}

func (s *UpdateService) watchTerminalState() {
	state, err := s.store.LoadState()
	if err != nil {
		return
	}
	if !updatePhaseActive(state.Phase) {
		s.reconcileTerminalState()
		return
	}
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		timer := time.NewTimer(2 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ticker.C:
				current, loadErr := s.store.LoadState()
				if loadErr != nil {
					continue
				}
				if !updatePhaseActive(current.Phase) {
					s.reconcileTerminalState()
					return
				}
			case <-timer.C:
				return
			}
		}
	}()
}

func updatePhaseActive(phase updater.Phase) bool {
	switch phase {
	case updater.PhaseChecking, updater.PhaseDownloading, updater.PhaseReady, updater.PhaseWaitingForExit, updater.PhaseReplacing, updater.PhaseRestarting, updater.PhaseVerifying:
		return true
	default:
		return false
	}
}

func updateAppError(err error, message string) error {
	code := updater.ErrorCode(err)
	if code == "update_internal_error" {
		code = updater.CodePersistence
	}
	return &AppError{Code: code, Message: message, Cause: err}
}

func managedMessage(reason string) string {
	switch reason {
	case updateManagedContainer, updateManagedDeployment:
		return "当前部署方式负责 Server 更新"
	case updateManagedPlatform:
		return "当前平台不支持 Server 自更新"
	default:
		return "当前 Server 可执行文件不能安全替换"
	}
}

func defaultContainerDetector() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func defaultUpdateHelperLauncher(helperExecutable, planPath string) error {
	command := exec.Command(helperExecutable, updater.HelperFlag, planPath)
	command.Env = os.Environ()
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
