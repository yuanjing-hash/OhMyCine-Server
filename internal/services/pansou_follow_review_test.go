package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestFollowReviewLastCandidateCancellationRemainsCancelled(t *testing.T) {
	f, driver := newFollowPreviewFixture(t)
	adapter := f.worker.sites.adapters["pansou_tg"].(*followSharesAdapter)
	adapter.items = adapter.items[:1]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver.onRead = cancel
	claimed, err := f.follows.queue.Claim([]string{JobTypeFollowSearch})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if result := f.worker.Run(ctx, &followProgressRuntime{}, *claimed); result.ErrorCode != "" {
		t.Fatalf("worker=%+v", result)
	}
	var run models.FollowRun
	if err := f.follows.db.First(&run, "job_id = ?", claimed.Job.ID).Error; err != nil {
		t.Fatal(err)
	}
	var tasks int64
	if err := f.follows.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ?", f.subscription.ID).Count(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != models.FollowRunCancelled || run.ErrorCode != "follow_cancelled" || tasks != 0 || len(driver.reads) != 1 {
		t.Fatalf("last cancelled candidate lost cancellation: status=%s code=%s tasks=%d reads=%d", run.Status, run.ErrorCode, tasks, len(driver.reads))
	}
}

type reviewShareAuthorizationRecovery struct {
	cloud.ShareBrowseDriver
	resume         func()
	resumes, reads int
}

func (d *reviewShareAuthorizationRecovery) ResumeAuthorizations(context.Context) error {
	d.resumes++
	d.resume()
	return nil
}

func (d *reviewShareAuthorizationRecovery) InspectShare(ctx context.Context, raw string) (cloud.ShareSnapshot, error) {
	return d.InspectShareDirectory(ctx, raw, "0")
}

func (d *reviewShareAuthorizationRecovery) InspectShareDirectory(context.Context, string, string) (cloud.ShareSnapshot, error) {
	d.reads++
	return cloud.ShareSnapshot{ShareCode: "fixture", Items: []cloud.ShareItem{{ID: "leaf", Name: "Show.S01E12.mkv", Size: 10}}}, nil
}

func TestPanSouReviewExplicitPreviewCanRecoverAuthorizationButStillRequiresProof(t *testing.T) {
	for _, proof := range []bool{true, false} {
		name := "saved-proof"
		if !proof {
			name = "missing-proof"
		}
		t.Run(name, func(t *testing.T) {
			s, _, actor, _, _, downloaders := siteFixture(t)
			receiver, _, connection := seedPanSouReceiver(t, s, models.ConnectionProviderQuark)
			connections := downloaders.connections
			saveQuarkTestBundle(t, connections, connection.ID, func(bundle *quark.Credentials) {
				bundle.Agent.IdentityVerified = false
				if !proof {
					bundle.Proofs = nil
				}
			})
			_, native, err := connections.cachedDriver(connection.ID)
			if err != nil {
				t.Fatal(err)
			}
			driver := &reviewShareAuthorizationRecovery{ShareBrowseDriver: native.(cloud.ShareBrowseDriver), resume: func() {
				saveQuarkTestBundle(t, connections, connection.ID, func(bundle *quark.Credentials) { bundle.Agent.IdentityVerified = true })
			}}
			connections.drivers[connection.ID] = driver
			if summary := downloaders.summary(actor, receiver); summary.Capabilities.ShareReceive || driver.resumes != 0 || driver.reads != 0 {
				t.Fatal("summary resumed pending authorization or advertised unverified identity")
			}
			claim := siteResultClaim{ActorID: actor.User.ID, SourceKind: RouteSourceQuarkShare, CloudProvider: "quark", TorrentID: "https://pan.quark.cn/s/fixture"}
			entries, _, _, err := s.inspectResultShare(context.Background(), actor, claim, claim.TorrentID, receiver.ID, true, RequestContext{})
			if driver.resumes != 1 {
				t.Fatalf("explicit preview did not attempt saved authorization recovery: %d", driver.resumes)
			}
			if proof {
				if err != nil || len(entries) != 1 || driver.reads != 1 {
					t.Fatalf("saved proof could not recover: entries=%d reads=%d err=%v", len(entries), driver.reads, err)
				}
			} else if err == nil || driver.reads != 0 {
				t.Fatalf("authorization recovery bypassed root proof: reads=%d err=%v", driver.reads, err)
			}
		})
	}
}

func TestFollowReviewLastCandidatePauseCannotBecomeNoMatch(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "pause"
		if resume {
			name = "pause-and-resume"
		}
		t.Run(name, func(t *testing.T) {
			f, driver := newFollowPreviewFixture(t)
			adapter := f.worker.sites.adapters["pansou_tg"].(*followSharesAdapter)
			adapter.items = adapter.items[:1]
			driver.onRead = func() {
				driver.onRead = nil
				if _, err := f.follows.SetPaused(f.actor, f.subscription.ID, true, RequestContext{}); err != nil {
					t.Fatal(err)
				}
				if resume {
					if _, err := f.follows.SetPaused(f.actor, f.subscription.ID, false, RequestContext{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			run := f.run(t)
			var tasks int64
			if err := f.follows.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ?", f.subscription.ID).Count(&tasks).Error; err != nil {
				t.Fatal(err)
			}
			if run.Status != models.FollowRunCancelled || run.ErrorCode != "follow_inactive" || tasks != 0 {
				t.Fatalf("last candidate lost lifecycle stop: status=%s code=%s tasks=%d", run.Status, run.ErrorCode, tasks)
			}
			var subscription models.FollowSubscription
			if err := f.follows.db.First(&subscription, "id = ?", f.subscription.ID).Error; err != nil {
				t.Fatal(err)
			}
			want := models.FollowStatusPaused
			if resume {
				want = models.FollowStatusActive
			}
			if subscription.Status != want {
				t.Fatalf("stopped run changed current subscription: %s", subscription.Status)
			}
		})
	}
}
