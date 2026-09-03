package service

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	flashbackRunLockWaitUnlimited  = 24 * time.Hour
	gaFlashbackRunLockWaitSec      = "flashback_run_lock_wait_sec"
	flashbackDefaultRunLockWaitSec = 7200 // 2h
)

// flashbackLocalRunLock Redis 不可用时的进程内兜底：全进程同时只跑 1 个闪回任务。
var flashbackLocalRunLock sync.Mutex

func flashbackRunLockWait(ctx context.Context) time.Duration {
	sec := getGlobalArgIntDefault(ctx, gaFlashbackRunLockWaitSec, flashbackDefaultRunLockWaitSec)
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

func flashbackLockWaitBounded(ctx context.Context, wait time.Duration) time.Duration {
	if wait <= 0 {
		wait = flashbackRunLockWaitUnlimited
	}
	if ctx == nil {
		return wait
	}
	if dl, ok := ctx.Deadline(); ok {
		remain := time.Until(dl)
		if remain <= 0 {
			return time.Millisecond
		}
		if remain < wait {
			return remain
		}
	}
	return wait
}

// flashbackAcquireRunLock 进程内互斥：同时只跑 1 个闪回任务。
// wait<=0 表示不限制（内部按 24h 自旋上限）。
func flashbackAcquireRunLock(ctx context.Context, wait time.Duration) (func(), error) {
	if wait <= 0 {
		flashbackLocalRunLock.Lock()
		return func() { flashbackLocalRunLock.Unlock() }, nil
	}
	deadline := time.Now().Add(wait)
	for {
		if flashbackLocalRunLock.TryLock() {
			return func() { flashbackLocalRunLock.Unlock() }, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("等待闪回执行锁超时")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
