package runtime

import (
	"testing"
	"time"
)

func TestBrowserLoginSequenceHasBoundedMultiRequestBudget(t *testing.T) {
	for _, operation := range []string{"resource.auth.login", "resource.auth.captcha"} {
		if got := operationTimeout(operation); got != 60*time.Second {
			t.Fatalf("%s timeout = %v", operation, got)
		}
	}
	for _, operation := range []string{"resource.search", "resource.health", "resource.resolve", "resource.auth.cookie"} {
		if got := operationTimeout(operation); got != resourceCallTimeout {
			t.Fatalf("%s unexpectedly enlarged: %v", operation, got)
		}
	}
	if operationTimeout("site.search") != defaultCallTimeout {
		t.Fatal("unrelated plugin budget changed")
	}
}
