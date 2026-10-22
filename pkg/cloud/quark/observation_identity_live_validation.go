//go:build quark_live

package quark

// ObservationTVIdentity validates an existing opaque identity without converting
// it to another namespace or exposing its raw handle to acceptance assemblers.
func ObservationTVIdentity(id, account string) bool {
	_, err := parseID(id, RouteTV, account, false)
	return err == nil
}
