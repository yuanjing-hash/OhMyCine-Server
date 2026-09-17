package services

import (
	"context"
	"encoding/json"
	"math"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type ProviderEventReviewItem struct {
	Name      string    `json:"name"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}
type ProviderEventReviewPage struct {
	List     []ProviderEventReviewItem `json:"list"`
	Total    int64                     `json:"total"`
	Page     int                       `json:"page"`
	PageSize int                       `json:"page_size"`
}

func providerReviewName(value string) string {
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return "名称不可用"
		}
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "\x00\r\n?:#") {
		return "名称不可用"
	}
	name := path.Base(strings.ReplaceAll(value, "\\", "/"))
	if name == "." || name == "/" || name == "" || len(name) > 512 {
		return "名称不可用"
	}
	return name
}
func providerReviewReason(code string) string {
	switch code {
	case "deletion_source_unproven":
		return "通知与当前媒体库来源不一致，已保留文件"
	case "deletion_artifact_source_unproven":
		return "无法确认 STRM 等产物属于当前媒体库来源，未执行删除"
	case "deletion_scope_too_large":
		return "目录涉及的文件超过本次安全处理范围，未执行删除；请手动完整核对"
	case "deletion_event_order_unproven":
		return "同一时间收到多条变更，无法确认先后顺序，未执行删除"
	case "deletion_identity_unproven", "deletion_directory_unproven":
		return "缺少文件或目录的本地归属记录，未执行删除"
	case "deletion_newer_observation":
		return "发现更新的文件记录，已保留当前文件"
	case "deletion_event_unavailable":
		return "通知的原始依据不可用，未执行删除"
	default:
		return "缺少安全处理依据，已暂停自动处理"
	}
}

func (s *MediaLibraryService) ProviderEventReviews(ctx context.Context, actor Actor, id uint, page int) (ProviderEventReviewPage, error) {
	result := ProviderEventReviewPage{List: []ProviderEventReviewItem{}, Page: page, PageSize: 50}
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(id)) {
		return result, appError(CodePermissionDenied, "无权查看媒体库", nil)
	}
	if page < 1 || page > math.MaxInt/result.PageSize {
		return result, appError(CodeInvalidRequest, "页码无效", nil)
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, id).Error; err != nil {
		return result, mediaLibraryNotFound(err)
	}
	query := s.db.WithContext(ctx).Model(&models.MediaLibraryProviderEvent{}).Where("library_id = ? AND processed_at IS NULL AND resolution_code = ?", id, providerEventNeedsReview)
	if err := query.Count(&result.Total).Error; err != nil {
		return result, err
	}
	var rows []models.MediaLibraryProviderEvent
	if err := query.Order("id DESC").Offset((page - 1) * result.PageSize).Limit(result.PageSize).Find(&rows).Error; err != nil {
		return result, err
	}
	for _, row := range rows {
		// A parked malformed event still has a displayable name; decoding it here
		// grants no processing authority and never forwards the original payload.
		var payload struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(row.PayloadJSON), &payload)
		result.List = append(result.List, ProviderEventReviewItem{Name: providerReviewName(payload.Name), Reason: providerReviewReason(row.ResolutionReason), CreatedAt: row.CreatedAt})
	}
	return result, nil
}
