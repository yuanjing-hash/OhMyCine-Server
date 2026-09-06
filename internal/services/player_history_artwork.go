package services

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	HistoryArtworkMaxBytes    = 2 << 20
	historyArtworkMaxPixels   = 16_000_000
	historyArtworkQuotaBytes  = 64 << 20
	historyArtworkQuotaCount  = 512
	CodeHistoryArtworkInvalid = "history_artwork_invalid"
	CodeHistoryArtworkQuota   = "history_artwork_quota"
)

type HistoryArtworkReceipt struct {
	AssetID  string `json:"asset_id"`
	Revision uint64 `json:"revision"`
}

func historyArtworkColumn(slot string) (string, bool) {
	switch slot {
	case "poster":
		return "poster_asset_id", true
	case "backdrop":
		return "backdrop_asset_id", true
	case "title_logo":
		return "title_logo_asset_id", true
	default:
		return "", false
	}
}

// Stage decoding/re-encoding outside the SQLite writer. Commit bytes and their
// association in one transaction: unlike filesystem + DB publication, failure
// cannot leave an orphan or a pointer to missing bytes. No URL is ever fetched.
func (s *PlayerHistoryService) PutArtwork(ctx context.Context, actor Actor, key, slot, contentType string, input io.Reader) (HistoryArtworkReceipt, error) {
	column, ok := historyArtworkColumn(slot)
	if !ok || len(key) != 64 || !isHex(key) {
		return HistoryArtworkReceipt{}, appError(CodeHistoryArtworkInvalid, "历史图片参数无效", nil)
	}
	query := s.db.WithContext(ctx)
	var owned int64
	if err := query.Model(&models.PlayerPlaybackHistory{}).Where("user_id = ? AND sync_key = ? AND deleted = ? AND source_kind <> ?", actor.User.ID, key, false, "server").Count(&owned).Error; err != nil {
		return HistoryArtworkReceipt{}, err
	}
	if owned != 1 {
		return HistoryArtworkReceipt{}, appError(CodeNotFound, "播放历史不存在或不支持上传图片", nil)
	}
	// Bound concurrent image decoding as well as bytes/pixels; queued bodies
	// are not retained by this service.
	select {
	case s.artworkSlots <- struct{}{}:
		defer func() { <-s.artworkSlots }()
	default:
		return HistoryArtworkReceipt{}, appError(CodeHistoryArtworkQuota, "图片处理繁忙，请稍后重试", nil)
	}
	body, err := normalizeHistoryArtwork(ctx, contentType, input)
	if err != nil {
		return HistoryArtworkReceipt{}, err
	}
	result := HistoryArtworkReceipt{AssetID: uuid.NewString()}
	err = query.Transaction(func(tx *gorm.DB) error {
		var history models.PlayerPlaybackHistory
		if err := tx.Where("user_id = ? AND sync_key = ? AND deleted = ? AND source_kind <> ?", actor.User.ID, key, false, "server").First(&history).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return appError(CodeNotFound, "播放历史不存在", nil)
			}
			return err
		}
		var usage struct {
			Bytes int64
			Count int64
		}
		if err := tx.Model(&models.PlayerHistoryArtwork{}).Select("COALESCE(SUM(length(body)),0) AS bytes, COUNT(*) AS count").Where("user_id = ? AND NOT (sync_key = ? AND slot = ?)", actor.User.ID, key, slot).Scan(&usage).Error; err != nil {
			return err
		}
		if usage.Bytes+int64(len(body)) > historyArtworkQuotaBytes || usage.Count >= historyArtworkQuotaCount {
			return appError(CodeHistoryArtworkQuota, "该账号历史图片额度已满", nil)
		}
		if err := tx.Where("user_id = ? AND sync_key = ? AND slot = ?", actor.User.ID, key, slot).Delete(&models.PlayerHistoryArtwork{}).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		asset := models.PlayerHistoryArtwork{ID: result.AssetID, UserID: actor.User.ID, SyncKey: key, Slot: slot, Body: body, CreatedAt: now}
		if err := tx.Create(&asset).Error; err != nil {
			return err
		}
		revision := models.PlayerPlaybackHistoryRevision{UserID: actor.User.ID, SyncKey: key, ChangedAt: now}
		if err := tx.Create(&revision).Error; err != nil {
			return err
		}
		result.Revision = revision.ID
		return tx.Model(&models.PlayerPlaybackHistory{}).Where("user_id = ? AND sync_key = ?", actor.User.ID, key).Updates(map[string]any{column: result.AssetID, "revision": revision.ID, "updated_at": now}).Error
	})
	return result, err
}

func normalizeHistoryArtwork(ctx context.Context, contentType string, input io.Reader) ([]byte, error) {
	invalid := func() ([]byte, error) {
		return nil, appError(CodeHistoryArtworkInvalid, "只支持不超过 2 MiB、1600 万像素的有效 PNG/JPEG 图片", nil)
	}
	mimeType, _, err := mime.ParseMediaType(contentType)
	if err != nil || (mimeType != "image/jpeg" && mimeType != "image/png") {
		return invalid()
	}
	data, err := io.ReadAll(io.LimitReader(input, HistoryArtworkMaxBytes+1))
	if err != nil || len(data) > HistoryArtworkMaxBytes {
		return invalid()
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != mimeType || config.Width < 1 || config.Height < 1 || config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > historyArtworkMaxPixels {
		return invalid()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return invalid()
	}
	var output bytes.Buffer
	if format == "png" {
		err = png.Encode(&output, decoded) // Preserve transparent title logos.
	} else {
		err = jpeg.Encode(&output, decoded, &jpeg.Options{Quality: 85})
	}
	if err != nil || output.Len() > HistoryArtworkMaxBytes {
		return invalid()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (s *PlayerHistoryService) Artwork(ctx context.Context, actor Actor, id string) ([]byte, error) {
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return nil, appError(CodeNotFound, "历史图片不存在", nil)
	}
	var asset models.PlayerHistoryArtwork
	err := s.db.WithContext(ctx).Model(&models.PlayerHistoryArtwork{}).Select("player_history_artworks.*").
		Joins("JOIN player_playback_history AS h ON h.user_id = player_history_artworks.user_id AND h.sync_key = player_history_artworks.sync_key").
		Where("player_history_artworks.id = ? AND player_history_artworks.user_id = ? AND h.deleted = ? AND h.source_kind <> ?", id, actor.User.ID, false, "server").First(&asset).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, appError(CodeNotFound, "历史图片不存在", nil)
	}
	return asset.Body, err
}

func browserHistoryArtworkURL(id string) string {
	if parsed, err := uuid.Parse(id); err == nil && parsed.String() == id {
		return "/api/v1/media-libraries/history/artwork/" + id
	}
	return ""
}
