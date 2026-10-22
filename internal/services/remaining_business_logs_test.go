package services

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func captureRemainingBusiness(t *testing.T, module string) (*serverlog.Manager, zerolog.Logger) {
	t.Helper()
	manager, err := serverlog.NewManager(t.TempDir(), "production", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, manager.Logger(module, "business-test")
}

func remainingBusinessEntries(t *testing.T, manager *serverlog.Manager) []serverlog.Entry {
	t.Helper()
	page, err := manager.Query(context.Background(), serverlog.Filter{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Second), BusinessOnly: true, Direction: "asc", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return page.List
}

func requireRemainingBusinessEvent(t *testing.T, entries []serverlog.Entry, code, outcome string) serverlog.Entry {
	t.Helper()
	for _, entry := range entries {
		if entry.Fields["event_code"] == code && entry.Fields["outcome"] == outcome {
			return entry
		}
	}
	t.Fatalf("missing actual business event %s/%s: %+v", code, outcome, entries)
	return serverlog.Entry{}
}

func TestAuthenticationBusinessLogsNeverContainSubmittedSecrets(t *testing.T) {
	service, db, _, session, now := authSessionContentionFixture(t, time.Date(2026, 9, 6, 9, 30, 0, 0, time.UTC))
	manager, logger := captureRemainingBusiness(t, "authentication")
	service.log = logger
	password := "Fixture-Secret-987"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.User{}).Where("id = ?", session.UserID).Update("password_hash", hash).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	actor, token, err := service.Login("session-owner", password, "private-agent", RequestContext{IPHint: "private-ip", RequestID: "auth-request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Login("session-owner", "bad-secret-password", "", RequestContext{IPHint: "private-ip"}); ErrorCode(err) != CodeInvalidCredentials {
		t.Fatalf("bad login=%v", err)
	}
	for i := 0; i < 5; i++ {
		service.limiter.Failure("private-ip|session-owner")
	}
	if _, _, err := service.Login("session-owner", password, "", RequestContext{IPHint: "private-ip"}); ErrorCode(err) != CodeLoginRateLimited {
		t.Fatalf("limited login=%v", err)
	}
	if err := service.Logout(token, actor, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	entries := remainingBusinessEntries(t, manager)
	requireRemainingBusinessEvent(t, entries, "auth.login.confirmed", "confirmed")
	requireRemainingBusinessEvent(t, entries, "auth.login.rejected", "failed")
	requireRemainingBusinessEvent(t, entries, "auth.login.limited", "waiting")
	requireRemainingBusinessEvent(t, entries, "auth.logout.confirmed", "confirmed")
	data, _ := json.Marshal(entries)
	for _, secret := range []string{password, "bad-secret-password", token, hash, "private-agent", "private-ip", "session-owner"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("auth log leaked %q: %s", secret, data)
		}
	}
}
