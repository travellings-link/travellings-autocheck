package main

import (
	"context"
	"testing"
	"time"
)

// from-api 与 write 必须互斥：API 清单没有行级上下文，写回会打错站。
func TestRunOnceFromAPIWriteMutex(t *testing.T) {
	err := runOnce(context.Background(), runConfig{FromAPI: true, Write: true})
	if err == nil {
		t.Fatal("from-api + write must be rejected")
	}
	t.Logf("rejected: %v", err)
}

// 回归锁：from-api dry-run 必须在「没有任何数据库参数」时跑通，
// 一旦有人把取数改回无条件 db.Open，这里会立刻红。
func TestRunOnceFromAPINoTouchDB(t *testing.T) {
	cfg := runConfig{
		FromAPI: true, Write: false,
		Driver: "mysql", DSN: "bogus:bogus@tcp(127.0.0.1:1)/nope", // 故意不可用
		Limit: 1, Stage: "raw", Format: "json",
		HTTPWorkers: 2, FetchQueue: 4, TimeoutMS: 5000, JSSettleMS: 2000,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := runOnce(ctx, cfg); err != nil {
		t.Fatalf("from-api dry-run must not touch DB, got: %v", err)
	}
}
