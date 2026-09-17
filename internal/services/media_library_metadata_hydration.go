package services

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

type detailLookupCall struct {
	done  chan struct{}
	match tmdb.Match
	err   error
}

// Lifetime is one bounded recognition batch, not a second persistent cache.
type batchDetailLookup struct {
	mediaRecognitionLookup
	mu    sync.Mutex
	calls map[string]*detailLookupCall
}

func (b *batchDetailLookup) GetByID(ctx context.Context, mediaType string, id int64, language string) (tmdb.Match, error) {
	key := mediaType + ":" + strconv.FormatInt(id, 10) + ":" + language
	b.mu.Lock()
	if call := b.calls[key]; call != nil {
		b.mu.Unlock()
		select {
		case <-call.done:
			return call.match, call.err
		case <-ctx.Done():
			return tmdb.Match{}, ctx.Err()
		}
	}
	call := &detailLookupCall{done: make(chan struct{})}
	b.calls[key] = call
	b.mu.Unlock()
	call.match, call.err = b.mediaRecognitionLookup.GetByID(ctx, mediaType, id, language)
	close(call.done)
	return call.match, call.err
}

// A compact, verified transfer identity is not a detail response. Older detail
// snapshots predate DetailsFetched; their detail-only fields remain reusable.
// Episode data is deliberately excluded: it can be fetched independently.
func recognitionDetailsComplete(snapshot tmdb.Snapshot) bool {
	return snapshot.DetailsFetched || snapshot.OriginalTitle != "" || snapshot.ReleaseDate != "" ||
		snapshot.Overview != "" || snapshot.OriginalLanguage != "" || snapshot.PosterPath != "" ||
		snapshot.BackdropPath != "" || snapshot.Status != "" || len(snapshot.Genres) > 0 ||
		len(snapshot.Seasons) > 0
}

func hydrateRecognitionDetails(ctx context.Context, lookup mediaRecognitionLookup, rate *mediaRecognitionRateGate, language string, result MediaRecognitionResult, rules classification.RulesV1) (MediaRecognitionResult, error) {
	if result.TMDBID == nil || *result.TMDBID <= 0 || (result.MediaType != "tv" && result.MediaType != "movie") {
		return result, nil // Non-TMDB provider identities have their own metadata.
	}
	if result.Snapshot.TMDBID == *result.TMDBID && result.Snapshot.MediaType == result.MediaType && recognitionDetailsComplete(result.Snapshot) {
		return result, nil
	}
	if lookup == nil {
		return result, appError(CodeTMDBUnavailable, "作品已识别，但完整资料尚未获取，请配置 TMDB 后重试", nil)
	}
	if err := rate.Wait(ctx); err != nil {
		return result, err
	}
	select {
	case mediaRecognitionGlobalGate <- struct{}{}:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	defer func() { <-mediaRecognitionGlobalGate }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	match, err := lookup.GetByID(ctx, result.MediaType, *result.TMDBID, language)
	if err != nil {
		return result, appError(tmdb.ErrorCode(err), "作品已识别，但完整资料获取失败，请重试补全资料", nil)
	}
	if match.ID != *result.TMDBID || match.MediaType != result.MediaType || match.Snapshot.TMDBID != match.ID || match.Snapshot.MediaType != match.MediaType {
		return result, appError(tmdb.ErrorInvalidResponse, "作品资料身份不一致，已保留原识别结果", nil)
	}
	previous := result.Snapshot
	result.Snapshot = match.Snapshot
	result.Snapshot.DetailsFetched = true
	if previous.TMDBID == match.ID && previous.MediaType == match.MediaType {
		result.Snapshot.EpisodeSnapshots = previous.EpisodeSnapshots
		result.Snapshot.EpisodeSeasons = previous.EpisodeSeasons
		result.Snapshot.EpisodeLanguage = previous.EpisodeLanguage
	}
	// Keep the verified/manual identity, display title and per-file hints. Only
	// detail/classification data is enriched; no title search runs here.
	result.Metadata = classificationMetadataForMatch(match)
	classified := classification.Classify(result.Metadata, rules)
	result.CategoryName, result.MatchedRuleID = classified.CategoryName, classified.MatchedRuleID
	return result, nil
}
