package handlers

import (
	"encoding/json"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestParseSearchSiteScopeAcceptsOrderedDeduplicatedMultiSiteSelection(t *testing.T) {
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest("GET", "/api/v1/discovery/torrent-search?site_ids=3&site_ids=1,3", nil)
	siteID, siteIDs, err := parseSearchSiteScope(context)
	if err != nil || siteID != nil || !reflect.DeepEqual(siteIDs, []uint{3, 1}) {
		t.Fatalf("site_id=%v site_ids=%v err=%v", siteID, siteIDs, err)
	}
}

func TestParseSearchSiteScopeRejectsAmbiguousOrEmptySelection(t *testing.T) {
	for _, target := range []string{
		"/api/v1/discovery/torrent-search?site_id=1&site_ids=2",
		"/api/v1/discovery/torrent-search?site_ids=",
		"/api/v1/discovery/torrent-search?site_ids=0",
	} {
		context, _ := gin.CreateTestContext(httptest.NewRecorder())
		context.Request = httptest.NewRequest("GET", target, nil)
		if _, _, err := parseSearchSiteScope(context); err == nil {
			t.Fatalf("unsafe scope accepted: %s", target)
		}
	}
}

func TestShareValidationErrorEnvelope(t *testing.T) {
	for _, tc := range []struct {
		code, status string
		http         int
	}{{"pan115_share_expired", "expired", 410}, {"pan115_share_password_invalid", "password_required", 400}, {"pan115_rate_limited", "unavailable", 429}, {"pan115_auth_expired", "unavailable", 503}} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		validation := &services.SiteShareValidation{Status: tc.status, CheckedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), DownloaderID: "d"}
		writeError(ctx, zerolog.Nop(), &services.AppError{Code: tc.code, Message: "safe message", ShareValidation: validation})
		var body struct {
			Data struct {
				ErrorCode  string                       `json:"error_code"`
				Validation services.SiteShareValidation `json:"share_validation"`
			}
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != tc.http || body.Data.ErrorCode != tc.code || body.Data.Validation.Status != tc.status {
			t.Fatalf("response=%s", recorder.Body.String())
		}
	}
}
