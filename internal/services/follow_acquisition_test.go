package services

import (
	"encoding/json"
	"testing"
)

func TestFollowTransferPreferencePrecedesCoverageAndUsesActualSource(t *testing.T) {
	for _, preference := range []string{"transfer_first", "existing_order", ""} {
		t.Run(preference, func(t *testing.T) {
			var snapshot FollowExecutionSnapshot
			if err := json.Unmarshal([]byte(`{"filters":{"acquisition_preference":"`+preference+`"}}`), &snapshot); err != nil {
				t.Fatal(err)
			}
			share, _, ok := buildFollowCandidate(SiteSearchResult{SourceKind: "115_share", Title: "Show", ResourceFingerprint: "share"}, 1, 1, 1, snapshot)
			if !ok {
				t.Fatal("share candidate rejected")
			}
			magnet, _, ok := buildFollowCandidate(SiteSearchResult{SourceKind: "bt", Title: "Show.S01E13-E15", ResourceFingerprint: "magnet"}, 1, 0, 1, snapshot)
			if !ok {
				t.Fatal("magnet candidate rejected")
			}
			share.Episodes, magnet.Episodes = []int{13}, []int{13, 15}
			selected := selectFollowCandidates([]followCandidate{magnet, share}, map[[2]int]struct{}{{1, 13}: {}, {1, 15}: {}}, 2)
			want := "magnet"
			if preference == "transfer_first" {
				want = "share"
			}
			if len(selected) != 2 && preference == "transfer_first" || len(selected) == 0 || selected[0].Fingerprint != want {
				t.Fatalf("preference=%s selected=%+v", preference, selected)
			}
			if preference == "transfer_first" && !followCandidateLess(share, magnet) {
				t.Fatal("bounded preparation order did not prioritize actual share")
			}
		})
	}
}
