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

func TestSiteSearchCoordinatorIsolatesTimeout(t *testing.T) {
	service := &SiteService{searchSlots: make(chan struct{}, 2)}
	records := []models.Site{{ID: 1, Name: "slow", TimeoutSeconds: 1}, {ID: 2, Name: "fast", TimeoutSeconds: 2}}
	groups := map[uint]SiteSearchGroup{}
	err := service.runSiteSearches(context.Background(), records, func(ctx context.Context, site models.Site) SiteSearchGroup {
		if site.ID == 1 {
			<-ctx.Done()
			return SiteSearchGroup{SiteID: site.ID, SiteName: site.Name, Status: "error"}
		}
		return SiteSearchGroup{SiteID: site.ID, SiteName: site.Name, Status: "success"}
	}, func(group SiteSearchGroup) { groups[group.SiteID] = group }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if groups[1].Status != "error" || groups[2].Status != "success" {
		t.Fatalf("timeout was not isolated: %+v", groups)
	}
}
