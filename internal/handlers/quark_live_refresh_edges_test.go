//go:build quark_live

package handlers_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuarkLiveTVCommittedCheckpointCanRenewExpiredAccess(t *testing.T) {
	f := newLiveTVFixture(t)
	s := f.open(t)
	// Model interruption after the verified main state was committed, but
	// before its identical pending pair was retired. Its access is now expired.
	pending := s.session
	pending.IdentityVerified = false
	if _, err := s.save(liveTVCheckpointName, liveTVPurpose+":pending:v1", pending); err != nil {
		t.Fatal(err)
	}
	_ = s.lock.Close()
	reloaded := f.open(t)
	if c, renewed, err := readyLiveTV(reloaded); err != nil || !renewed || c.TV == nil || c.TV.Generation != 8 || c.TV.RefreshToken != "" || f.brokerCalls.Load() != 1 {
		t.Fatalf("committed pair was misclassified as an expired unverified checkpoint: %v", err)
	}
}

func TestQuarkLiveTVFreshQRRetiresObsoleteAttempt(t *testing.T) {
	f := newLiveTVFixture(t)
	f.brokerFailure.Store(true)
	s := f.open(t)
	if _, _, err := readyLiveTV(s); err == nil || err.Error() != "tv_refresh_uncertain" {
		t.Fatal("fixture did not establish an ambiguous refresh")
	}
	// An explicit successful legacy QR login advances the verified generation
	// and replaces the token pair; it does not know this helper's marker name.
	fresh := s.session
	fresh.Generation++
	fresh.AccessToken, fresh.RefreshToken = "fixture-new-access", "fixture-new-refresh"
	if _, err := s.save(liveTVSessionName, liveTVPurpose, fresh); err != nil {
		t.Fatal(err)
	}
	_ = s.lock.Close()
	reloaded := f.open(t)
	if _, renewed, err := readyLiveTV(reloaded); err != nil || renewed || f.brokerCalls.Load() != 1 {
		t.Fatalf("fresh verified login was not reused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, liveTVAttemptName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete ambiguous attempt survived successful newer login verification")
	}
}

func TestQuarkLiveTVPendingStateFencesBeforeNetwork(t *testing.T) {
	for _, field := range []string{"refresh", "profile", "device", "account", "generation"} {
		t.Run(field, func(t *testing.T) {
			f := newLiveTVFixture(t)
			s := f.open(t)
			pending := s.session
			pending.IdentityVerified = false
			pending.Generation++
			pending.AccessToken, pending.RefreshToken = "fixture-new-access", "fixture-new-refresh"
			switch field {
			case "refresh":
				pending.RefreshToken = ""
			case "profile":
				pending.ProfileFingerprint = strings.Repeat("c", 64)
			case "device":
				pending.DeviceID = strings.Repeat("c", 32)
			case "account":
				pending.UserID = "fixture-other-user"
			case "generation":
				pending.Generation++
			}
			if _, err := s.save(liveTVCheckpointName, liveTVPurpose+":pending:v1", pending); err != nil {
				t.Fatal(err)
			}
			_ = s.lock.Close()
			reloaded, err := openLiveTVStore(f.dir)
			if err == nil {
				defer reloaded.lock.Close()
				_, _, err = readyLiveTV(reloaded)
			}
			if err == nil || f.brokerCalls.Load() != 0 || f.identityCalls.Load() != 0 {
				t.Fatal("invalid pending identity reached a cloud request")
			}
		})
	}
}
