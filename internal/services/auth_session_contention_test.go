package services

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/config"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func authSessionContentionFixture(t *testing.T, lastSeen time.Time) (*AuthService, *gorm.DB, string, models.Session, time.Time) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "auth-contention.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	user := models.User{Username: "session-owner", UsernameNormalized: "session-owner", DisplayName: "Session Owner", PasswordHash: "x", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	token := "session-contention-token"
	session := models.Session{ID: "session-contention", TokenHash: tokenHash(token), UserID: user.ID, CreatedAt: now.Add(-time.Hour), LastSeenAt: lastSeen, IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{SessionIdleTTL: 2 * time.Hour, SessionMaxTTL: 7 * 24 * time.Hour}
	auth, err := NewAuthService(db, cfg, NewAuthorizationService(db), NewAuditService(db), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	auth.now = func() time.Time { return now }
	return auth, db, token, session, now
}

func holdSQLiteWriter(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if err := tx.Exec("UPDATE users SET updated_at = updated_at WHERE id = 1").Error; err != nil {
		_ = tx.Rollback().Error
		t.Fatal(err)
	}
	return tx
}

func TestAuthenticateSessionWithinTouchWindowDoesNotNeedWriter(t *testing.T) {
	auth, db, token, session, now := authSessionContentionFixture(t, time.Date(2026, 9, 6, 9, 59, 0, 0, time.UTC))
	writer := holdSQLiteWriter(t, db)
	defer func() { _ = writer.Rollback().Error }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	actor, authenticated, err := auth.AuthenticateContext(ctx, token)
	if err != nil || actor.User.ID != session.UserID || authenticated.ID != session.ID {
		t.Fatalf("authentication actor=%+v session=%+v err=%v", actor, authenticated, err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("read-only authentication waited for writer: %s", elapsed)
	}
	var persisted models.Session
	if err := db.First(&persisted, "id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !persisted.LastSeenAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("last_seen_at changed inside touch window: %s", persisted.LastSeenAt)
	}
}

func TestAuthenticateSessionDefersBusyTouchAfterValidation(t *testing.T) {
	auth, db, token, session, now := authSessionContentionFixture(t, time.Date(2026, 9, 6, 9, 50, 0, 0, time.UTC))
	writer := holdSQLiteWriter(t, db)
	started := time.Now()
	actor, authenticated, err := auth.AuthenticateContext(context.Background(), token)
	if err != nil || actor.User.ID != session.UserID || authenticated.ID != session.ID {
		_ = writer.Rollback().Error
		t.Fatalf("validated session was rejected by touch contention: actor=%+v session=%+v err=%v", actor, authenticated, err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		_ = writer.Rollback().Error
		t.Fatalf("busy touch blocked the request for %s", elapsed)
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var refreshed models.Session
	for {
		if err := db.First(&refreshed, "id = ?", session.ID).Error; err != nil {
			t.Fatal(err)
		}
		if refreshed.LastSeenAt.Equal(now) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !refreshed.LastSeenAt.Equal(now) || !refreshed.IdleExpiresAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("deferred touch did not recover: %+v", refreshed)
	}
}

func TestDeferredSessionTouchDoesNotReactivateRevokedSession(t *testing.T) {
	auth, db, token, session, now := authSessionContentionFixture(t, time.Date(2026, 9, 6, 9, 50, 0, 0, time.UTC))
	writer := holdSQLiteWriter(t, db)
	if _, _, err := auth.AuthenticateContext(context.Background(), token); err != nil {
		_ = writer.Rollback().Error
		t.Fatal(err)
	}
	revokedAt := now.Add(time.Second)
	if err := writer.Model(&models.Session{}).Where("id = ?", session.ID).Update("revoked_at", revokedAt).Error; err != nil {
		_ = writer.Rollback().Error
		t.Fatal(err)
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(auth.touchSlot) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(auth.touchSlot) != 0 {
		t.Fatal("deferred touch did not finish")
	}
	var persisted models.Session
	if err := db.First(&persisted, "id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.RevokedAt == nil || !persisted.RevokedAt.Equal(revokedAt) {
		t.Fatalf("revocation was not preserved: %+v", persisted)
	}
	if !persisted.LastSeenAt.Equal(session.LastSeenAt) {
		t.Fatalf("deferred touch changed revoked session activity: before=%s after=%s", session.LastSeenAt, persisted.LastSeenAt)
	}
}

func TestAuthenticateSessionStillRejectsExpiredSession(t *testing.T) {
	auth, db, token, session, now := authSessionContentionFixture(t, time.Date(2026, 9, 6, 9, 59, 0, 0, time.UTC))
	if err := db.Model(&models.Session{}).Where("id = ?", session.ID).Update("idle_expires_at", now.Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.AuthenticateContext(context.Background(), token); ErrorCode(err) != CodeNotAuthenticated {
		t.Fatalf("expired session err=%v code=%s", err, ErrorCode(err))
	}
}

func TestAuthenticateDeviceDoesNotWaitForActivityTouch(t *testing.T) {
	auth, db, _, session, now := authSessionContentionFixture(t, time.Date(2026, 9, 6, 9, 59, 0, 0, time.UTC))
	auth.config.DeviceTokenIdleTTL = 30 * 24 * time.Hour
	rawToken := deviceTokenPrefix + "device-contention-token"
	device := models.DeviceToken{ID: "device-contention", TokenHash: tokenHash(rawToken), UserID: session.UserID, DeviceIDHash: "device-hash", DeviceName: "测试设备", ClientKind: deviceClientKind, CreatedAt: now.Add(-time.Hour), LastSeenAt: now.Add(-10 * time.Minute), IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(60 * 24 * time.Hour)}
	if err := db.Create(&device).Error; err != nil {
		t.Fatal(err)
	}
	writer := holdSQLiteWriter(t, db)
	started := time.Now()
	actor, authenticated, err := auth.AuthenticateDevice(rawToken)
	if err != nil || actor.User.ID != session.UserID || authenticated.ID != device.ID {
		_ = writer.Rollback().Error
		t.Fatalf("device authentication actor=%+v device=%+v err=%v", actor, authenticated, err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		_ = writer.Rollback().Error
		t.Fatalf("device activity touch blocked authentication for %s", elapsed)
	}
	if err := writer.Commit().Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := db.First(&device, "id = ?", device.ID).Error; err != nil {
			t.Fatal(err)
		}
		if device.LastSeenAt.Equal(now) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !device.LastSeenAt.Equal(now) {
		t.Fatalf("device activity touch did not recover: %+v", device)
	}
}
