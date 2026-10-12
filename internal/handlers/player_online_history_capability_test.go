package handlers

import (
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"testing"
)

func TestOnlineHistoryCapabilityRequiresProductionBridgeAndCurrentRead(t *testing.T) {
	actor := services.Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	plugins := services.NewPluginRepositoryService(nil, nil, nil, zerolog.Nop())
	history := services.NewPlayerHistoryService(nil)
	api := &API{playerHistory: history, pluginRepositories: plugins}
	contains := func(values []string) bool {
		for _, value := range values {
			if value == "online_playback_history_v1" {
				return true
			}
		}
		return false
	}
	if contains(api.playerCapabilitiesWithServices(actor)) {
		t.Fatal("unwired bridge was advertised")
	}
	history.SetPluginService(plugins)
	if !contains(api.playerCapabilitiesWithServices(actor)) {
		t.Fatal("wired bridge was not advertised")
	}
	delete(actor.Permissions, authz.PermissionMediaLibrariesRead)
	if contains(api.playerCapabilitiesWithServices(actor)) {
		t.Fatal("actor without media read was advertised online history")
	}
}
