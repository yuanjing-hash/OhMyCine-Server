package services

import (
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
