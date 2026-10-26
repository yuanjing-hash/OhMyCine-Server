package pan115

import (
	"context"
	"testing"
	"time"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"golang.org/x/time/rate"
)

type quotaSDK struct {
	bulkSDK
	info          pan115sdk.InfoData
	infoCalls     int
	identityCalls int
}

func (s *quotaSDK) CookieCheck() error { s.identityCalls++; return nil }
func (s *quotaSDK) GetUser() (*pan115sdk.UserInfo, error) {
	s.identityCalls++
	return &pan115sdk.UserInfo{}, nil
}

func (s *quotaSDK) GetInfo() (pan115sdk.InfoData, error) { s.infoCalls++; return s.info, nil }
func TestQuotaReadsAccountSpaceWithoutDirectoryOrIdentityProbe(t *testing.T) {
	sdk := &quotaSDK{}
	sdk.info.SpaceInfo.AllUse.Size, sdk.info.SpaceInfo.AllTotal.Size = 12, 90
	c := &Client{sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), callSlots: make(chan struct{}, maxInFlightCalls), now: time.Now, jitter: func() time.Duration { return 0 }}
	quota, err := c.Quota(context.Background())
	if err != nil || quota.UsedBytes != 12 || quota.TotalBytes != 90 || sdk.infoCalls != 1 || sdk.listPageCalls != 0 || sdk.identityCalls != 0 {
		t.Fatalf("quota=%+v %v", quota, err)
	}
	sdk.info.SpaceInfo.AllUse.Size = 91
	if _, err := c.Quota(context.Background()); err == nil {
		t.Fatal("invalid capacity accepted")
	} else if code, _ := cloud.ErrorInfo(err); code != cloud.CodeResponseInvalid {
		t.Fatalf("invalid code=%v", err)
	}
}
