package services

import (
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/builtin"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func cloudSiteConfig(record models.Site) *sitepkg.CloudConfig {
	if record.Kind != pansou.Kind {
		return nil
	}
	var cfg sitepkg.CloudConfig
	if json.Unmarshal([]byte(record.CloudConfigJSON), &cfg) != nil {
		return nil
	}
	return &cfg
}
func normalizeCloudSite(kind string, cfg *sitepkg.CloudConfig, credential *siteCredentialEnvelope) (*sitepkg.CloudConfig, error) {
	if kind != pansou.Kind {
		if cfg != nil || credential.Username != "" || credential.Password != "" {
			return nil, appError(CodeSiteKindUnsupported, "这个站点不支持网盘频道配置", nil)
		}
		return nil, nil
	}
	credential.Cookie = ""
	credential.Passkey = ""
	credential.APIKey = ""
	normalized, err := pansou.NormalizeConfig(cfg)
	if err != nil {
		return nil, appError(CodeInvalidRequest, "请选择 115 网盘并填写 1–100 个有效的 TG 公开频道", nil)
	}
	credential.Username = strings.TrimSpace(credential.Username)
	if normalized.AuthEnabled && (credential.Username == "" || credential.Password == "") {
		return nil, appError(CodeSiteCredentialInvalid, "请输入 PanSou 用户名和密码", nil)
	}
	if len(credential.Username) > 128 || len(credential.Password) > 4096 || strings.ContainsAny(credential.Username+credential.Password, "\x00\r\n") {
		return nil, appError(CodeSiteCredentialInvalid, "PanSou 凭据格式无效", nil)
	}
	if !normalized.AuthEnabled {
		credential.Username = ""
		credential.Password = ""
	}
	return normalized, nil
}
func cloudConfigJSON(cfg *sitepkg.CloudConfig) string {
	if cfg == nil {
		return "{}"
	}
	raw, _ := json.Marshal(cfg)
	return string(raw)
}

// Search context does not override explicit theatrical release evidence in a share.
func siteResultMediaTypeHint(siteType, title, hint string) string {
	hint = safeRecognitionMediaTypeHint(hint)
	if siteType == builtin.SiteTypeCloud {
		if parsed, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: title, SourceKind: mediarecognition.SourceDownload}); err == nil {
			for _, evidence := range parsed.TypeEvidence {
				if evidence.Code == "franchise_movie_index" {
					return "movie"
				}
			}
		}
	}
	return hint
}
