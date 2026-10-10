package contract

import "testing"

func TestDefaultSegmentRequiresExactMembershipWithoutChangingOrder(t *testing.T) {
	work := MediaWork{DefaultSegmentID: "latest", Segments: []MediaSegment{{ID: "first"}, {ID: "latest"}}}
	if err := ValidateDefaultSegment(work); err != nil {
		t.Fatal(err)
	}
	if work.Segments[0].ID != "first" {
		t.Fatal("default reordered official chronology")
	}
	for _, segments := range [][]MediaSegment{nil, {{ID: "first"}}, {{ID: "latest"}, {ID: "latest"}}} {
		work.Segments = segments
		if ValidateDefaultSegment(work) == nil {
			t.Fatal("foreign or duplicate default accepted")
		}
	}
	work.DefaultSegmentID = ""
	work.Segments = nil
	if err := ValidateDefaultSegment(work); err != nil {
		t.Fatal("old contract absent default rejected")
	}
}
