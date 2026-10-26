package services

import (
	"context"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BrowserHistoryOptions changes presentation only. Exact episode/version rows
// and the Player synchronization protocol retain their existing identities.
type BrowserHistoryOptions struct {
	GroupByWork bool
	LibraryID   uint
	WorkID      string
}

type browserHistoryWork struct {
	libraryID uint
	workID    string
}

func (s *PlayerHistoryService) BrowserListWithOptions(actor Actor, page, pageSize int, options BrowserHistoryOptions) (BrowserHistoryPage, error) {
	if page < 1 || page > 100_000 || pageSize < 1 || pageSize > 100 {
		return BrowserHistoryPage{}, appError(CodeInvalidRequest, "播放历史分页参数无效", nil)
	}
	if (options.LibraryID == 0) != (options.WorkID == "") || len(options.WorkID) > 2048 {
		return BrowserHistoryPage{}, appError(CodeInvalidRequest, "播放历史作品范围无效", nil)
	}
	if options.WorkID != "" {
		if _, err := decodeCatalogToken(options.WorkID); err != nil {
			return BrowserHistoryPage{}, appError(CodeInvalidRequest, "播放历史作品范围无效", nil)
		}
	}
	wantedOffset := (page - 1) * pageSize
	result := BrowserHistoryPage{List: make([]BrowserHistoryItem, 0, pageSize), Page: page, PageSize: pageSize}
	selected := make([]PlayerHistoryChange, 0, pageSize)
	counts := make([]int, 0, pageSize)
	// A selected index, or -1 for a work outside this page. The complete visible
	// traversal groups before counting/paging and can count later episodes for a
	// selected work without retaining every history row.
	seen := make(map[browserHistoryWork]int)
	err := s.visitAvailableHistory(actor, true, false, func(change PlayerHistoryChange) bool {
		item, ok := browserHistoryItem(change, nil)
		if !ok || (options.LibraryID != 0 && (item.LibraryID != options.LibraryID || item.WorkID != options.WorkID)) {
			return true
		}
		key := browserHistoryWork{item.LibraryID, item.WorkID}
		if options.GroupByWork {
			if index, exists := seen[key]; exists {
				if index >= 0 {
					counts[index]++
				}
				return true
			}
			seen[key] = -1
		}
		if result.Total >= int64(wantedOffset) && len(selected) < pageSize {
			if options.GroupByWork {
				seen[key] = len(selected)
			}
			selected = append(selected, change)
			counts = append(counts, 1)
		}
		result.Total++
		return true
	})
	if err != nil {
		return BrowserHistoryPage{}, err
	}
	// Image configuration can need another pool connection. Decorate only after
	// releasing the pinned catalogue/history read transaction.
	result.List = browserHistoryItems(selected, s.libraries)
	for index := range result.List {
		if options.GroupByWork {
			result.List[index].RecordCount = counts[index]
		}
	}
	result.HasMore = int64(wantedOffset+len(result.List)) < result.Total
	return result, nil
}

// SetBrowserHistoryWatched edits the actor's existing exact history row. It is
// not a work-wide completion action and does not manufacture playback progress.
func (s *PlayerHistoryService) SetBrowserHistoryWatched(ctx context.Context, actor Actor, historyID string, completed bool) error {
	if !actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		return appError(CodePermissionDenied, "没有媒体库读取权限", nil)
	}
	historyID = strings.ToLower(strings.TrimSpace(historyID))
	if len(historyID) != 64 || !isHex(historyID) {
		return appError(CodeInvalidRequest, "播放历史标识无效", nil)
	}
	return withForegroundTransaction(ctx, s.db, s.writeAdmission, func(tx *gorm.DB) error {
		var row models.PlayerPlaybackHistory
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"user_id = ? AND sync_key = ? AND source_kind = ? AND deleted = ?",
			actor.User.ID, historyID, "server", false,
		).First(&row).Error
		if err == gorm.ErrRecordNotFound {
			return appError(CodeNotFound, "播放历史不存在", nil)
		}
		if err != nil {
			return err
		}
		change := playerHistoryChangeDTO(row)
		if _, ok := browserHistoryItem(change, nil); !ok {
			return appError(CodeNotFound, "播放历史不存在", nil)
		}
		available, err := s.browserHistoryAvailabilityTx(tx, actor, []models.PlayerPlaybackHistory{row})
		if err != nil {
			return err
		}
		if len(available) != 1 || !available[0] {
			return appError(CodeNotFound, "播放历史不存在", nil)
		}
		if row.Completed == completed {
			return nil
		}
		now := time.Now()
		if s.now != nil {
			now = s.now()
		}
		if row.ClientUpdatedAt > now.Add(playerHistoryFutureTolerance).UnixMilli() {
			return appError(CodeHistoryClockAhead, "播放历史时间明显超前，请校准设备时间后重试", nil)
		}
		change.Completed, change.UpdatedAt = completed, now.UnixMilli()
		if change.UpdatedAt <= row.ClientUpdatedAt {
			change.UpdatedAt = row.ClientUpdatedAt + 1
		}
		_, err = writePlayerHistoryChange(tx, actor.User.ID, change)
		return err
	})
}
