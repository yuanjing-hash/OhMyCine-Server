package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"gorm.io/gorm"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/config"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/handlers"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/httpserver"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	pluginhostapi "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
	pluginrepository "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/repository"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader/pan115offline"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader/qbittorrent"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/mediatool"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

func main() {
	// This command is part of the updater's explicit capability handshake. It
	// must stay ahead of config loading, logging, migrations and all workers.
	if handled, err := updater.RunCompatibilityCommand(os.Args, os.Stdout); handled {
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, updater.ErrorCode(err))
			os.Exit(2)
		}
		return
	}
	if handled, exitCode := runUpdateHelper(os.Args); handled {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return
	}
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	runtimeDirectory, err := resolveUpdateRuntimeDirectory(cfg)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, updater.CodeCompatibilityRequired)
		os.Exit(1)
	}
	executable, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, updater.CodeCompatibilityRequired)
		os.Exit(1)
	}
	catalogCompatibility, err := updater.CheckStartupCompatibility(cfg.DatabasePath, runtimeDirectory, executable, os.Args[1:])
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, updater.ErrorCode(err))
		os.Exit(1)
	}
	exclusiveRuntime, err := database.AcquireExclusiveRuntime(cfg.DatabasePath)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "database_exclusive_runtime_unavailable")
		os.Exit(1)
	}
	defer func() { _ = exclusiveRuntime.Close() }()
	logManager, err := logging.NewManager(cfg.LogDirectory, cfg.Environment, os.Stdout)
	if err != nil {
		panic(err)
	}
	defer func() {
		if closeErr := logManager.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "close runtime logger: %v\n", closeErr)
		}
	}()
	log := logManager.Logger("server", "bootstrap")
	if err := catalogCompatibility.ReserveFreshDatabase(); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", updater.ErrorCode(err)).Msg(logging.OperationServerLifecycle.Message("新数据库初始化保护失败，未打开数据库"))
	}
	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "database_open_failed").Msg(logging.OperationServerLifecycle.Message("数据库打开失败"))
	}
	db, err = exclusiveRuntime.Bind(db)
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "database_exclusive_runtime_unavailable").Msg(logging.OperationServerLifecycle.Message("数据库进程独占保护校验失败"))
	}
	if err := validateCatalogDatabaseCompatibility(context.Background(), db, catalogCompatibility); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", updater.CodeFormatIncompatible).Msg(logging.OperationServerLifecycle.Message("目录格式不兼容，已停止启动并保留数据库"))
	}
	if err := database.Migrate(db); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "database_migration_failed").Msg(logging.OperationServerLifecycle.Message("数据库迁移失败"))
	}
	for {
		recovered, err := services.RecoverCatalogPhysicalRuntimeBatch(context.Background(), db)
		if err != nil {
			logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "catalog_physical_recovery_failed").Msg(logging.OperationServerLifecycle.Message("文件操作恢复凭据校验失败，未启动后台任务"))
		}
		if recovered == 0 {
			break
		}
	}
	if err := validateCatalogDatabaseCompatibility(context.Background(), db, catalogCompatibility); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", updater.CodeFormatIncompatible).Msg(logging.OperationServerLifecycle.Message("目录格式保护校验失败，未启动后台任务"))
	}
	// Keep catalogCompatibility as the explicit startup capability for the
	// future converter. No production conversion is enabled here: it must call
	// EnsureCatalogFormat before its first new-format activation transaction.
	catalogReadDB, err := database.OpenReadOnly(cfg.DatabasePath)
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "catalog_reader_initialization_failed").Msg(logging.OperationServerLifecycle.Message("目录只读连接初始化失败"))
	}
	catalogReadPool, err := catalogReadDB.DB()
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "catalog_reader_initialization_failed").Msg(logging.OperationServerLifecycle.Message("目录只读连接初始化失败"))
	}
	defer func() { _ = catalogReadPool.Close() }()
	catalogStore := services.NewCatalogSnapshotStore(db, catalogReadDB)
	credentialStore, err := credential.Open(cfg.CredentialKeyFile, cfg.CredentialMasterKey)
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "credential_store_initialization_failed").Msg(logging.OperationServerLifecycle.Message("凭据加密初始化失败"))
	}
	audit := services.NewAuditService(db)
	runtimeLogs, err := services.NewRuntimeLogService(db, logManager, audit)
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "runtime_logging_initialization_failed").Msg(logging.OperationServerLifecycle.Message("运行日志初始化失败"))
	}
	authorization := services.NewAuthorizationService(db)
	auth, err := services.NewAuthService(db, cfg, authorization, audit, logManager.Logger("authentication", "session"))
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "authentication_initialization_failed").Msg(logging.OperationServerLifecycle.Message("认证服务初始化失败"))
	}
	admin := services.NewAdminService(db, authorization, auth, audit)
	admin.SetCatalogSnapshotStore(catalogStore)
	cloudRegistry := cloudpkg.NewRegistry()
	if err := cloudRegistry.Register(cloudpkg.ProviderPan115, pan115.New); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "pan115_provider_registration_failed").Msg(logging.OperationServerLifecycle.Message("115 驱动注册失败"))
	}
	connections := services.NewConnectionService(db, audit, credentialStore, cloudRegistry, logManager.Logger("connection", "pan115"))
	credentialReveal := services.NewCredentialRevealService(db, audit, credentialStore)
	signedProxy, err := services.NewSignedProxyService(db, credentialStore, connections, cfg.PublicOrigin, logManager.Logger("proxy", "signed_strm"))
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "signed_proxy_initialization_failed").Msg(logging.OperationServerLifecycle.Message("302 代理初始化失败"))
	}
	signedProxy.SetCatalogSnapshotStore(catalogStore)
	if err := signedProxy.Start(context.Background()); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "pan115_playback_coordinator_start_failed").Msg(logging.OperationServerLifecycle.Message("115 多设备播放协调器启动失败"))
	}
	defer signedProxy.Close()
	embyGateway, err := services.NewEmbyGatewayService(db, audit, signedProxy, cfg.PublicOrigin, logManager.Logger("proxy", "emby_gateway"))
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "emby_gateway_initialization_failed").Msg(logging.OperationServerLifecycle.Message("Emby 302 网关初始化失败"))
	}
	providerDirectories := services.NewProviderDirectoryService(connections, credentialStore)
	storages := services.NewStorageService(db, audit)
	storages.SetCatalogSnapshotStore(catalogStore)
	connections.SetCatalogSnapshotStore(catalogStore)
	storages.SetConnectionService(connections)
	directories, err := services.NewDirectoryBrowserService(db, nil)
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "directory_browser_initialization_failed").Msg(logging.OperationServerLifecycle.Message("目录浏览器初始化失败"))
	}
	directories.SetProviderDirectoryService(providerDirectories)
	profiles := services.NewMediaClassificationProfileService(db, audit, nil)
	libraries := services.NewMediaLibraryService(db, audit, logManager.Logger("media_library", "supervisor"))
	libraries.SetCatalogSnapshotStore(catalogStore)
	libraries.SetConnectionService(connections)
	profiles.SetReferences(libraries)
	profiles.SetRevisionNotifier(libraries)
	storages.SetReferenceChecker(libraries)
	queue := services.NewQueueService(db, audit)
	queue.SetWriteAdmission(catalogStore.Admission())
	libraries.SetQueueService(queue)
	recycleCleanup := services.NewPan115RecycleCleanupService(db, queue, audit, connections, logManager.Logger("connection", "pan115_recycle_cleanup"))
	mediaChanges := services.NewMediaChangeService(db)
	mediaChanges.SetWriteAdmission(catalogStore.Admission())
	storages.SetMediaChangeService(mediaChanges)
	connections.SetMediaChangeService(mediaChanges)
	mediaServerRefresh := services.NewMediaServerRefreshService(db, queue, audit, connections)
	mediaChanges.SetReadyHandler(mediaServerRefresh.EnqueueLibrary)
	libraries.SetMediaChangeService(mediaChanges)
	artifacts := services.NewMediaArtifactService(db, queue, signedProxy, logManager.Logger("media_artifact", "worker"))
	artifacts.SetCatalogSnapshotStore(catalogStore)
	artifacts.SetConnectionService(connections)
	artifacts.SetMediaChangeService(mediaChanges)
	libraries.SetArtifactService(artifacts)
	libraryStructure := services.NewMediaLibraryStructureService(db, audit, queue, connections, logManager.Logger("media_library", "structure"))
	libraryStructure.SetCatalogSnapshotStore(catalogStore)
	libraryStructure.SetCatalogPublicationServices(mediaChanges, artifacts)
	libraryStructure.SetReconcileNotifier(libraries.RequestReconcile)
	libraries.SetStructureService(libraryStructure)
	strmManagement := services.NewSTRMManagementService(db, audit, queue, libraries, artifacts, logManager.Logger("strm", "management"))
	artifacts.SetCleanupService(strmManagement)
	providerRegistry := downloadpkg.NewRegistry()
	if err := providerRegistry.Register(models.DownloaderTypeQBittorrent, qbittorrent.Capabilities, func(config downloadpkg.Config) (downloadpkg.Client, error) { return qbittorrent.New(config) }); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "qbittorrent_provider_registration_failed").Msg(logging.OperationServerLifecycle.Message("qBittorrent 驱动注册失败"))
	}
	if err := providerRegistry.Register(models.DownloaderTypePan115Offline, pan115offline.Capabilities, func(config downloadpkg.Config) (downloadpkg.Client, error) { return pan115offline.New(config) }); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "pan115_downloader_registration_failed").Msg(logging.OperationServerLifecycle.Message("115 离线下载驱动注册失败"))
	}
	if cfg.Environment != "production" {
		fakeClient := downloadpkg.NewFakeClient()
		if err := providerRegistry.Register(models.DownloaderTypeFake, downloadpkg.FakeCapabilities, func(downloadpkg.Config) (downloadpkg.Client, error) { return fakeClient, nil }); err != nil {
			logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "fake_downloader_registration_failed").Msg(logging.OperationServerLifecycle.Message("测试下载驱动注册失败"))
		}
	}
	downloaders := services.NewDownloaderService(db, audit, credentialStore, providerRegistry)
	downloaders.SetConnectionService(connections)
	transferNodes := services.NewTransferNodeService(db, audit, credentialStore)
	downloaders.SetTransferNodeService(transferNodes)
	downloadSettings := services.NewDownloadSettingsService(db, audit)
	seedingSettings := services.NewSeedingSettingsService(db, audit)
	metadataSettings := services.NewMetadataSettingsService(db, audit, credentialStore, tmdb.Credential{Kind: tmdb.CredentialKind(cfg.TMDBDeploymentCredentialKind), Value: cfg.TMDBDeploymentCredentialValue})
	aiRecognitionSettings := services.NewAIRecognitionSettingsService(db, audit, credentialStore)
	discoveryService := services.NewDiscoveryService(db, metadataSettings, logManager.Logger("discovery", "service"))
	mediaCoverage := services.NewMediaCoverageService(db, metadataSettings)
	mediaCoverage.SetCatalogSnapshotStore(catalogStore)
	playerHistory := services.NewPlayerHistoryService(db, libraries)
	playerHistory.SetWriteAdmission(catalogStore.Admission())
	playerMediaState := services.NewPlayerMediaStateService(db, libraries)
	playerOverview := services.NewPlayerOverviewService(playerHistory, playerMediaState, libraries)
	libraries.SetMetadataSettingsService(metadataSettings)
	libraries.SetAIRecognitionSettings(aiRecognitionSettings)
	artifacts.SetMetadataSettingsService(metadataSettings)
	storages.AddReferenceChecker(downloadSettings)
	downloads := services.NewDownloadService(db, audit, credentialStore, downloaders, downloadSettings, queue, logManager.Logger("download", "service"))
	sites := services.NewSiteService(db, audit, credentialStore, downloads, logManager.Logger("site", "service"))
	if cfg.CloakBrowserCompanionURL != "" {
		cloakBrowser, cloakErr := sitepkg.NewCloakBrowserFetcher(cfg.CloakBrowserCompanionURL)
		if cloakErr != nil {
			logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "cloakbrowser_companion_config_invalid").Msg(logging.OperationServerLifecycle.Message("CloakBrowser companion 配置无效，仅允许本机回环地址"))
		}
		sites.SetRenderedFetcher(cloakBrowser)
	}
	sites.SetMetadataSettings(metadataSettings)
	sites.SetAIRecognitionSettings(aiRecognitionSettings)
	follows := services.NewFollowService(db, audit, queue, mediaCoverage, authorization)
	follows.SetDownloadService(downloads)
	cookieCloud := services.NewCookieCloudService(db, audit, credentialStore, sites, logManager.Logger("site", "cookiecloud"))
	unifiedSchedules := services.NewUnifiedScheduleService(db, queue, authorization, libraries, libraryStructure, strmManagement, follows, cookieCloud, logManager.Logger("scheduler", "unified"))
	unifiedSchedules.SetRecycleCleanup(recycleCleanup)
	downloads.SetMetadataSettings(metadataSettings)
	downloads.SetAIRecognitionSettings(aiRecognitionSettings)
	downloads.SetSeedingSettings(seedingSettings)
	libraries.SetIngestEnqueuer(downloads)
	transfers := services.NewTransferService(db, audit, queue, logManager.Logger("transfer", "service"))
	transfers.SetConnectionService(connections)
	transfers.SetDownloaderService(downloaders)
	transfers.SetMediaChangeService(mediaChanges)
	transfers.SetCatalogSnapshotStore(catalogStore)
	transfers.SetMediaArtifactService(artifacts)
	transfers.SetMediaLibraryStructureService(libraryStructure)
	transfers.SetMediaLibraryReconciler(libraries)
	reorganizations := services.NewMediaReorganizationService(db, audit, queue, metadataSettings, connections, logManager.Logger("media_reorganization", "worker"))
	reorganizations.SetMediaLibraryService(libraries)
	seeding := services.NewSeedingService(db, audit, queue, downloaders, logManager.Logger("seeding", "service"))
	pluginHost := pluginruntime.NewHost(context.Background())
	pluginHostAPI := pluginhostapi.New(db, credentialStore, logManager.Logger("plugin", "host"))
	pluginHost.SetCapabilityHost(pluginHostAPI)
	pluginRepositories := services.NewPluginRepositoryService(db, audit, pluginrepository.NewGitHubClient(nil), logManager.Logger("plugin", "repository"), services.WithPluginRoot(cfg.PluginDirectory), services.WithPluginRuntimeHost(pluginHost), services.WithPluginCredentialStore(credentialStore))
	sites.SetPluginResourceBridge(pluginRepositories)
	libraryArtwork := services.NewLibraryArtworkService(
		db, metadataSettings, pluginRepositories, pluginHostAPI, logManager.Logger("library_artwork", "generator"),
		services.WithLibraryArtworkRoot(filepath.Join(filepath.Dir(cfg.DatabasePath), "cache", "artwork", "categories")),
	)
	libraries.SetLibraryArtworkScheduler(libraryArtwork)
	libraryArtwork.SetCatalogSnapshotStore(catalogStore)
	libraries.EnableCatalogScanFollowups()
	pluginDownloads := services.NewPluginDownloadExecutor(downloads, pluginRepositories, pluginHostAPI, mediatool.Discover(cfg.FFmpegPath))
	downloads.SetPluginDownloadExecutor(pluginDownloads)
	if err := pluginRepositories.RestorePlugins(context.Background()); err != nil {
		logging.OperationPluginRuntime.Event(log.Fatal()).Str("error_code", services.ErrorCode(err)).Msg(logging.OperationPluginRuntime.Message("插件运行时恢复失败"))
	}
	if err := libraryArtwork.Start(context.Background()); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "library_artwork_start_failed").Msg(logging.OperationServerLifecycle.Message("媒体库分类封面服务启动失败"))
	}
	defer libraryArtwork.Close()
	defer func() {
		if err := pluginRepositories.ClosePlugins(context.Background()); err != nil {
			logging.OperationPluginRuntime.Event(log.Error()).Str("error_code", services.ErrorCode(err)).Msg(logging.OperationPluginRuntime.Message("插件运行时关闭失败"))
		}
	}()
	transfers.SetSeedingService(seeding)
	seeding.SetStagingCleanup(transfers.CleanupAfterSeeding)
	downloads.SetTransferService(transfers)
	updateStop := make(chan struct{}, 1)
	updateService, err := services.NewUpdateService(runtimeDirectory, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", cfg.Port), audit, logManager.Logger("server", "update"), func() {
		select {
		case updateStop <- struct{}{}:
		default:
		}
	})
	if err != nil {
		logging.OperationServerUpdate.Event(log.Fatal()).Str("error_code", services.ErrorCode(err)).Msg(logging.OperationServerUpdate.Message("更新服务初始化失败"))
	}
	api := handlers.NewAPI(cfg, auth, admin, audit, storages, directories, profiles, log)
	acquisition := services.NewAcquisitionService(db)
	api.SetAcquisitionService(acquisition)
	api.SetUpdateService(updateService)
	api.SetConnectionService(connections)
	api.SetCredentialRevealService(credentialReveal)
	api.SetSignedProxyService(signedProxy)
	api.SetEmbyGatewayService(embyGateway)
	api.SetProviderDirectoryService(providerDirectories)
	api.SetRuntimeLogService(runtimeLogs)
	api.SetMediaLibraryService(libraries)
	api.SetMediaLibraryStructureService(libraryStructure)
	api.SetSTRMManagementService(strmManagement)
	api.SetMediaChangeService(mediaChanges)
	api.SetMediaServerRefreshService(mediaServerRefresh)
	queueEvents := services.NewQueueEventHub()
	queue.SetEventHub(queueEvents)
	registry := services.NewWorkerRegistry()
	if cfg.Environment != "production" {
		services.RegisterFakeWorkers(registry)
	}
	if err := registry.Register("download", services.NewDownloadWorker(downloads)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "download_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("下载任务 Worker 注册失败"))
	}
	if err := registry.Register("transfer", services.NewTransferWorker(transfers)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "transfer_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("媒体整理 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeMediaReorganization, services.NewMediaReorganizationWorker(reorganizations)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "media_reorganization_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("重新整理 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeMediaLibraryRepair, services.NewMediaLibraryRepairWorker(libraryStructure)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "media_library_repair_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("媒体库结构修复 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeMediaLibraryStructureDiagnosis, services.NewMediaLibraryStructureDiagnosisWorker(libraryStructure)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "media_library_structure_diagnosis_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("目录结构诊断 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeMediaLibraryRecognition, services.NewMediaLibraryRecognitionWorker(libraries)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "media_library_recognition_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("媒体库识别 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeMediaLibraryRetirement, services.NewMediaLibraryRetirementWorker(libraries)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", "media_library_retirement_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("媒体库索引移除 Worker 注册失败"))
	}
	if err := registry.Register("seeding", services.NewSeedingWorker(seeding)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "seeding_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("做种管理 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeMediaArtifact, services.NewMediaArtifactWorker(artifacts)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "media_artifact_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("媒体产物 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeSTRMReconcile, services.NewSTRMReconcileWorker(strmManagement)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "strm_reconcile_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("STRM 刷新 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeMediaServerRefresh, services.NewMediaServerRefreshWorker(mediaServerRefresh)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "media_server_refresh_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("媒体服务器刷新 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeFollowSearch, services.NewFollowSearchWorker(follows, sites)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "follow_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("自动追更 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypePan115RecycleCleanup, services.NewPan115RecycleCleanupWorker(recycleCleanup)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "pan115_recycle_cleanup_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("115 回收站清理 Worker 注册失败"))
	}
	if err := registry.Register(services.JobTypeUnifiedSchedule, services.NewUnifiedScheduleWorker(unifiedSchedules)); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "unified_schedule_worker_registration_failed").Msg(logging.OperationServerLifecycle.Message("统一计划任务 Worker 注册失败"))
	}
	scheduler := services.NewScheduler(queue, registry, logManager.Logger("queue", "scheduler"))
	api.SetQueueService(queue)
	api.SetQueueEventHub(queueEvents)
	api.SetDownloaderService(downloaders)
	api.SetTransferNodeService(transferNodes)
	api.SetDownloadService(downloads)
	api.SetTransferService(transfers)
	api.SetMediaReorganizationService(reorganizations)
	api.SetDownloadSettingsService(downloadSettings)
	api.SetMetadataSettingsService(metadataSettings)
	api.SetAIRecognitionSettingsService(aiRecognitionSettings)
	api.SetSeedingSettingsService(seedingSettings)
	api.SetSeedingService(seeding)
	api.SetPluginRepositoryService(pluginRepositories)
	api.SetPluginAssetGateway(pluginHostAPI)
	api.SetLibraryArtworkService(libraryArtwork)
	api.SetDiscoveryService(discoveryService)
	api.SetMediaCoverageService(mediaCoverage)
	api.SetPlayerHistoryService(playerHistory)
	api.SetPlayerMediaStateService(playerMediaState)
	api.SetPlayerOverviewService(playerOverview)
	api.SetFollowService(follows)
	api.SetSiteService(sites)
	api.SetCookieCloudService(cookieCloud)
	api.SetUnifiedScheduleService(unifiedSchedules)
	if err := scheduler.Start(context.Background()); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "scheduler_start_failed").Msg(logging.OperationServerLifecycle.Message("任务调度器启动失败"))
	}
	defer scheduler.Close()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cleaned, err := downloads.ReconcileManagedProviderTags(ctx, 200)
		if err != nil {
			logging.OperationDownloadTask.Event(log.Warn()).Str("error_code", services.ErrorCode(err)).Msg(logging.OperationDownloadTask.Message("存量 qBittorrent 受管标签清理未完成"))
			return
		}
		if cleaned > 0 {
			logging.OperationDownloadTask.Event(log.Info()).Int("cleaned_tags", cleaned).Msg(logging.OperationDownloadTask.Message("存量 qBittorrent 受管标签清理完成"))
		}
	}()
	if err := unifiedSchedules.Start(context.Background()); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "unified_schedule_start_failed").Msg(logging.OperationServerLifecycle.Message("统一计划任务调度器启动失败"))
	}
	defer unifiedSchedules.Close()
	if err := mediaServerRefresh.RecoverPending(); err != nil {
		logging.OperationServerLifecycle.Event(log.Error()).Err(err).Str("error_code", "media_server_refresh_recovery_failed").Msg(logging.OperationServerLifecycle.Message("媒体服务器刷新恢复失败"))
	}
	if err := artifacts.RecoverCatalogArtifactBindings(context.Background(), 100); err != nil {
		logging.OperationServerLifecycle.Event(log.Error()).Str("error_code", services.ErrorCode(err)).Msg(logging.OperationServerLifecycle.Message("媒体产物任务恢复暂未完成，将自动重试"))
	}
	changeDispatchCtx, stopChangeDispatch := context.WithCancel(context.Background())
	changeDispatchDone := make(chan struct{})
	go func() {
		defer close(changeDispatchDone)
		mediaChanges.Run(changeDispatchCtx, func() error {
			artifactErr := artifacts.RecoverCatalogArtifactBindings(changeDispatchCtx, 100)
			refreshErr := mediaServerRefresh.RecoverPendingContext(changeDispatchCtx)
			_, cleanupErr := mediaChanges.CleanupPendingBatch(changeDispatchCtx)
			return errors.Join(artifactErr, refreshErr, cleanupErr)
		}, func(err error) {
			logging.OperationServerLifecycle.Event(log.Error()).Str("error_code", services.ErrorCode(err)).Msg(logging.OperationServerLifecycle.Message("媒体变更通知分发暂未完成，将自动重试"))
		})
	}()
	defer func() { stopChangeDispatch(); <-changeDispatchDone }()
	scanFollowupCtx, stopScanFollowups := context.WithCancel(context.Background())
	scanFollowupDone := make(chan struct{})
	go func() { defer close(scanFollowupDone); libraries.RunCatalogScanFollowups(scanFollowupCtx) }()
	defer func() { stopScanFollowups(); <-scanFollowupDone }()
	catalogMaintenanceCtx, stopCatalogMaintenance := context.WithCancel(context.Background())
	catalogMaintenanceDone := make(chan struct{})
	go func() {
		defer close(catalogMaintenanceDone)
		catalogStore.RunMaintenance(catalogMaintenanceCtx, func(err error) {
			logging.OperationServerLifecycle.Event(log.Warn()).Str("error_code", services.ErrorCode(err)).Msg(logging.OperationServerLifecycle.Message("媒体目录后台合并或清理暂未完成，将自动重试"))
		})
	}()
	defer func() { stopCatalogMaintenance(); <-catalogMaintenanceDone }()
	if err := libraries.Start(context.Background()); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "media_library_supervisor_start_failed").Msg(logging.OperationServerLifecycle.Message("媒体库监听启动失败"))
	}
	defer libraries.Close()
	providerEvents := services.NewProviderEventService(db, libraries, downloads)
	providerEventMonitor := services.NewProviderEventMonitor(db, connections, providerEvents, logManager.Logger("provider_event", "monitor"))
	if err := providerEventMonitor.Start(context.Background()); err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "provider_event_monitor_start_failed").Msg(logging.OperationServerLifecycle.Message("115 生活事件监听启动失败"))
	}
	defer providerEventMonitor.Close()
	server := &http.Server{
		Addr: cfg.Address(), Handler: httpserver.New(cfg, api, auth, logManager.Logger("http", "request")),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	listener, err := net.Listen("tcp", cfg.Address())
	if err != nil {
		logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "server_listen_failed").Msg(logging.OperationServerLifecycle.Message("服务监听失败"))
	}
	if err := catalogCompatibility.CompleteInitialization(); err != nil {
		_ = listener.Close()
		logging.OperationServerLifecycle.Event(log.Fatal()).Str("error_code", updater.ErrorCode(err)).Msg(logging.OperationServerLifecycle.Message("新数据库初始化证明保存失败，未开放服务"))
	}

	go func() {
		logging.OperationServerLifecycle.Event(log.Info()).Str("address", cfg.Address()).Msg(logging.OperationServerLifecycle.Message("OhMyCine Server 已启动"))
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.OperationServerLifecycle.Event(log.Fatal()).Err(err).Str("error_code", "server_listen_failed").Msg(logging.OperationServerLifecycle.Message("服务异常停止"))
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
	case <-updateStop:
		logging.OperationServerUpdate.Event(log.Info()).Msg(logging.OperationServerUpdate.Message("更新包已就绪，开始优雅停机"))
	}
	signal.Stop(stop)
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	logging.OperationServerLifecycle.Event(log.Info()).Msg(logging.OperationServerLifecycle.Message("OhMyCine Server 已停止"))
}

func runUpdateHelper(arguments []string) (bool, int) {
	if len(arguments) < 2 || arguments[1] != updater.HelperFlag {
		return false, 0
	}
	if len(arguments) != 3 || strings.TrimSpace(arguments[2]) == "" {
		_, _ = fmt.Fprintln(os.Stderr, updater.CodePlanInvalid)
		return true, 2
	}
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, updater.CodeCompatibilityRequired)
		return true, 1
	}
	if err := updater.RunHelper(context.Background(), arguments[2], updater.HelperOptions{DatabasePath: cfg.DatabasePath}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, updater.ErrorCode(err))
		return true, 1
	}
	return true, 0
}

func validateCatalogDatabaseCompatibility(ctx context.Context, db *gorm.DB, compatibility *updater.CatalogCompatibility) error {
	format, err := database.ReadCatalogFormat(ctx, db)
	if err != nil {
		return err
	}
	return compatibility.ValidateDatabaseFormat(format)
}

func resolveUpdateRuntimeDirectory(cfg config.Config) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("OMC_RUNTIME_DIR")); configured != "" {
		resolved, err := filepath.Abs(configured)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	}
	databasePath, err := filepath.Abs(cfg.DatabasePath)
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(filepath.Clean(databasePath))
	if strings.EqualFold(filepath.Base(directory), "data") {
		directory = filepath.Dir(directory)
	}
	return directory, nil
}
