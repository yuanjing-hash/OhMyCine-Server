package services

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestDownloadContentSelectionNodeWireBudgetIncludesUnionAndCleanup(t *testing.T) {
	sel := resourcecontent.Selection{Version: 1, Identity: resourcecontent.Identity{V1: strings.Repeat("a", 40)}, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "a.mkv", Size: 10}}}
	source := downloadpkg.Source{Kind: downloadpkg.SourceTorrent, Torrent: []byte("bounded-local-fixture"), ContentSelection: &sel}
	if err := validateNodeSelectionWireBudget(source, sel); err != nil {
		t.Fatal(err)
	}
	largeSource := source
	largeSource.Torrent = make([]byte, 900*1024)
	if err := validateNodeSelectionWireBudget(largeSource, sel); err == nil {
		t.Fatal("oversized sealed source passed pre-admission")
	}
	union := sel
	union.Files = append([]resourcecontent.SelectedFile(nil), sel.Files...)
	for i := 1; i < 2000; i++ {
		union.Files = append(union.Files, resourcecontent.SelectedFile{Index: i, Path: fmt.Sprintf("%s/%d.mkv", strings.Repeat("x", 200), i), Size: 10})
	}
	if err := validateNodeSelectionWireBudget(source, union); err == nil {
		t.Fatal("accumulated union/cleanup body passed pre-admission")
	}
}

func TestDownloadContentSelectionFreezeAndMissingFence(t *testing.T) {
	raw := []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	input := DownloadSourceInput{Kind: downloadpkg.SourceTorrent, Torrent: raw, Filename: "movie.torrent", ContentSelection: &resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}}
	source, _, err := normalizeDownloadSource(input, "movie")
	if err != nil {
		t.Fatal(err)
	}
	input.ContentSelection.Files[0].Size = 99
	raw[0] = 'x'
	if source.ContentSelection.Files[0].Size != 10 || source.Torrent[0] != 'd' || source.SelectionMode != "files" || validateStoredContentSelection(source) != nil {
		t.Fatal("selection aliases mutable input")
	}
	encoded, _ := json.Marshal(source)
	var restored downloadSourceEnvelope
	if json.Unmarshal(encoded, &restored) != nil || validateStoredContentSelection(restored) != nil {
		t.Fatal("private envelope lost selection")
	}
	restored.ContentSelection = nil
	if validateStoredContentSelection(restored) == nil {
		t.Fatal("lost selection became whole package")
	}
	if validateStoredContentSelection(downloadSourceEnvelope{Kind: downloadpkg.SourceURL, URL: "magnet:legacy"}) != nil {
		t.Fatal("legacy nil selection rejected")
	}
	input.ContentSelection.Files[0].Size = 10
	input.Torrent = source.Torrent
	input.ContentSelection.Identity.V1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, _, err := normalizeDownloadSource(input, "movie"); err == nil {
		t.Fatal("unverified identity accepted")
	}
}
