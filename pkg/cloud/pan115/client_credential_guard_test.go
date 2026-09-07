package pan115

import (
	"context"
	"testing"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestConfirmedAuthenticationFailureStopsOtherOperations(t *testing.T) {
	sdk := &mutationTestSDK{bulkSDK: &bulkSDK{}, err: pan115sdk.ErrNotLogin}
	client := newMutationTestClient(sdk)
	var outcome string
	client.onCallResult = func(err error) { outcome, _ = cloud.ErrorInfo(err) }
	if code, _ := cloud.ErrorInfo(client.Move(context.Background(), "one", "target")); code != cloud.CodeAuthExpired || outcome != cloud.CodeAuthExpired {
		t.Fatal("missing authentication outcome")
	}
	sdk.err = nil
	if code, _ := cloud.ErrorInfo(client.Recycle(context.Background(), "two")); code != cloud.CodeAuthExpired || len(sdk.deleted) != 0 {
		t.Fatal("expired driver continued unrelated mutation")
	}
	// A late successful old request cannot reactivate an invalid credential.
	client.recordOutcome(nil)
	if code, _ := cloud.ErrorInfo(client.Rename(context.Background(), "three", "new.mkv")); code != cloud.CodeAuthExpired || sdk.renameID != "" {
		t.Fatal("late success cleared authentication failure")
	}
}

func TestConnectionGuardStopsIOAndPreservesErrorClassification(t *testing.T) {
	sdk := &mutationTestSDK{bulkSDK: &bulkSDK{}}
	client := newMutationTestClient(sdk)
	checks := 0
	client.beforeCall = func(context.Context) error {
		checks++
		// Failure appeared while waiting for admission.
		if checks == 2 {
			return cloud.Error(cloud.CodeAuthExpired, false, nil)
		}
		return nil
	}
	if code, _ := cloud.ErrorInfo(client.Move(context.Background(), "one", "target")); code != cloud.CodeAuthExpired || len(sdk.moveItems) != 0 {
		t.Fatal("guard allowed I/O or lost auth code")
	}
	if len(client.callSlots) != 0 {
		t.Fatal("guard leaked provider slot")
	}
}

func TestRiskControlDoesNotInvalidateCredential(t *testing.T) {
	client := newMutationTestClient(&mutationTestSDK{bulkSDK: &bulkSDK{}})
	client.recordOutcome(cloud.Error(cloud.CodeRateLimited, true, nil))
	if client.authExpired {
		t.Fatal("risk response was treated as expired Cookie")
	}
}
