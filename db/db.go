package db

import (
	"errors"
	"os"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Config 是 DB 连接配置。字段对齐 .env（生产用 MySQL，本地用 SQLite）。
type Config struct {
	Driver     string // "sqlite" | "mysql"
	DSN        string
	SQLitePath string // Driver=sqlite 时用
}

func open(cfg Config) (*gorm.DB, error) {
	switch cfg.Driver {
	case "sqlite":
		return gorm.Open(sqlite.Open(cfg.SQLitePath), &gorm.Config{})
	case "mysql":
		return gorm.Open(mysql.Open(cfg.DSN), &gorm.Config{})
	default:
		return nil, errors.New("未知 driver: " + cfg.Driver)
	}
}

// Web 对齐 webs 表（成员站 + 巡查状态）。name/link/tag 是成员元数据，本包只读不写。
type Web struct {
	ID              int        `gorm:"column:id;primaryKey"`
	Status          string     `gorm:"column:status"`
	Name            string     `gorm:"column:name"`
	Link            string     `gorm:"column:link"`
	Tag             *string    `gorm:"column:tag"`
	FailedReason    *string    `gorm:"column:failedReason"`
	// lastManualCheck 列故意不声明字段：30 天豁免在 SQL WHERE 里完成，Go 侧从不读它的值。
	// 一旦声明 *time.Time，mysql 驱动在 DSN 未开 parseTime 时会把 DATETIME 当 []uint8 返回、
	// 整行扫描直接报错（2026-11-02 线上实测：unsupported Scan ... into *time.Time）。
}

// TableName 固定 webs 表（gorm 默认会把 Web 复数成 webs，但显式写出防歧义）。
func (Web) TableName() string { return "webs" }

// ListSites 取待巡清单：按 30 天豁免过滤（线上口径契约）。
// status 不过滤（全量入队），对齐原项目 axios 阶段取数语义。
func ListSites(db *gorm.DB, now time.Time) ([]Web, error) {
	cutoff := now.Add(-30 * 24 * time.Hour)
	var ws []Web
	err := db.Where(
		"lastManualCheck IS NULL OR lastManualCheck < ?",
		cutoff,
	).Find(&ws).Error
	return ws, err
}

// Open 从环境/参数构造。供 cmd 调用。
func Open(driver, sqlitePath, mysqlDSN string) (*gorm.DB, error) {
	if driver == "" {
		driver = os.Getenv("DB_DRIVER")
	}
	if driver == "sqlite" {
		if sqlitePath == "" {
			sqlitePath = os.Getenv("SQLITE_PATH")
		}
		if sqlitePath == "" {
			sqlitePath = "gorm.db"
		}
		return open(Config{Driver: "sqlite", SQLitePath: sqlitePath})
	}
	// MySQL
	if mysqlDSN == "" {
		mysqlDSN = os.Getenv("DB_DSN")
	}
	if mysqlDSN == "" {
		return nil, errors.New("mysql 需要 DB_DSN 或 --dsn")
	}
	return open(Config{Driver: "mysql", DSN: mysqlDSN})
}

// VerdictUpdate 是待写回的一条判定。
type VerdictUpdate struct {
	SiteID int
	Status string
	Reason string
}

// WriteVerdicts 批量写回 webs，只动 status/failedReason/lastManualCheck（契约：
// member 元数据 name/link/tag 只读；不 INSERT/DELETE）。WAIT 不变量写进 SQL——
// status='WAIT' 且新值<>RUN 时保持 WAIT（原 sqlModel.ts beforeUpdate 语义）。
// 单事务，幂等（按 id 覆盖）。
func WriteVerdicts(db *gorm.DB, updates []VerdictUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, u := range updates {
			// 只动三列；WAIT 不变量用 CASE 在 SQL 层强制
			res := tx.Model(&Web{}).Where("id = ?", u.SiteID).
				Updates(map[string]interface{}{
					"status": gorm.Expr(
						"CASE WHEN status = 'WAIT' AND ? <> 'RUN' THEN 'WAIT' ELSE ? END",
						u.Status, u.Status),
					"failedReason":    nullableReason(u.Reason),
					"lastManualCheck": nil,
				})
			if res.Error != nil {
				return res.Error
			}
		}
		return nil
	})
}

func nullableReason(r string) *string {
	if r == "" {
		return nil
	}
	return &r
}

// RunStats 计算本轮 RUN 占比（用于熔断闸门）。分母 = 非 WAIT。
func RunStats(updates []VerdictUpdate) (runPct float64, count int) {
	var runN, total int
	for _, u := range updates {
		total++
		if u.Status == "RUN" {
			runN++
		}
	}
	if total == 0 {
		return 0, 0
	}
	return 100 * float64(runN) / float64(total), total
}
