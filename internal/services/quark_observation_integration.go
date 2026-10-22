package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

type quarkObservationReader struct{ driver cloudpkg.Driver }

func (r quarkObservationReader) ObserveDirectory(ctx context.Context, _ models.ProviderObservationScope, directory string, offset int64, limit int) (ObservationDirectoryPage, error) {
	p, err := r.driver.List(cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground), directory, cloudpkg.PageRequest{Offset: offset, Limit: int64(limit)})
	if err != nil {
		return ObservationDirectoryPage{}, err
	}
	result := ObservationDirectoryPage{Offset: p.Offset, HasMore: p.HasMore, Total: p.Total}
	for _, item := range p.Items {
		result.Items = append(result.Items, ObservationFact{ID: item.ID, ParentID: item.ParentID, Name: item.Name, IsDir: item.IsDir, Size: item.Size, ModifiedAt: item.ModifiedAt, ContentVersion: item.Revision, Digest: item.SHA1})
	}
	return result, nil
}

// SetProviderObservationService is wired once before library supervisors start.
// It never installs a second recursive scanner or a fabricated change journal.
func (s *MediaLibraryService) SetProviderObservationService(observations *ProviderObservationService) {
	s.observations = observations
	if observations == nil {
		return
	}
	observations.SetPublisher(s)
	observations.SetLogger(s.log)
	observations.SetReaderResolver(func(ctx context.Context, scope models.ProviderObservationScope) (ObservationDirectoryReader, error) {
		if s.connections == nil {
			return nil, ErrObservationUnavailable
		}
		_, driver, err := s.connections.driver(scope.ConnectionID)
		if err != nil {
			return nil, err
		}
		return quarkObservationReader{driver: driver}, nil
	})
	observations.SetFenceValidator(func(tx *gorm.DB, scope models.ProviderObservationScope) error {
		input, err := quarkObservationScopeTx(tx, scope.LibraryID)
		if scope.Kind == "intake" {
			input, err = quarkIntakeScopeInput(tx, scope.LibraryID, scope.DownloaderID, scope.OwnerID)
		}
		if err != nil {
			return err
		}
		if input.Fence != scope.Fence || input.RootID != scope.RootID || input.AccountKey != scope.AccountKey || input.ConnectionID != scope.ConnectionID {
			return ErrObservationFence
		}
		entered, err := catalogPhysicalWriteEntered(tx.Statement.Context, tx, scope.LibraryID)
		if err != nil {
			return err
		}
		if entered {
			return ErrObservationDeferred
		}
		retired, err := mediaLibraryRetiringTx(tx, scope.LibraryID)
		if err != nil {
			return err
		}
		if retired {
			return ErrObservationFence
		}
		return nil
	})
	observations.SetProgressListener(s.recordQuarkObservationProgress)
}

func quarkObservationScopeTx(tx *gorm.DB, libraryID uint) (ObservationScopeInput, error) {
	var library models.MediaLibrary
	var storage models.Storage
	var connection models.Connection
	var profile models.MediaClassificationProfile
	if err := tx.First(&library, libraryID).Error; err != nil {
		return ObservationScopeInput{}, err
	}
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return ObservationScopeInput{}, err
	}
	if !library.Enabled || !storage.Enabled || storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil {
		return ObservationScopeInput{}, ErrObservationFence
	}
	if err := tx.First(&connection, *storage.ConnectionID).Error; err != nil {
		return ObservationScopeInput{}, err
	}
	if !connection.Enabled || connection.Provider != models.ConnectionProviderQuark {
		return ObservationScopeInput{}, ErrObservationFence
	}
	if err := tx.First(&profile, library.ProfileID).Error; err != nil {
		return ObservationScopeInput{}, err
	}
	var head models.CatalogHead
	if err := tx.First(&head, "library_id = ?", library.ID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return ObservationScopeInput{}, err
	}
	// Recognition/dirty generation changes do not replace the observation scope.
	// The catalog publisher independently rebases current manual recognition.
	facts := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d\x00%t\x00%s\x00%s\x00%s\x00%s", catalogSourceFingerprint(library, storage), head.SourceEpoch, profile.ID, profile.Revision, connection.Revision, library.Recursive, library.VideoExtensionsJSON, library.STRMAssetExtraExtensionsJSON, library.IgnorePatternsJSON, library.MatchStrategy)
	fence := sha256.Sum256([]byte(facts))
	rootID := library.ProviderRootID
	if rootID == "" {
		rootID = storage.RootPath
	}
	return ObservationScopeInput{LibraryID: library.ID, ConnectionID: connection.ID, AccountKey: models.QuarkObservationAccountKey(connection.ID, connection.AccountID), Provider: models.ConnectionProviderQuark, Kind: "library", RootID: rootID, RootPath: path.Join("/", storage.RootDisplayPath, library.RelativeRoot), Fence: hex.EncodeToString(fence[:])}, nil
}

func (s *MediaLibraryService) ensureQuarkObservation(ctx context.Context, libraryID uint) (models.ProviderObservationScope, error) {
	if s.observations == nil {
		return models.ProviderObservationScope{}, ErrObservationUnavailable
	}
	var input ObservationScopeInput
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		input, err = quarkObservationScopeTx(tx, libraryID)
		return err
	})
	if err != nil {
		return models.ProviderObservationScope{}, err
	}
	return s.observations.EnsureScope(ctx, input)
}

func (s *MediaLibraryService) superviseQuarkObservation(ctx context.Context, libraryID uint, wake <-chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	var lastWarning time.Time
	for {
		scope, err := s.ensureQuarkObservation(ctx, libraryID)
		if err == nil && scope.RequestedScanRunID == 0 {
			_, err = s.requestQuarkObservation(ctx, libraryID, false)
		}
		if err == nil {
			err = s.processQuarkIntake(ctx, libraryID)
		}
		if err != nil && ctx.Err() == nil && time.Since(lastWarning) >= time.Minute {
			s.log.Warn().Uint("library_id", libraryID).Str("error_code", "quark_observation_waiting").Msg("夸克目录同步正在等待有效授权和媒体库配置")
			lastWarning = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

func (s *MediaLibraryService) requestQuarkObservation(ctx context.Context, libraryID uint, full bool) (models.MediaLibraryScanRun, error) {
	scope, err := s.ensureQuarkObservation(ctx, libraryID)
	if err != nil {
		return models.MediaLibraryScanRun{}, err
	}
	if full {
		if err = s.observations.RequestFullSync(ctx, scope.ID); err != nil {
			return models.MediaLibraryScanRun{}, err
		}
	}
	var run models.MediaLibraryScanRun
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&scope, "id = ?", scope.ID).Error; err != nil {
			return err
		}
		if scope.RequestedScanRunID != 0 {
			if err := tx.First(&run, scope.RequestedScanRunID).Error; err == nil && run.FinishedAt == nil {
				return nil
			} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		run = models.MediaLibraryScanRun{LibraryID: libraryID, Kind: "provider_observation", Status: "queued", Phase: "observing", Partial: true, SourceFingerprint: scope.Fence, CheckpointJSON: "{}", StartedAt: time.Now().UTC()}
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		return s.observations.BindScanRunTx(tx, scope.ID, run.ID)
	})
	return run, err
}

func (s *MediaLibraryService) recordQuarkObservationProgress(ctx context.Context, scope models.ProviderObservationScope, status ProviderObservationStatus) error {
	if scope.RequestedScanRunID == 0 {
		return nil
	}
	updates := map[string]any{"status": "running", "phase": "observing", "partial": true, "error_code": status.ErrorCode}
	if status.PathRepairs > 0 {
		updates["phase"] = "updating_paths"
	}
	if status.PendingConfirmations > 0 {
		updates["phase"] = "confirming_observations"
	}
	if status.PendingPublications > 0 {
		updates["phase"] = "publishing"
	}
	if status.ErrorCode != "" {
		updates["phase"] = "waiting_retry"
	}
	if status.LastCompleteAt != nil && !status.LastCompleteAt.Before(scope.CoverageStartedAt) && status.PendingDirectories == 0 && status.PendingPublications == 0 && status.PathRepairs == 0 && status.PendingConfirmations == 0 && status.ErrorCode == "" {
		updates = map[string]any{"status": "success", "phase": "completed", "partial": false, "finished_at": status.LastCompleteAt, "error_code": ""}
	}
	return s.db.WithContext(ctx).Model(&models.MediaLibraryScanRun{}).Where("id = ? AND library_id = ? AND finished_at IS NULL", scope.RequestedScanRunID, scope.LibraryID).Updates(updates).Error
}
