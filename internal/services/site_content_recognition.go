package services

import (
	"context"
	"strings"
)

// RecognizeTitleResult is the quick title lane. Actual file inspection has its
// own bounded request and cannot occupy a title or coverage worker.
func (s *SiteService) RecognizeTitleResult(ctx context.Context, actor Actor, token string) (SiteRecognitionSummary, error) {
	actor, err := s.inspectionActor(ctx, actor)
	if err != nil {
		return SiteRecognitionSummary{}, err
	}
	claim, _, err := s.contentClaim(ctx, actor, token)
	if err != nil {
		return SiteRecognitionSummary{}, err
	}
	// The quick lane is an advisory title fact. It must not race a verified
	// actual-file/Follow identity by binding an advertisement into the claim.
	result, err := s.recognizeSiteClaim(ctx, actor, claim, "", nil)
	if err != nil {
		return result, err
	}
	if _, err = s.inspectionActor(ctx, actor); err != nil {
		return SiteRecognitionSummary{}, err
	}
	current, _, err := s.contentClaim(ctx, actor, strings.TrimSpace(token))
	if err != nil || shareAnalysisIdentityDigest(claim) != shareAnalysisIdentityDigest(current) {
		return SiteRecognitionSummary{}, contentInspectionExpired()
	}
	return result, nil
}
