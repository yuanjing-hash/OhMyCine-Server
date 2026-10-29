package resourcecontent

import (
	"reflect"
	"strings"
	"testing"
)

func TestSelectionBindsCompleteManifest(t *testing.T) {
	identity := Identity{V1: strings.Repeat("a", 40), V2: strings.Repeat("b", 64)}
	manifest := Manifest{Identity: identity, Files: []File{{Index: 0, Path: "02.mkv", Size: 20}, {Index: 1, Path: ".pad/10", Size: 10, Padding: true}, {Index: 2, Path: "01.mkv", Size: 30}}}
	selection := Selection{Version: SelectionVersion, Identity: identity, Files: []SelectedFile{{Index: 2, Path: "01.mkv", Size: 30}, {Index: 0, Path: "02.mkv", Size: 20}}}
	if err := selection.ValidateAgainst(manifest); err != nil {
		t.Fatal(err)
	}
	before := append([]SelectedFile(nil), selection.Files...)
	digest, err := selection.Digest()
	if err != nil || len(digest) != 64 {
		t.Fatal(digest, err)
	}
	if !reflect.DeepEqual(before, selection.Files) {
		t.Fatal("digest mutated selection")
	}
	reversed := selection
	reversed.Files = []SelectedFile{selection.Files[1], selection.Files[0]}
	if second, _ := reversed.Digest(); digest != second {
		t.Fatal("digest depends on order")
	}
	for _, mutate := range []func(*Selection){
		func(s *Selection) { s.Version = 2 }, func(s *Selection) { s.Identity.V2 = "" }, func(s *Selection) { s.Identity.V1 = strings.Repeat("c", 40) },
		func(s *Selection) { s.Files = nil }, func(s *Selection) { s.Files[0].Index = 1 }, func(s *Selection) { s.Files[0].Path = "../01.mkv" },
		func(s *Selection) { s.Files[0].Path = "other.mkv" }, func(s *Selection) { s.Files[0].Size++ },
		func(s *Selection) { s.Files = append(s.Files, s.Files[0]) },
		func(s *Selection) { s.Files = []SelectedFile{{Index: 1, Path: ".pad/10", Size: 10}} },
	} {
		changed := selection
		changed.Files = append([]SelectedFile(nil), selection.Files...)
		mutate(&changed)
		if err := changed.ValidateAgainst(manifest); err == nil {
			t.Fatalf("accepted altered intent: %+v", changed)
		}
	}
	changed := selection
	changed.Files = append([]SelectedFile(nil), selection.Files...)
	changed.Files = changed.Files[:1]
	if second, _ := changed.Digest(); second == digest {
		t.Fatal("digest omits selection")
	}
	changed = selection
	changed.Identity.V2 = strings.Repeat("c", 64)
	if second, _ := changed.Digest(); second == digest {
		t.Fatal("digest omits full v2 identity")
	}
}
