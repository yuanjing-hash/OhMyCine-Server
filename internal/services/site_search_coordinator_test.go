package services

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestSiteSearchCoordinatorUsesSharedBoundedParallelismAndMonotonicProgress(t *testing.T) {
	service := &SiteService{searchSlots: make(chan struct{}, 4)}
	records := make([]models.Site, 8)
	for i := range records {
		records[i] = models.Site{ID: uint(i + 1), Name: fmt.Sprintf("site-%d", i+1), TimeoutSeconds: 2}
	}
	var active, maxActive atomic.Int32
	progress := make([]SiteSearchProgress, 0, 20)
	err := service.runSiteSearches(context.Background(), records, func(_ context.Context, site models.Site) SiteSearchGroup {
		current := active.Add(1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		active.Add(-1)
		return SiteSearchGroup{SiteID: site.ID, SiteName: site.Name, Status: "success", Items: []SiteSearchResult{{Title: site.Name}}}
	}, nil, func(item SiteSearchProgress) { progress = append(progress, item) })
	if err != nil {
		t.Fatal(err)
	}
	if maxActive.Load() < 2 {
		t.Fatalf("search was not parallel: %d", maxActive.Load())
	}
	if maxActive.Load() > 4 {
		t.Fatalf("global limit exceeded: %d", maxActive.Load())
	}
	lastCompleted, lastResults := 0, 0
	for _, item := range progress {
		if item.Completed < lastCompleted || item.ResultCount < lastResults {
			t.Fatalf("progress regressed: %+v", item)
		}
		lastCompleted, lastResults = item.Completed, item.ResultCount
	}
	final := progress[len(progress)-1]
	if final.Completed != 8 || final.Succeeded != 8 || final.ResultCount != 8 {
		t.Fatalf("unexpected final progress: %+v", final)
	}
}
