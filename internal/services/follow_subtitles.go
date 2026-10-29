package services

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// SubtitlePolicy is frozen with a subscription. Nil is the legacy ignore policy.
type SubtitlePolicy struct {
	Mode      string   `json:"mode"`
	Languages []string `json:"languages"`
}

func effectiveSubtitlePolicy(policy *SubtitlePolicy) SubtitlePolicy {
	if policy == nil {
		return SubtitlePolicy{Mode: "ignore", Languages: []string{}}
	}
	return SubtitlePolicy{Mode: policy.Mode, Languages: append([]string{}, policy.Languages...)}
}

func normalizeSubtitlePolicy(input *SubtitlePolicy) (*SubtitlePolicy, error) {
	if input == nil {
		return nil, nil
	}
	policy := SubtitlePolicy{Mode: strings.ToLower(strings.TrimSpace(input.Mode)), Languages: []string{}}
	if policy.Mode != "ignore" && policy.Mode != "prefer" && policy.Mode != "require" || len(input.Languages) > 8 {
		return nil, appError(CodeFollowConfigurationInvalid, "字幕策略无效", nil)
	}
	seen := map[string]bool{}
	for _, raw := range input.Languages {
		value := ""
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "zh-hans", "zh-cn", "zh-sg":
			value = "zh-Hans"
		case "zh-hant", "zh-tw", "zh-hk":
			value = "zh-Hant"
		case "en", "ja", "ko":
			value = strings.ToLower(strings.TrimSpace(raw))
		default:
			return nil, appError(CodeFollowConfigurationInvalid, "字幕语言无效", nil)
		}
		if !seen[value] {
			seen[value] = true
			policy.Languages = append(policy.Languages, value)
		}
	}
	if policy.Mode != "ignore" && len(policy.Languages) == 0 {
		return nil, appError(CodeFollowConfigurationInvalid, "请选择至少一种字幕语言", nil)
	}
	return &policy, nil
}

type FollowPolicyDefaultsSummary struct {
	Revision  uint64         `json:"revision"`
	Subtitles SubtitlePolicy `json:"subtitles"`
	UpdatedAt time.Time      `json:"updated_at"`
}
type UpdateFollowPolicyDefaultsInput struct {
	Revision  uint64         `json:"revision"`
	Subtitles SubtitlePolicy `json:"subtitles"`
}

func (s *FollowService) policyDefaults(ctx context.Context) (FollowPolicyDefaultsSummary, error) {
	var row models.FollowPolicyDefaults
	if err := s.db.WithContext(ctx).First(&row, 1).Error; err != nil {
		return FollowPolicyDefaultsSummary{}, err
	}
	var policy SubtitlePolicy
	if json.Unmarshal([]byte(row.SubtitlesJSON), &policy) != nil {
		return FollowPolicyDefaultsSummary{}, appError(CodeFollowConfigurationInvalid, "默认字幕策略不可用", nil)
	}
	normalized, err := normalizeSubtitlePolicy(&policy)
	if err != nil {
		return FollowPolicyDefaultsSummary{}, err
	}
	return FollowPolicyDefaultsSummary{Revision: row.Revision, Subtitles: *normalized, UpdatedAt: row.UpdatedAt}, nil
}
func (s *FollowService) PolicyDefaults(ctx context.Context, actor Actor) (FollowPolicyDefaultsSummary, error) {
	if !actor.Can(authz.PermissionSettingsRead) {
		return FollowPolicyDefaultsSummary{}, appError(CodePermissionDenied, "无权查看默认订阅策略", nil)
	}
	return s.policyDefaults(ctx)
}
func (s *FollowService) UpdatePolicyDefaults(ctx context.Context, actor Actor, input UpdateFollowPolicyDefaultsInput, request RequestContext) (FollowPolicyDefaultsSummary, error) {
	if !actor.Can(authz.PermissionSettingsUpdate) {
		return FollowPolicyDefaultsSummary{}, appError(CodePermissionDenied, "无权编辑默认订阅策略", nil)
	}
	if input.Revision == 0 || input.Revision >= math.MaxInt64 {
		return FollowPolicyDefaultsSummary{}, appError(CodeConflict, "默认订阅策略版本无效，请刷新", nil)
	}
	policy, err := normalizeSubtitlePolicy(&input.Subtitles)
	if err != nil {
		return FollowPolicyDefaultsSummary{}, err
	}
	raw, _ := json.Marshal(policy)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.FollowPolicyDefaults{}).Where("id = 1 AND revision = ?", input.Revision).Updates(map[string]any{"revision": input.Revision + 1, "subtitles_json": string(raw), "updated_at": s.now()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return appError(CodeConflict, "默认订阅策略已被更新，请刷新", nil)
		}
		if s.audit != nil {
			return s.audit.Record(tx, &actor.User.ID, "follow_policy_defaults.update", "follow_policy_defaults", "1", "success", map[string]any{"subtitle_mode": policy.Mode}, request)
		}
		return nil
	})
	if err != nil {
		return FollowPolicyDefaultsSummary{}, err
	}
	return s.policyDefaults(ctx)
}
