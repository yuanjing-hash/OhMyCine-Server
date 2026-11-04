package services

import (
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// FollowWait reports a locally observed gate from a completed run. Reading this
// projection never probes or changes the provider, claims or scheduler.
type FollowWait struct {
	NotBefore  time.Time `json:"not_before"`
	Reason     string    `json:"reason"`
	RecordedAt time.Time `json:"recorded_at"`
	ServerTime time.Time `json:"server_time"`
}

func followRunWait(run models.FollowRun, now time.Time) *FollowWait {
	if run.RetryNotBefore == nil || run.RetryNotBefore.IsZero() || run.RetryCode != cloud.RetryReasonProviderCooldown || run.FinishedAt == nil || (run.Status != models.FollowRunFailed && run.Status != models.FollowRunSubmitted) || (run.ErrorCode != "follow_submit_failed" && run.ErrorCode != "follow_submit_partial" && run.ErrorCode != "follow_content_unavailable" && run.ErrorCode != "follow_candidate_budget") {
		return nil
	}
	return &FollowWait{NotBefore: run.RetryNotBefore.UTC(), Reason: run.RetryCode, RecordedAt: run.FinishedAt.UTC(), ServerTime: now.UTC()}
}

// Only rows already filtered for actor resource access may enter this method.
// One local batch read avoids a provider probe or N+1 reads for list rendering.
func (s *FollowService) projectCurrentWaits(rows []models.FollowSubscription, summaries []FollowSummary) error {
	ids := []string{}
	for _, row := range rows {
		if row.LastRunID != nil {
			ids = append(ids, *row.LastRunID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var runs []models.FollowRun
	if err := s.db.Where("id IN ?", ids).Find(&runs).Error; err != nil {
		return err
	}
	byID := map[string]models.FollowRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	now := s.now()
	for index, row := range rows {
		item := &summaries[index]
		if row.LastRunID == nil || item.LastErrorMessage == "" || row.Status != models.FollowStatusActive {
			continue
		}
		run, ok := byID[*row.LastRunID]
		if !ok || run.SubscriptionID != row.ID || run.OwnerID != row.OwnerID || run.SubscriptionRevision != row.Revision || run.LifecycleRevision != row.LifecycleRevision || run.ErrorCode != row.LastErrorCode || run.HistoryClearedAt != nil {
			continue
		}
		item.Wait = followRunWait(run, now)
	}
	return nil
}

type followRetryObservation struct {
	Until       time.Time
	Reason      string
	Message     string
	Coordinates []FollowEpisodeCoordinate
}

func observeFollowRetry(observed *[]followRetryObservation, err error, candidate followCandidate, message ...string) {
	until, reason, _ := cloud.RetryTiming(err)
	safe := safeErrorMessage(err, "资源实际文件或入库路线未能核实")
	if len(message) > 0 {
		safe = message[0]
	}
	item := followRetryObservation{Until: until, Reason: reason, Message: safe}
	for _, episode := range candidate.Episodes {
		item.Coordinates = append(item.Coordinates, FollowEpisodeCoordinate{Season: candidate.Season, Episode: episode})
	}
	*observed = append(*observed, item)
}

func applyFollowRetryObservations(run *models.FollowRun, observed []followRetryObservation, missing map[[2]int]struct{}) (message string, failures int) {
	run.RetryNotBefore, run.RetryCode = nil, ""
	for _, item := range observed {
		for _, coordinate := range item.Coordinates {
			if _, ok := missing[[2]int{coordinate.Season, coordinate.Episode}]; ok {
				failures++
				message = item.Message
				if !item.Until.IsZero() && item.Reason == cloud.RetryReasonProviderCooldown && (run.RetryNotBefore == nil || item.Until.After(*run.RetryNotBefore)) {
					value := item.Until
					run.RetryNotBefore, run.RetryCode = &value, item.Reason
				}
				break
			}
		}
	}
	return message, failures
}
