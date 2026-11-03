package report

import (
	"travellings-bot-go/db"
	"travellings-bot-go/pipeline"

	"github.com/rs/zerolog"
	"gorm.io/gorm"
)

// pkglog 由 SetLogger 注入（对齐 pipeline/crawl/feishu 模式）。
var pkglog = zerolog.Nop()

// SetLogger 让调用方把配置好的 logger 传进来。
func SetLogger(l zerolog.Logger) { pkglog = l }

// Finalize 执行轮末「占比闸门→写回」共享逻辑（MS_TASK §5：bot run 与 master 各调一次，
// 不复制实现）。调用前提：非中断、cfg.Write 已开、d 非 nil——这些判断留在调用方，
// 因为两侧语义来源不同（run 是 ^C 信号，master 是无中断批处理）。
// 返回 (written, err)：闸门丢弃 = (false, nil)；写库出错 = (false, err)，
// 由调用方上抛/记录——保持与抽取前 runOnce 完全的控制流语义（写库失败仍走错误路径）。
// 日志措辞与抽取前的 runOnce 尾段逐字一致，保证运维口径不变。
func Finalize(d *gorm.DB, verdicts []pipeline.Verdict, minPct float64) (written bool, err error) {
	ups := Updates(verdicts)
	pct, count := db.RunStats(ups)
	pkglog.Info().Float64("run_pct", pct).Int("sites", count).Msg("RUN 占比")
	if pct < minPct {
		pkglog.Warn().Float64("min_pct", minPct).Msg("低于阈值，本轮结果丢弃不写库")
		return false, nil
	}
	if err := Write(d, verdicts); err != nil {
		return false, err
	}
	return true, nil
}

// Write 直接写回（不判占比闸门）——--limit 调试/灰度轮的旁路用，
// bot run 与 master 共用同一实现。日志措辞与抽取前 runOnce 一致。
func Write(d *gorm.DB, verdicts []pipeline.Verdict) error {
	ups := Updates(verdicts)
	if err := db.WriteVerdicts(d, ups); err != nil {
		return err
	}
	pkglog.Info().Int("sites", len(ups)).Msg("已写回")
	return nil
}
