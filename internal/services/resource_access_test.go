package services

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestResourceAccessModesAndAuthorityExpansion(t *testing.T) {
	base := rolePermissionActor([]string{authz.PermissionDownloadsCreate, authz.PermissionMediaLibrariesRead})
	for _, item := range []struct {
		mode             string
		ids              []string
		existing, future bool
	}{
		{models.ResourceAccessModeAll, nil, true, true},
		{models.ResourceAccessModeAllowlist, nil, false, false},
		{models.ResourceAccessModeAllowlist, []string{"one"}, true, false},
		{models.ResourceAccessModeDenylist, nil, true, true},
		{models.ResourceAccessModeDenylist, []string{"one"}, false, true},
		{"unknown", nil, false, false},
	} {
		actor := base
		actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Scope: models.ResourceAccessScopeDownloaderUse, Mode: item.mode, ResourceIDs: item.ids}}
		if actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, "one") != item.existing ||
			actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, "future") != item.future {
			t.Fatalf("mode %s %v did not apply to current/new resources", item.mode, item.ids)
		}
	}
	denied := base
	denied.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionDownloadsCreate, Effect: models.AuthorizationEffectAllow, ResourceType: models.AuthorizationResourceDownloader, ResourceID: "one"}, {PermissionCode: authz.PermissionDownloadsCreate, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceDownloader, ResourceID: "one"}}
	if denied.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, "one") {
		t.Fatal("legacy deny was overridden")
	}
	withoutFunction := Actor{ResourceAccessPolicies: map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Mode: models.ResourceAccessModeAll}}}
	if withoutFunction.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, "one") {
		t.Fatal("resource policy granted a function")
	}
	limited := base
	limited.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{"one"}}}
	before := base
	before.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{"one"}}}
	if err := validateAuthorityExpansion(limited, before, base); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("opening future resources passed a finite allowlist: %v", err)
	}
	if err := validateAuthorityExpansion(limited, base, before); err != nil {
		t.Fatalf("narrowing a broader user was rejected: %v", err)
	}
	managementCodes := []string{authz.PermissionUsersRead, authz.PermissionLogsRead, authz.PermissionDownloadersCreate, authz.PermissionSitesCreate, authz.PermissionMediaLibrariesCreate}
	manager := rolePermissionActor(managementCodes)
	manager.ResourceAccessPolicies = map[string]ResourceAccessPolicy{
		models.ResourceAccessScopeDownloaderUse: {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{}},
		models.ResourceAccessScopeSiteSearch:    {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{}},
		models.ResourceAccessScopeLibraryRead:   {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{}},
	}
	if err := validateAuthorityExpansion(manager, Actor{}, rolePermissionActor(managementCodes)); err != nil {
		t.Fatalf("unrelated global function blocked by resource lists: %v", err)
	}
	explicitLegacy := Actor{ResourceRules: []AuthorizationRule{{PermissionCode: authz.PermissionUsersRead, Effect: models.AuthorizationEffectAllow, ResourceType: models.AuthorizationResourceSite, ResourceID: "1"}}}
	if err := validateAuthorityExpansion(manager, Actor{}, explicitLegacy); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("explicit legacy resource pair escaped its grant boundary: %v", err)
	}
	noRead := rolePermissionActor([]string{authz.PermissionDownloadsCreate})
	if noRead.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceMediaLibrary, "7") || noRead.CanIngestLibrary("7") {
		t.Fatal("invisible library accepted as an ingest target")
	}
	before.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryIngest: {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{}}}
	if err := validateAuthorityExpansion(noRead, before, base); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("admin without library-read authority granted ingestion: %v", err)
	}
	repair := rolePermissionActor([]string{authz.PermissionMediaLibrariesRead, authz.PermissionJobsControlOwn, authz.PermissionTransfersReadOwn})
	repairBefore := repair
	repairBefore.ResourceAccessPolicies = before.ResourceAccessPolicies
	repairLimited := repair
	repairLimited.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryIngest: {Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{"7"}}}
	if !repairLimited.libraryIngestAuthority().allows("7") || repairLimited.libraryIngestAuthority().allows("999") {
		t.Fatal("repair-only ingestion authority is incorrect")
	}
	if err := validateAuthorityExpansion(repairLimited, repairBefore, repair); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("repair-only widening bypassed its ingest list: %v", err)
	}
	aliased := base
	aliased.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeLibraryRead: {Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{"009"}}}
	if aliased.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, "9") || aliased.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, "09") {
		t.Fatal("numeric alias bypassed library denylist")
	}
	aliased.ResourceAccessPolicies = nil
	aliased.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: "009"}}
	if aliased.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, "9") {
		t.Fatal("numeric alias bypassed old resource deny")
	}
	normalized, err := normalizeResourceAccessPolicy(ResourceAccessPolicy{Scope: models.ResourceAccessScopeSiteSearch, Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{"009", "9"}})
	if err != nil || len(normalized.ResourceIDs) != 1 || normalized.ResourceIDs[0] != "9" {
		t.Fatalf("numeric list not normalized: %+v %v", normalized, err)
	}
}

func TestUserResourceAccessTransactionsAndLegacyGrantBoundaries(t *testing.T) {
	queue, initial, _ := queueFixture(t)
	db := queue.db
	var administrator, viewer models.Role
	if err := db.Where("code = ?", authz.RoleAdministrator).First(&administrator).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("code = ?", authz.RoleViewer).First(&viewer).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.User{}).Where("id = ?", initial.User.ID).Update("is_owner", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.UserRole{UserID: initial.User.ID, RoleID: administrator.ID}).Error; err != nil {
		t.Fatal(err)
	}
	authorizer := NewAuthorizationService(db)
	owner, err := authorizer.Resolve(initial.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	admin := NewAdminService(db, authorizer, nil, NewAuditService(db))
	create := func(name string) Actor {
		t.Helper()
		user := models.User{Username: name, UsernameNormalized: name, DisplayName: name, Status: models.UserStatusActive, PasswordHash: "unused", AuthzVersion: 1}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&models.UserRole{UserID: user.ID, RoleID: administrator.ID}).Error; err != nil {
			t.Fatal(err)
		}
		actor, err := authorizer.Resolve(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		return actor
	}
	target, limited := create("policy-target"), create("limited-admin")
	first := models.Downloader{ID: uuid.NewString(), Name: "Visible downloader", NameNormalized: "visible", Type: models.DownloaderTypeQBittorrent, Enabled: true, CapabilitiesJSON: `{}`, BaseURL: "http://private-downloader", PasswordCiphertext: "secret-ciphertext"}
	second := first
	second.ID, second.Name, second.NameNormalized = uuid.NewString(), "Hidden downloader", "hidden"
	for _, record := range []*models.Downloader{&first, &second} {
		if err := db.Create(record).Error; err != nil {
			t.Fatal(err)
		}
	}
	get := func(userID uint) UserResourceAccess {
		t.Helper()
		value, err := admin.UserResourceAccess(owner, userID)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	set := func(value UserResourceAccess, scope, mode string, ids ...string) UserResourceAccess {
		for i := range value.Policies {
			if value.Policies[i].Scope == scope {
				value.Policies[i].Mode, value.Policies[i].ResourceIDs = mode, ids
			}
		}
		return value
	}
	save := func(by Actor, userID uint, value UserResourceAccess) UserResourceAccess {
		t.Helper()
		result, err := admin.ReplaceUserResourceAccess(by, userID, value, RequestContext{})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	defaults := get(target.User.ID)
	if defaults.Revision != 1 || len(defaults.Policies) != 4 {
		t.Fatalf("defaults: %+v", defaults)
	}
	for _, policy := range defaults.Policies {
		if policy.Mode != models.ResourceAccessModeAll || policy.ResourceIDs == nil {
			t.Fatalf("default policy: %+v", policy)
		}
	}
	value := save(owner, target.User.ID, set(defaults, models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAllowlist, first.ID, first.ID))
	if value.Revision != 2 || len(value.Policies[0].ResourceIDs) != 1 {
		t.Fatalf("save not normalized/versioned: %+v", value)
	}
	fresh, err := authorizer.Resolve(target.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, first.ID) || fresh.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, second.ID) {
		t.Fatal("next request did not apply policy")
	}
	if _, err := admin.ReplaceUserResourceAccess(owner, target.User.ID, defaults, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale save: %v", err)
	}
	invalid := set(get(target.User.ID), models.ResourceAccessScopeSiteSearch, models.ResourceAccessModeAllowlist, "999999")
	if _, err := admin.ReplaceUserResourceAccess(owner, target.User.ID, invalid, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("missing resource: %v", err)
	}
	if get(target.User.ID).Revision != 2 {
		t.Fatal("rejected transaction advanced revision")
	}
	// Two clients saving one revision must not silently overwrite each other.
	concurrent := get(target.User.ID)
	results := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := admin.ReplaceUserResourceAccess(owner, target.User.ID, concurrent, RequestContext{})
			results <- err
		}()
	}
	group.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if ErrorCode(err) == CodeConflict {
			conflicts++
		} else {
			t.Fatalf("concurrent save: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent results success=%d conflict=%d", successes, conflicts)
	}
	if _, err := admin.ReplaceUserResourceAccess(owner, owner.User.ID, get(owner.User.ID), RequestContext{}); ErrorCode(err) != CodeSelfModification {
		t.Fatalf("self edit: %v", err)
	}
	if _, err := admin.ReplaceUserResourceAccess(limited, owner.User.ID, get(owner.User.ID), RequestContext{}); ErrorCode(err) != CodeOwnerProtected {
		t.Fatalf("owner edit: %v", err)
	}
	save(owner, limited.User.ID, set(get(limited.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAllowlist, first.ID))
	if _, err := admin.ReplaceUserResourceAccess(limited, target.User.ID, set(get(target.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAll), RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("future expansion: %v", err)
	}
	if _, err := admin.ReplaceUserResourceAccess(limited, target.User.ID, set(get(target.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAllowlist, second.ID), RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("existing expansion: %v", err)
	}
	save(owner, target.User.ID, set(get(target.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAll))
	save(limited, target.User.ID, set(get(target.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAllowlist, first.ID))

	// Removal of an old deny is itself a grant and cannot bypass the new lists.
	save(owner, target.User.ID, set(get(target.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAll))
	deny := AuthorizationRule{PermissionCode: authz.PermissionDownloadsCreate, Effect: models.AuthorizationEffectDeny}
	if err := admin.ReplaceUserAuthorizationRules(owner, target.User.ID, ReplaceUserAuthorizationRulesInput{Rules: []AuthorizationRule{deny}}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := admin.ReplaceUserAuthorizationRules(limited, target.User.ID, ReplaceUserAuthorizationRulesInput{}, RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("legacy global rule bypass: %v", err)
	}
	if err := admin.ReplaceUserRoles(limited, target.User.ID, []uint{viewer.ID}, RequestContext{}); err != nil {
		t.Fatalf("role narrowing: %v", err)
	}
	if err := admin.ReplaceUserRoles(limited, target.User.ID, []uint{administrator.ID}, RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("role assignment bypass: %v", err)
	}
	if _, err := admin.CreateUser(limited, CreateUserInput{Username: "must-rollback", Password: "a-strong-test-password", RoleIDs: []uint{administrator.ID}}, RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("new user bypass: %v", err)
	}
	var count int64
	if err := db.Model(&models.User{}).Where("username = ?", "must-rollback").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rejected create persisted user: %d %v", count, err)
	}
	if _, err := admin.CreateRole(limited, CreateRoleInput{Code: "limited-template", Name: "Template", PermissionCodes: []string{authz.PermissionDownloadersRead}}, RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("role template bypass: %v", err)
	}
	managementRole, err := admin.CreateRole(limited, CreateRoleInput{Code: "safe-management", Name: "Management", PermissionCodes: []string{authz.PermissionUsersRead, authz.PermissionLogsRead, authz.PermissionDownloadersCreate}}, RequestContext{})
	if err != nil {
		t.Fatalf("unrelated role functions blocked by downloader list: %v", err)
	}
	if _, err := admin.CreateUser(limited, CreateUserInput{Username: "management-user", Password: "a-strong-test-password", RoleIDs: []uint{managementRole.ID}}, RequestContext{}); err != nil {
		t.Fatalf("unrelated user functions blocked by downloader list: %v", err)
	}
	custom, err := admin.CreateRole(owner, CreateRoleInput{Code: "resource-role", Name: "Resource Role", PermissionCodes: []string{authz.PermissionMediaLibrariesRead}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.UserRole{UserID: target.User.ID, RoleID: custom.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.ReplaceRolePermissions(limited, custom.ID, []string{authz.PermissionMediaLibrariesRead, authz.PermissionDownloadsCreate}, RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("role permission bypass: %v", err)
	}
	previousRevision := get(target.User.ID).Revision
	if err := admin.ReplaceRolePermissions(limited, custom.ID, nil, RequestContext{}); err != nil {
		t.Fatalf("template narrowing: %v", err)
	}
	if get(target.User.ID).Revision != previousRevision+1 {
		t.Fatal("role permissions did not invalidate policy revision")
	}
	if err := admin.ReplaceRolePermissions(owner, custom.ID, []string{authz.PermissionDownloadersRead}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	inactive := false
	if _, err := admin.UpdateRole(owner, custom.ID, UpdateRoleInput{Active: &inactive}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	active := true
	if _, err := admin.UpdateRole(limited, custom.ID, UpdateRoleInput{Active: &active}, RequestContext{}); ErrorCode(err) != CodePrivilegeEscalation {
		t.Fatalf("role activation bypass: %v", err)
	}

	// A referenced deleted ID remains removable without being silently lost.
	save(owner, target.User.ID, set(get(target.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAllowlist, first.ID))
	if err := db.Delete(&first).Error; err != nil {
		t.Fatal(err)
	}
	legacyDeleted := models.UserAuthorizationRule{UserID: target.User.ID, PermissionCode: authz.PermissionDiscoveryRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceSite, ResourceID: "00777", CreatedBy: owner.User.ID}
	if err := db.Create(&legacyDeleted).Error; err != nil {
		t.Fatal(err)
	}
	options, err := admin.UserResourceAccessOptions(limited, target.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(options)
	if strings.Contains(string(encoded), "Hidden downloader") || strings.Contains(string(encoded), "private-downloader") || strings.Contains(string(encoded), "secret-ciphertext") {
		t.Fatalf("unsafe options: %s", encoded)
	}
	if len(options.Scopes[0].Options) != 1 || !options.Scopes[0].Options[0].Deleted {
		t.Fatalf("deleted reference not retained: %+v", options)
	}
	if len(options.Scopes[1].Options) != 1 || options.Scopes[1].Options[0].ID != "777" || !options.Scopes[1].Options[0].Deleted {
		t.Fatalf("old deleted direct-rule reference missing: %+v", options.Scopes[1])
	}
	save(owner, target.User.ID, get(target.User.ID))
	if err := db.Model(&models.UserResourceAccessPolicy{}).Where("user_id = ? AND scope = ?", target.User.ID, models.ResourceAccessScopeDownloaderUse).Update("resource_ids_json", `{"wrong":true}`).Error; err != nil {
		t.Fatal(err)
	}
	fresh, err = authorizer.Resolve(target.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ResourceAccessAllows(models.ResourceAccessScopeDownloaderUse, second.ID) || !fresh.ResourceAccessAllows(models.ResourceAccessScopeLibraryRead, "999") {
		t.Fatal("corrupt scope was opened or unrelated scope was denied")
	}
	options, err = admin.UserResourceAccessOptions(owner, target.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(options.Scopes[0].Options[0].DenialReason, "格式异常") {
		t.Fatalf("corrupt policy not explained: %+v", options.Scopes[0].Options)
	}
	save(owner, target.User.ID, set(get(target.User.ID), models.ResourceAccessScopeDownloaderUse, models.ResourceAccessModeAll))
	if err := db.Model(&models.User{}).Where("id = ?", target.User.ID).Update("status", models.UserStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	save(owner, target.User.ID, get(target.User.ID))
	if err := admin.DeleteUser(owner, target.User.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.UserResourceAccessPolicy{}).Where("user_id = ?", target.User.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("deleted user retains policy: %d %v", count, err)
	}
}
