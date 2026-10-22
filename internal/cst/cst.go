// Package cst 固定 UTC+8（北京时间）：上游「自然日」的唯一定义。
//
// 为什么需要一个包：WorkBuddy 的额度、签到、每日活跃、夜猫子窗口、积分包到期日
// 全部按北京时间零点切日，而网关可能跑在 Docker / NAS / 海外 VPS 或改了时区的
// Windows 上——那些机器的本地时区不是 UTC+8。日界跟着宿主时区走，「今天」的边界
// 就整体漂移 8 小时，而且**不会报任何错**，只会静默少拿积分：
//
//   - 排程钟点：配 activity_hours=[10]，在 UTC 主机上实际于 CST 18:00 执行；
//   - 硬冷却「次日 04:00」落在 CST 12:00（此时签到早已结束，等于白等）；
//   - 夜猫子 23:00–08:00 窗口错位、签到日键跨日、积分包到期批次比较错日；
//   - 积分包切片日期以错误日期发给上游（SlicePeriodStartTime/EndTime）。
//
// 所以日界只能由一个常量决定，不能跟着宿主走。
//
// 为什么是固定偏移而不是时区库：中国标准时间没有夏令时，1970 年后恒为 +08:00，
// 固定偏移既够用又不依赖宿主 tzdata（tdsdata 缺失的容器里 time.LoadLocation 会
// 直接失败，那才是更难查的故障）。
//
// 什么时候**不该**用本包：面向操作者的统计口径（用量报表按小时/按天分桶、请求
// 日志按天切文件、面板里手填的时间）保留宿主本地时区——那看的是「我的昨天」，
// 不是上游的自然日。
package cst

import "time"

// Loc 固定 UTC+8 时区（无夏令时）。
var Loc = time.FixedZone("CST", 8*60*60)

// Now 当前时刻（带 CST 时区信息；time.Time 存的是绝对时刻，换区不改变语义）。
func Now() time.Time { return time.Now().In(Loc) }

// In 把 t 换算到 CST。
func In(t time.Time) time.Time { return t.In(Loc) }

// DayLayout 上游自然日的字符串布局；字典序即时间序。
const DayLayout = "2006-01-02"

// Day 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func Day(t time.Time) string { return t.In(Loc).Format(DayLayout) }
