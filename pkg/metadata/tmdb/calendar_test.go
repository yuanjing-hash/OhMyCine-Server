package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCalendarScheduleKeepsLongSeasonsAndOrdinaryLimit(t *testing.T) {
	episodes := make([]map[string]any, 0, 1201)
	for n := 1201; n >= 1; n-- {
		episodes = append(episodes, map[string]any{"id": n, "season_number": 1, "episode_number": n, "name": fmt.Sprintf("Episode %d", n), "air_date": "2026-10-26", "overview": strings.Repeat("x", 1900)})
	}
	body, _ := json.Marshal(map[string]any{"season_number": 1, "episodes": episodes})
	if len(body) <= maxResponseBytes {
		t.Fatal("fixture must exceed the ordinary response budget")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/100/season/1" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = w.Write(body)
	}))
	defer upstream.Close()
	client, err := NewForTest("test", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := client.GetTVSeasonSchedule(context.Background(), 100, 1, "zh-CN")
	if err != nil || len(schedule.Episodes) != 1201 || schedule.Episodes[1200].EpisodeNumber != 1201 {
		t.Fatalf("schedule rows=%d err=%v", len(schedule.Episodes), err)
	}
	if _, err := client.GetTVSeasonEpisodes(context.Background(), 100, 1, "zh-CN"); err == nil {
		t.Fatal("calendar must not increase the ordinary response budget")
	}
}

func TestCalendarScheduleRejectsIncompleteAndOversizedResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"season_number":0}`, `{"season_number":0,"episodes":null}`, `{"season_number":0,"episodes":[{"season_number":0,"episode_number":1,"air_date":"bad"}]}`, `{"season_number":0,"episodes":[{"season_number":0,"episode_number":1},{"season_number":0,"episode_number":1}]}`, strings.Repeat(" ", maxCalendarResponseBytes+1)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer upstream.Close()
			client, _ := NewForTest("test", upstream.URL, upstream.Client())
			if _, err := client.GetTVSeasonSchedule(context.Background(), 100, 0, "zh-CN"); err == nil {
				t.Fatal("invalid season accepted as a complete empty schedule")
			}
		})
	}
}
