package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"travellings-bot-go/db"
)

// 验证 runOnce 在 sqlite dry-run 下可本地跑通。
//
// 建库内联在测试里：不依赖仓库外的预置 db 文件，别人 clone 后
// go test ./... 能直接跑通（旧写法靠 tmp/test.db，那是手工 seed 的本地状态，
// 不在版本库里，等于把测试前提藏在了开发者机器上）。
func TestRunOnceSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open("sqlite", dbPath, "")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := d.Exec(
		"CREATE TABLE webs (id INTEGER PRIMARY KEY, status TEXT, name TEXT, link TEXT, " +
			"tag TEXT, failedReason TEXT, lastManualCheck DATETIME)").Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	// 只种一行、指向 example.com：--limit 1 就只抓这一站，外网依赖最小且稳定。
	if err := d.Exec(
		"INSERT INTO webs (status, name, link) VALUES ('RUN', 'example', 'https://example.com')").Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 建库用的这条连接用完即关：否则它在 t.TempDir 清理时仍握着文件句柄，
	// Windows 下 unlinkat 会失败（runOnce 另开一条连接，不依赖这条）。
	if sqlDB, e := d.DB(); e == nil {
		_ = sqlDB.Close()
	}

	cfg := runConfig{
		Driver: "sqlite", SQLitePath: dbPath,
		Limit: 1, Stage: "raw", Format: "json",
		HTTPWorkers: 2, FetchQueue: 4, TimeoutMS: 3000, JSSettleMS: 2000,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runOnce(ctx, cfg); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
}
