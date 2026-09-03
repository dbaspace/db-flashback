package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlashbackLockWaitBounded(t *testing.T) {
	if got := flashbackLockWaitBounded(context.Background(), 0); got != flashbackRunLockWaitUnlimited {
		t.Fatalf("unlimited wait got %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := flashbackLockWaitBounded(ctx, time.Hour)
	if got > 50*time.Millisecond || got <= 0 {
		t.Fatalf("should cap to remaining deadline, got %s", got)
	}
}

func TestFlashbackAcquireRunLockSerial(t *testing.T) {
	if baseService != nil && baseService.redisLock != nil {
		t.Skip("redis lock configured; skipping local fallback test")
	}
	ctx := context.Background()
	var running int32
	var maxConcurrent int32
	var wg sync.WaitGroup
	for n := 0; n < 6; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := flashbackAcquireRunLock(ctx, 2*time.Second)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			cur := atomic.AddInt32(&running, 1)
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt32(&running, -1)
			rel()
		}()
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Fatalf("max concurrency = %d, want 1", maxConcurrent)
	}
}

func TestFlashbackAcquireRunLockTimeout(t *testing.T) {
	if baseService != nil && baseService.redisLock != nil {
		t.Skip("redis lock configured; skipping local fallback test")
	}
	held, err := flashbackAcquireRunLock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := flashbackAcquireRunLock(ctx, 80*time.Millisecond); err == nil {
		t.Fatal("expected timeout while lock held")
	}
}
