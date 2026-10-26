package keylock

import (
	"context"
	"errors"
	"testing"
)

func TestCancelledWaitAndCleanup(t *testing.T) {
	var locks Map
	release, err := locks.Lock(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locks.Lock(ctx, "task"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	release()
	if len(locks.entries) != 0 {
		t.Fatalf("idle lock leaked: %+v", locks.entries)
	}
	release, err = locks.Lock(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
