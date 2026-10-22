package services

import (
	"context"
	"errors"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type resumableQuarkDriver struct {
	*fakeCloudDriver
	resume func() error
}

func (d *resumableQuarkDriver) ResumeAuthorizations(context.Context) error { return d.resume() }

func TestQuarkDriverResumesOutsideCacheLockAndRechecksConfiguration(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy_tv_pending_agent", true: "disabled_during_confirmation"}[disabled], func(t *testing.T) {
			s, _, record := createQuarkConnection(t)
			calls := 0
			d := &resumableQuarkDriver{fakeCloudDriver: &fakeCloudDriver{}}
			d.resume = func() error {
				calls++
				// A real persister does this; holding s.mu around resume deadlocks.
				s.invalidate(record.ID)
				if disabled {
					if err := s.db.Model(&models.Connection{}).Where("id=?", record.ID).Update("enabled", false).Error; err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("agent identity temporarily unavailable")
			}
			if err := s.registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return d, nil }); err != nil {
				t.Fatal(err)
			}
			_, actual, err := s.driver(record.ID)
			if calls != 1 {
				t.Fatal("saved identity checkpoint not resumed before returning driver")
			}
			if disabled {
				if actual != nil || ErrorCode(err) != CodeConnectionUnavailable {
					t.Fatalf("disabled connection remained usable: %v", err)
				}
			} else if err != nil || actual != d {
				t.Fatalf("pending Agent incorrectly blocked verified TV: %v", err)
			}
		})
	}
}
