package hostapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

func TestHostMethodObserverCoversUncredentialedHTTPWithoutPrivateFacts(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"api.example.test"}}})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithResolver(publicResolver), WithHTTPClient(client))
	var methods []string
	ctx := WithHTTPMethodObserver(context.Background(), func(method string) { methods = append(methods, method) })
	for _, method := range []string{" post ", "get"} {
		input, _ := json.Marshal(httpRequest{Method: method, URL: "https://api.example.test/private-path?token=private"})
		if _, err := host.Call(ctx, fixture.pluginID, OperationHTTP, input); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(methods, []string{"POST", "GET"}) {
		t.Fatalf("method-only observation=%v", methods)
	}
	// A guest cannot install, replace or forge a method observer in its body.
	input := []byte(`{"method":"GET","url":"https://api.example.test/","observer":"POST"}`)
	if _, err := host.Call(ctx, fixture.pluginID, OperationHTTP, input); err == nil {
		t.Fatal("guest observer field accepted")
	}
	if len(methods) != 2 {
		t.Fatal("invalid guest payload changed observer")
	}
}
