package pan115offline

import (
	"context"
	"sync"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func TestTaskOperationScopeIsImmutableAndPrivate(t *testing.T) {
	c := &Client{}
	if err := c.BindTaskOperation("original"); err != nil {
		t.Fatal(err)
	}
	var done sync.WaitGroup
	for i := 0; i < 12; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			if err := c.BindTaskOperation("original"); err != nil {
				t.Error(err)
			}
		}()
	}
	done.Wait()
	if err := c.BindTaskOperation("different"); err == nil {
		t.Fatal("adapter was rebound to another task")
	}
	ctx, err := c.taskOperationContext(context.Background(), "omc-original")
	if err != nil || cloud.OfflineOperationScope(ctx) != "download:original" {
		t.Fatal("task scope lost", err)
	}
	if _, err = c.taskOperationContext(context.Background(), "omc-different"); err == nil {
		t.Fatal("source tag changed task scope")
	}
}
