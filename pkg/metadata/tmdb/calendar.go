package tmdb

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
)

const MaxCalendarEpisodes = 5000
const maxCalendarResponseBytes = 8 << 20

// SeasonSchedule is a credential-free, complete bounded season observation.
// It deliberately does not alter the ordinary Player/Follow metadata budgets.
type SeasonSchedule struct {
	SeasonNumber int               `json:"season_number"`
	Episodes     []EpisodeSnapshot `json:"episodes"`
}

func (c *Client) GetTVSeasonSchedule(ctx context.Context, tvID int64, season int, language string) (SeasonSchedule, error) {
	result := SeasonSchedule{SeasonNumber: season, Episodes: []EpisodeSnapshot{}}
	if tvID <= 0 || season < 0 || season > 10000 {
		return result, clientError(ErrorInvalidRequest, nil)
	}
	values := url.Values{"language": []string{language}}
	endpoint := "/tv/" + strconv.FormatInt(tvID, 10) + "/season/" + strconv.Itoa(season)
	body, err := c.getAtBounded(ctx, c.apiBase, endpoint, values, maxCalendarResponseBytes)
	if err != nil && c.fallbackAPI != "" && isNetworkFailure(err) {
		body, err = c.getAtBounded(ctx, c.fallbackAPI, endpoint, values, maxCalendarResponseBytes)
	}
	if err != nil {
		return result, err
	}
	var response struct {
		SeasonNumber *int `json:"season_number"`
		Episodes     []struct {
			ID            int64  `json:"id"`
			SeasonNumber  int    `json:"season_number"`
			EpisodeNumber int    `json:"episode_number"`
			Name          string `json:"name"`
			AirDate       string `json:"air_date"`
		} `json:"episodes"`
	}
	if json.Unmarshal(body, &response) != nil || response.SeasonNumber == nil || *response.SeasonNumber != season || response.Episodes == nil || len(response.Episodes) > MaxCalendarEpisodes {
		return result, clientError(ErrorInvalidResponse, nil)
	}
	seen := map[int]bool{}
	for _, episode := range response.Episodes {
		if episode.EpisodeNumber <= 0 || episode.EpisodeNumber > 100000 || episode.SeasonNumber != season || seen[episode.EpisodeNumber] {
			return result, clientError(ErrorInvalidResponse, nil)
		}
		seen[episode.EpisodeNumber] = true
		date := cleanDate(episode.AirDate)
		if episode.AirDate != "" && date == "" {
			return result, clientError(ErrorInvalidResponse, nil)
		}
		result.Episodes = append(result.Episodes, EpisodeSnapshot{TMDBID: max(0, episode.ID), SeasonNumber: season, EpisodeNumber: episode.EpisodeNumber, Name: cleanText(episode.Name, 512), AirDate: date})
	}
	sort.Slice(result.Episodes, func(i, j int) bool { return result.Episodes[i].EpisodeNumber < result.Episodes[j].EpisodeNumber })
	return result, nil
}
