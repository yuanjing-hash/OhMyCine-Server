package services

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type contentGroupRecognition struct {
	Summary   SiteRecognitionSummary
	ExpiresAt time.Time
}
type contentGroupFlight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	result  SiteRecognitionSummary
	err     error
}

// Cache identity only, never a caller's file tokens or episode declarations.
// The caller rebuilds actual per-file coordinates from its own frozen manifest.
func (s *SiteService) contentMetadataGeneration(ctx context.Context) string {
	if s.db == nil {
		return "unconfigured"
	}
	var metadata models.MetadataSettings
	var ai models.AIRecognitionSettings
	_ = s.db.WithContext(ctx).Select("revision", "tmdb_token_ciphertext", "api_base_url", "image_base_url").First(&metadata, 1).Error
	_ = s.db.WithContext(ctx).Select("revision").First(&ai, 1).Error
	return inspectionDigest([]any{mediarecognition.EngineVersion, metadata.Revision, metadata.TMDBTokenCiphertext, metadata.APIBaseURL, metadata.ImageBaseURL, ai.Revision})
}

func (s *SiteService) recognizeContentGroup(ctx context.Context, actor Actor, claim siteResultClaim, group SiteShareAnalysisGroup, files []recognitionSourceFile) (SiteRecognitionSummary, error) {
	generation := s.contentMetadataGeneration(ctx)
	key := inspectionDigest([]any{actor.User.ID, discoveryAuthorizationScope(actor), group.Title, group.MediaType, group.Year, group.Season, claim.SearchIdentity, claim.RecognitionLocked, claim.ManualTMDBID, claim.ManualMediaType, generation})
	return s.readContentGroupShared(ctx, key, func(work context.Context) (SiteRecognitionSummary, error) {
		state := s.contents()
		select {
		case state.groupSlots <- struct{}{}:
		case <-work.Done():
			return SiteRecognitionSummary{}, work.Err()
		}
		defer func() { <-state.groupSlots }()
		actual := claim
		actual.Title, actual.Subtitle, actual.MediaTypeHint = group.Title, "", group.MediaType
		// Preserve only the already parsed group's year; file evidence remains private.
		if group.Year != nil {
			raw, _ := json.Marshal(*group.Year)
			actual.Title += " (" + string(raw) + ")"
		}
		summary, err := s.recognizeSiteClaim(work, actor, actual, "", files)
		if err == nil && s.contentMetadataGeneration(work) != generation {
			return SiteRecognitionSummary{}, contentInspectionExpired()
		}
		return summary, err
	})
}

func (s *SiteService) readContentGroupShared(ctx context.Context, key string, load func(context.Context) (SiteRecognitionSummary, error)) (SiteRecognitionSummary, error) {
	state := s.contents()
	state.mu.Lock()
	if saved, ok := state.groupCache[key]; ok && saved.ExpiresAt.After(s.now()) {
		state.mu.Unlock()
		return saved.Summary, nil
	}
	flight := state.groupFlights[key]
	if flight == nil {
		if len(state.groupFlights) >= 32 {
			state.mu.Unlock()
			return SiteRecognitionSummary{}, appError(CodeSiteUnavailable, "作品核验排队较多，请稍后重试", nil)
		}
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		flight = &contentGroupFlight{done: make(chan struct{}), cancel: cancel}
		state.groupFlights[key] = flight
		go func() {
			summary, err := load(work)
			owned := work.Err() == nil
			cancel()
			state.mu.Lock()
			defer state.mu.Unlock()
			flight.result, flight.err = summary, err
			delete(state.groupFlights, key)
			if err == nil && owned {
				ttl := 30 * time.Second
				if summary.Status == mediaRecognitionStatusMatched {
					ttl = 5 * time.Minute
				}
				for id, saved := range state.groupCache {
					if !saved.ExpiresAt.After(s.now()) {
						delete(state.groupCache, id)
					}
				}
				if len(state.groupCache) >= 512 {
					oldest := ""
					var expiry time.Time
					for id, saved := range state.groupCache {
						if oldest == "" || saved.ExpiresAt.Before(expiry) {
							oldest, expiry = id, saved.ExpiresAt
						}
					}
					delete(state.groupCache, oldest)
				}
				state.groupCache[key] = contentGroupRecognition{Summary: summary, ExpiresAt: s.now().Add(ttl)}
			}
			close(flight.done)
		}()
	}
	flight.waiters++
	state.mu.Unlock()
	defer func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		flight.waiters--
		if flight.waiters == 0 {
			select {
			case <-flight.done:
			default:
				flight.cancel()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return SiteRecognitionSummary{}, ctx.Err()
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return SiteRecognitionSummary{}, err
		}
		return flight.result, flight.err
	}
}

// Each directory analysis spends at most eight metadata lookups in one ten-
// second budget. Independent groups share four permits across all callers.
func (s *SiteService) recognizeContentGroups(ctx context.Context, actor Actor, claim siteResultClaim, groups []SiteShareAnalysisGroup, files [][]recognitionSourceFile) {
	deadline := time.Now().Add(10 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Add(-2*time.Second).Before(deadline) {
		deadline = parent.Add(-2 * time.Second)
	}
	lookup, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var wg sync.WaitGroup
	lookups := 0
	for index := range groups {
		group := &groups[index]
		if shareGroupMatchesIdentity(*group, claim.SearchIdentity) {
			group.TMDBID = cloneInt64(&claim.SearchIdentity.TMDBID)
			group.Title, group.Status, group.IdentityStatus = claim.SearchIdentity.Title, mediaRecognitionStatusMatched, mediaIdentityStatusVerified
			continue
		}
		if lookup.Err() != nil || lookups >= 8 {
			group.ErrorCode = "share_recognition_budget"
			continue
		}
		lookups++
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			summary, err := s.recognizeContentGroup(lookup, actor, claim, groups[i], files[i])
			group := &groups[i]
			if err != nil {
				group.ErrorCode = ErrorCode(err)
				return
			}
			group.Status, group.IdentityStatus, group.TMDBID, group.ErrorCode, group.PosterURL = summary.Status, summary.IdentityStatus, summary.TMDBID, summary.ErrorCode, summary.PosterURL
			if summary.Status == mediaRecognitionStatusMatched {
				group.Title, group.MediaType, group.Year = summary.Title, summary.MediaType, summary.Year
				if group.MediaType == "tv" && group.Season == nil && summary.SeasonCount == 1 {
					one := 1
					group.Season = &one
				}
			}
		}(index)
	}
	wg.Wait()
}
