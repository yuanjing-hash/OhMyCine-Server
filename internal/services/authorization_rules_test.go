package services

import (
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestAuthorizationResolveAppliesUserAllowDenyAndResourceScope(t *testing.T) {
	queue, _, _ := queueFixture(t)
	var viewer models.Role
	if err := queue.db.Where("code = ?", authz.RoleViewer).First(&viewer).Error; err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: "rule-user", UsernameNormalized: "rule-user", DisplayName: "Rule User", PasswordHash: "unused", Status: models.UserStatusActive, AuthzVersion: 1}
	if err := queue.db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Create(&models.UserRole{UserID: user.ID, RoleID: viewer.ID, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	rules := []models.UserAuthorizationRule{
		{UserID: user.ID, PermissionCode: authz.PermissionDiscoveryRead, Effect: models.AuthorizationEffectAllow, CreatedBy: user.ID},
		{UserID: user.ID, PermissionCode: authz.PermissionDashboardRead, Effect: models.AuthorizationEffectDeny, CreatedBy: user.ID},
		{UserID: user.ID, PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectAllow, CreatedBy: user.ID},
		{UserID: user.ID, PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectAllow, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: "9", CreatedBy: user.ID},
		{UserID: user.ID, PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: "10", CreatedBy: user.ID},
	}
	for i := range rules {
		if err := queue.db.Create(&rules[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	actor, err := NewAuthorizationService(queue.db).Resolve(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !actor.Can(authz.PermissionDiscoveryRead) {
		t.Fatal("direct global allow was not applied")
	}
	if actor.Can(authz.PermissionDashboardRead) {
		t.Fatal("direct global deny did not override role")
	}
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, "9") {
		t.Fatal("scoped allow was not applied")
	}
	if actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, "10") {
		t.Fatal("scoped deny did not override role")
	}
}

func TestPreviewScheduleRequiresFiveFieldsAndHonorsTimezone(t *testing.T) {
	if _, err := PreviewSchedule("0 0 1 1 * 2027", "Asia/Shanghai", 3, time.Now()); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("six-field cron accepted: %v", err)
	}
	items, err := PreviewSchedule("0 3 * * *", "Asia/Shanghai", 3, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d preview times", len(items))
	}
	location, _ := time.LoadLocation("Asia/Shanghai")
	for _, item := range items {
		if item.In(location).Hour() != 3 || item.In(location).Minute() != 0 {
			t.Fatalf("preview ignored timezone: %s", item)
		}
	}
}
