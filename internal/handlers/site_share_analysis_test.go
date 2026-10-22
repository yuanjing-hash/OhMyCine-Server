package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestShareAnalysisEnvelopeKeepsSparseGroupsForServerAndPlayer(t *testing.T) {
	season, id := 1, int64(100)
	analysis := &services.SiteShareAnalysis{Status: "complete", Complete: true, Groups: []services.SiteShareAnalysisGroup{{Title: "Actual Work", MediaType: "tv", TMDBID: &id, Season: &season, Episodes: []int{11, 13, 14}, VideoCount: 4, Status: "matched"}, {Title: "Movie", MediaType: "movie", Episodes: []int{}, VideoCount: 1, Status: "unrecognized"}}, VideoCount: 5, UnresolvedCount: 1, CheckedAt: time.Now(), Message: "已完整读取分享，集数来自实际视频文件"}
	for _, prefix := range []string{"/api/v1", "/api/v1/player"} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, prefix+"/discovery/torrent-results/recognize", nil)
		success(ctx, http.StatusOK, services.SiteRecognitionSummary{Status: "unrecognized", ShareAnalysis: analysis})
		var body struct {
			Data struct {
				Analysis services.SiteShareAnalysis `json:"share_analysis"`
				Episodes any                        `json:"episodes"`
			}
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || !body.Data.Analysis.Complete || !reflect.DeepEqual(body.Data.Analysis.Groups[0].Episodes, []int{11, 13, 14}) || body.Data.Analysis.Groups[1].Episodes == nil || body.Data.Episodes != nil {
			t.Fatalf("response=%s", recorder.Body.String())
		}
		for _, forbidden := range []string{"provider_id", "share_url", "entry_token", "selection_digest"} {
			if strings.Contains(recorder.Body.String(), forbidden) {
				t.Fatalf("private field=%s", forbidden)
			}
		}
	}
}

func TestUnavailableShareAnalysisSerializesEmptyGroupsWithoutTitleRange(t *testing.T) {
	response := services.SiteRecognitionSummary{Title: "Advertised 1-99", Status: "unrecognized", ShareAnalysis: &services.SiteShareAnalysis{Status: "unavailable", Groups: []services.SiteShareAnalysisGroup{}, ErrorCode: "downloader_unavailable", Message: "尚未读取分享文件"}}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"groups":[]`) || !strings.Contains(string(raw), `"complete":false`) || strings.Contains(string(raw), `"episodes"`) {
		t.Fatalf("response=%s", raw)
	}
}
