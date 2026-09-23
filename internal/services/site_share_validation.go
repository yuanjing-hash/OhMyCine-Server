package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
)

// Validity describes a read at a point in time, never a guarantee of transfer.
type SiteShareValidation struct {
	Status       string    `json:"status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	Message      string    `json:"message"`
	CheckedAt    time.Time `json:"checked_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	DownloaderID string    `json:"downloader_id"`
}
type siteShareValidationEntry struct {
	ActorID                    uint
	SourceDigest, ConfigDigest string
	Validation                 SiteShareValidation
}

func shareSourceDigest(raw string) string {
	normalized, _, err := pan115.NormalizeShareLink(raw, "")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func (s *SiteService) rememberShareValidation(actor Actor, raw, downloaderID, config string, err error) SiteShareValidation {
	now := s.now()
	v := SiteShareValidation{Status: "valid", Message: "已验证可读取", CheckedAt: now, ExpiresAt: now.Add(5 * time.Minute), DownloaderID: downloaderID}
	if err != nil {
		v.Status = "unavailable"
		v.ErrorCode, _ = cloud.ErrorInfo(err)
		v.Message = ErrorMessage(sharePreviewProviderError(err))
		v.ExpiresAt = now.Add(15 * time.Second)
		switch v.ErrorCode {
		case cloud.CodeShareExpired:
			v.Status = "expired"
			v.ExpiresAt = now.Add(2 * time.Minute)
		case cloud.CodeSharePassword:
			v.Status = "password_required"
			v.ExpiresAt = now.Add(2 * time.Minute)
		}
	}
	source := shareSourceDigest(raw)
	if source == "" {
		return v
	}
	key := uintID(actor.User.ID) + ":" + source + ":" + downloaderID + ":" + config
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	if s.shareValidations == nil {
		s.shareValidations = map[string]siteShareValidationEntry{}
	}
	for k, entry := range s.shareValidations {
		if !entry.Validation.ExpiresAt.After(now) {
			delete(s.shareValidations, k)
		}
	}
	if len(s.shareValidations) >= 1024 {
		var oldest string
		var at time.Time
		for k, entry := range s.shareValidations {
			if oldest == "" || entry.Validation.CheckedAt.Before(at) {
				oldest = k
				at = entry.Validation.CheckedAt
			}
		}
		delete(s.shareValidations, oldest)
	}
	s.shareValidations[key] = siteShareValidationEntry{ActorID: actor.User.ID, SourceDigest: source, ConfigDigest: config, Validation: v}
	return v
}

// Search only reads remembered evidence. It never contacts 115.
func (s *SiteService) cachedShareValidation(ctx context.Context, actor Actor, raw string) *SiteShareValidation {
	source := shareSourceDigest(raw)
	if source == "" {
		return nil
	}
	s.vaultMu.Lock()
	candidates := []siteShareValidationEntry{}
	for _, entry := range s.shareValidations {
		if entry.ActorID == actor.User.ID && entry.SourceDigest == source && entry.Validation.ExpiresAt.After(s.now()) {
			candidates = append(candidates, entry)
		}
	}
	s.vaultMu.Unlock()
	var newest *SiteShareValidation
	for _, entry := range candidates {
		if !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, entry.Validation.DownloaderID) {
			continue
		}
		digest, _, err := sharePreviewConfig(s.db.WithContext(ctx), entry.Validation.DownloaderID)
		if err != nil || digest != entry.ConfigDigest {
			continue
		}
		if newest == nil || entry.Validation.CheckedAt.After(newest.CheckedAt) {
			v := entry.Validation
			newest = &v
		}
	}
	return newest
}

// Both preview and submit use the same account, safety gates and error model.
// Cache evidence is display-only; explicit reads always validate current state.
func (s *SiteService) inspectResultShare(ctx context.Context, actor Actor, claim siteResultClaim, raw, downloaderID string, full bool, request RequestContext) ([]cloud.ShareTreeItem, string, SiteShareValidation, error) {
	var validation SiteShareValidation
	if !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, downloaderID) {
		return nil, "", validation, appError(CodePermissionDenied, "无权使用这个下载器", nil)
	}
	digest, connectionID, err := sharePreviewConfig(s.db.WithContext(ctx), downloaderID)
	if err != nil {
		return nil, "", validation, err
	}
	if s.downloads == nil || s.downloads.downloader == nil || s.downloads.downloader.connections == nil {
		return nil, "", validation, appError(CodeDownloaderUnavailable, "下载服务不可用", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	release, err := s.acquireSharePreview(ctx)
	if err != nil {
		return nil, "", validation, err
	}
	defer release()
	started := time.Now()
	stage := "root"
	if full {
		stage = "tree"
	}
	_, driver, err := s.downloads.downloader.connections.driver(connectionID)
	var entries []cloud.ShareTreeItem
	if err == nil {
		if full {
			if browse, ok := driver.(cloud.ShareBrowseDriver); ok {
				_, entries, err = cloud.InspectShareTree(ctx, browse, raw)
			} else {
				err = cloud.Error(cloud.CodeUnavailable, false, nil)
			}
		} else {
			if share, ok := driver.(cloud.ShareReceiveDriver); ok {
				_, err = share.InspectShare(ctx, raw)
			} else {
				err = cloud.Error(cloud.CodeUnavailable, false, nil)
			}
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, "", validation, ctx.Err()
	}
	// A short local fence can outlive the read deadline; it must not turn a
	// provider timeout into a misleading account-configuration error.
	fenceCtx := ctx
	if ctx.Err() != nil {
		var fenceCancel context.CancelFunc
		fenceCtx, fenceCancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer fenceCancel()
	}
	// Never accept evidence after an account/configuration change during I/O.
	current, _, configErr := sharePreviewConfig(s.db.WithContext(fenceCtx), downloaderID)
	if configErr != nil || current != digest {
		return nil, "", validation, sharePreviewExpired()
	}
	if errors.Is(err, context.Canceled) {
		return nil, "", validation, err
	}
	validation = s.rememberShareValidation(actor, raw, downloaderID, digest, err)
	code := ""
	if err != nil {
		code, _ = cloud.ErrorInfo(err)
	}
	status, providerCode, cooling := pan115.ShareReadDiagnostics(err)
	if err == nil {
		status = 200
	}
	_, shareID, _ := pan115.NormalizeShareLink(raw, "")
	fingerprint := sha256.Sum256([]byte("115:" + shareID))
	event := s.log.Info()
	if err != nil {
		event = s.log.Warn()
	}
	serverlog.OperationDiscoverySearch.Event(event).Str("request_id", request.RequestID).Uint("site_id", claim.SiteID).Str("downloader_id", downloaderID).Uint("connection_id", connectionID).Str("share_fingerprint", hex.EncodeToString(fingerprint[:])).Str("stage", stage).Int("upstream_status", status).Int("provider_code", providerCode).Bool("local_cooldown", cooling).Str("error_code", code).Int64("duration_ms", time.Since(started).Milliseconds()).Msg(serverlog.OperationDiscoverySearch.Message("分享读取完成"))
	if err != nil {
		safe := sharePreviewProviderError(err)
		var app *AppError
		if errors.As(safe, &app) {
			app.ShareValidation = &validation
		}
		return nil, digest, validation, safe
	}
	return entries, digest, validation, nil
}
