// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/atomicfile"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/guard"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfile"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// appVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
//
// 1.13.2-panel = 本 fork 在 1.13.0 发行版之上的修复版。CI（.github/workflows/go-binaries.yml）
// 会断言「打 tag 的版本号 == 源码 appVersion（剥掉 -panel 后缀）」，所以发布前必须先改这里，
// 否则任何 v* tag 都会在发布作业里失败。
const appVersion = "1.13.3-panel"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

// fallbackDirs 返回「同目录不可写时可用的暂存目录」候选列表。
//
// 场景（issue #226 / #134）：Docker 单文件 bind mount 下 config.json 位于镜像内
// /app（属主 uid 10001），而 compose 允许以宿主机 uid 运行容器——同目录的
// config.json.tmp 因此无法创建（EACCES），面板保存配置永远失败。
//
// 兜底顺序：
//  1. state 文件所在目录（Docker 部署下即 ./data，宿主机挂载、必然可写）；
//  2. auth 目录（./auths，同样是宿主机挂载目录）；
//  3. 系统临时目录（os.TempDir 由 atomicfile 内部追加）。
//
// 落在哪个目录都不影响语义：atomicfile 会把暂存文件 rename 回 config.json；
// 跨设备时退化为原地覆盖目标文件（单文件挂载点 rename 必然 EBUSY）。
func fallbackDirs(stateFile string) []string {
	dirs := []string{filepath.Dir(stateFile)}
	return dirs
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)
	p.SetExpiringSoonWindow(cfg.ExpiringSoonDur)                                                                           // 快过期紧迫度梯度窗口（issue #160）
	p.SetCrossDomainFallback(cfg.Pool.CrossDomainFallback)                                                                 // 裸名跨域回退（issue #135，默认关闭）
	p.SetBurnGuard(cfg.Pool.BurnGuardLimit, cfg.BurnGuardWindowDur, cfg.BurnGuardCooldownDur, cfg.BurnGuardCooldownMaxDur) // 积分燃尽熔断（issue #229，默认 0 = 关闭）
	_ = p.SetScheduling(cfg.Pool.Strategy, cfg.Pool.AccountOrder)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	applyCNChatBase(up, cfg)
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// DNS：Android 没有 /etc/resolv.conf，Go 的纯 Go 解析器会退回 127.0.0.1/::1，
	// 于是查任何域名都变成"连本机 53 端口"（dial tcp: lookup copilot.tencent.com
	// on [::1]:53: connection refused）。配了 dns_servers / WB2A_DNS 就显式指定。
	if len(cfg.DNSServers) > 0 {
		d := upstream.NewDNSDialer(cfg.DNSServers)
		up.SetDNSDialer(d)
		// 推送 webhook / Upstash 镜像等走 http.DefaultTransport，一并换掉
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t.DialContext = d.DialContext
		}
		log.Printf("DNS: 出站解析用 %v（不走系统 /etc/resolv.conf）", cfg.DNSServers)
	}
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		// 周期保活只刷新临期凭证（默认 7 天窗口）；面板手动全量保活不受影响。
		KeepaliveRefreshWindow: cfg.KeepaliveRefreshWindowDur,
		CheckinDisabled:        !cfg.Schedule.CheckinEnabled,
		TravelDisabled:         !cfg.Schedule.TravelEnabled,
		ActivityDisabled:       !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:      !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:       !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:         !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	switch {
	case !cfg.Schedule.TaskSweepEnabled:
		log.Printf("任务补领清扫已禁用（schedule.task_sweep_enabled=false）")
	case cfg.TaskSweepInterval > 0:
		log.Printf("任务补领清扫：每 %s（补领已达标未领的成长任务奖励）", cfg.TaskSweepInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(cfg.Models.Hidden),
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	// 积分历史：把每次真实查到的余额与上一次比对，变动即留痕（新账号首次只建
	// 基线）。挂载在余额查询路径上（upstream 的余额观察者），签到 / 活跃上报 /
	// 旅行 / 保活 / 面板手动刷新全覆盖——上游只在少数渠道落金额日志，比对余额
	// 是唯一可靠的口径。与 state/usage/request-logs 同目录，随 state_file 搬移。
	creditHist := credithist.New(stateSibling(cfg.StateFile, "credit-history.json"), 2000)
	up.SetCreditObserver(creditHist.Observe)
	log.Printf("[credithist] 积分历史已启用: %s", stateSibling(cfg.StateFile, "credit-history.json"))

	// 配置保存串行化（#104）：面板并发保存时，read-modify-write 竞态会丢掉先到的
	// 改动；用互斥锁把整个读改写串成一条。
	var configSaveMu sync.Mutex

	// 试用守卫：监控名单内的试用/限免模型一旦产生 usage.credit（免费结束开始
	// 计费），立即持久禁用并从 /v1/models 移除；到达 promo_valid_until 自动下线；
	// 上游延长免费则自动复活（同一份已被证伪的声明除外）。
	//   TRIAL_GUARD_ENABLED=false  关闭
	//   TRIAL_GUARD_WATCH=a,b      监控名单（逗号分隔，可带 cn: 前缀）
	//   TRIAL_GUARD_STATE=path     状态文件（默认 data/trial_guard.json）
	//   TRIAL_GUARD_WINDOWS=模型|HH:MM-HH:MM|[星期1-7]|[block|allow]  时段规则
	var trialGuard *guard.State
	if !strings.EqualFold(os.Getenv("TRIAL_GUARD_ENABLED"), "false") {
		guardPath := os.Getenv("TRIAL_GUARD_STATE")
		if guardPath == "" {
			guardPath = "data/trial_guard.json"
		}
		// 默认名单刻意留空：候选模型名是提交者自己环境里的三个（hy4-preview-f /
		// step-5-preview / hy3），换一台机器就是别人的模型。默认禁掉一批用户
		// 从没配置过的模型属于静默误伤，要用必须显式列 TRIAL_GUARD_WATCH。
		guardWatch := os.Getenv("TRIAL_GUARD_WATCH")
		var watch []string
		if strings.TrimSpace(guardWatch) != "" {
			watch = strings.Split(guardWatch, ",")
		}
		trialGuard = guard.New(guardPath, watch)
		for _, e := range trialGuard.List() {
			log.Printf("WARN: [server] trial guard restored disabled model=%s reason=%s credit=%.4f at=%s",
				e.Model, e.Reason, e.Credit, e.DetectedAt.Format(time.RFC3339))
		}
		for m, d := range trialGuard.Deadlines() {
			log.Printf("INFO: [server] trial guard deadline model=%s at=%s", m, d.Format(time.RFC3339))
		}
		if spec := os.Getenv("TRIAL_GUARD_WINDOWS"); strings.TrimSpace(spec) != "" {
			for _, r := range parseGuardWindows(spec) {
				if err := trialGuard.SetWindow(r); err != nil {
					log.Printf("WARN: [server] trial guard window rejected model=%s: %s", r.Model, err)
					continue
				}
				log.Printf("INFO: [server] trial guard window model=%s %s-%s days=%v mode=%s",
					r.Model, r.Start, r.End, r.Days, r.Mode)
			}
		}
	}

	pn := panel.New(panel.Config{
		Pool:             p,
		Usage:            rec,
		RequestLog:       requestLog,
		Upstream:         up,
		Scheduler:        sch,
		AutoTasksEnabled: sch.GrowthEnabled,
		AuthDir:          cfg.AuthDir,
		APIKey:           cfg.APIKey,
		RedisMode:        redisMode,
		StickyCount:      sessCount,
		Version:          appVersion,
		Live:             live,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:  stateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			configSaveMu.Lock()
			defer configSaveMu.Unlock()
			return saveConfig(raw, *cfgPath, live, p, up, sch, cfg, fallbackDirs(cfg.StateFile)...)
		},
		// 积分历史账本（只读展示；写入由上面的余额观察者完成）。
		CreditHistory: creditHist,
	})
	if err := pn.Logs().SetTaskArchive(stateSibling(cfg.StateFile, "task-logs.json")); err != nil {
		log.Printf("WARN: task log archive: %v", err)
	}
	// 每日将国区账号交给持久化后台任务；账号内互斥，返回不等待上游操作。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	// 清扫回调返回统计汇总（测试/日志可观测），调度器 hook 只要求 func()：
	// 这里收掉返回值——汇总已由 panel 侧内部日志输出。
	sch.SetTaskSweepHook(func() { pn.RunTaskSweepOnce() })
	// 日志落盘（logging.log_file）：Windows 上没有控制台窗口时，出问题只能靠猜。
	// 打不开文件就降级（只 WARN，不让网关起不来）——stderr 仍然有输出。
	var logFile *logfile.Writer
	if cfg.Logging.LogFile != "" {
		lf, err := logfile.New(cfg.Logging.LogFile, cfg.Logging.LogFileMaxMB, cfg.Logging.LogFileBackups)
		if err != nil {
			log.Printf("WARN: 日志落盘不可用（仅输出到控制台）: %v", err)
		} else {
			logFile = lf
			defer logFile.Close()
		}
	}
	var logWriters []io.Writer
	logWriters = append(logWriters, os.Stderr, pn.Logs())
	if logFile != nil {
		logWriters = append(logWriters, logFile)
	}
	log.SetOutput(io.MultiWriter(logWriters...))
	var chatWriters []io.Writer
	chatWriters = append(chatWriters, os.Stdout, pn.Logs())
	if logFile != nil {
		chatWriters = append(chatWriters, logFile)
	}
	server.SetChatLogOutput(io.MultiWriter(chatWriters...))
	if logFile != nil {
		log.Printf("INFO: 日志落盘已启用: %s", logFile.Path())
	}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		RequestLog:   requestLog,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		TrialGuard:    trialGuard,
		GlobalEnabled: cfg.Global.Enabled,
		// 模型黑名单原始条目；运行期以 livecfg 快照为准（热生效），此处供
		// Live 为 nil 的裸用/测试路径。
		ModelsHidden: cfg.Models.Hidden,
		// 零 tool_calls 停摆重试（issue #149）：非流式请求声明了 tools 却收到
		// 「只回正文 + stop 收尾」时换号再问一次。**不罚账号**（上游行为缺陷）。
		ToolStallRetry: cfg.Upstream.ToolStallRetry,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := pn.StartTaskJobs(ctx, stateSibling(cfg.StateFile, "task-jobs.json")); err != nil {
		log.Fatalf("task jobs: %v", err)
	}
	defer pn.StopTaskJobs()
	pn.ResumeTaskJobs()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	sch.StartTaskSweep(ctx, cfg.TaskSweepInterval)

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	go warmModelRates(ctx, up, p)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
// saveConfig 保存面板提交的配置：合并 → 校验 → 原子落盘 → 热应用，
// 并返回「本次改动中需要重启进程才能生效」的字段名清单。
//
// running 是**进程启动时**那份配置（可传 nil）：它与新配置做差集，得到真正
// 需要重启的字段；已改但尚未重启的字段会一直保留在清单里，直到改回原值
// 或重启（#104）。
//
// fallbackDirs 是同目录 tmp 不可写时的暂存目录候选（issue #226 / #134），
// 交给 atomicfile.Write 逐个尝试。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, running *Config, fallbackDirs ...string) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	previous, err := ParseConfig(oldRaw)
	if err != nil {
		return nil, fmt.Errorf("parse current config: %w", err)
	}
	baseline := previous
	if running != nil {
		baseline = running
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	// Environment overrides have the same precedence on save as on startup.
	applyEnv(newCfg)
	if err := newCfg.normalize(); err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	// 同目录 tmp 不可写时退到 data 目录暂存（Docker 单文件 bind mount 场景：
	// config.json 挂在 /app/config.json，而 /app 属镜像内 uid 10001，
	// tmp 写不动；./data 是宿主机挂载目录，一定可写）——见 issue #226 / #134。
	//
	// 覆盖前先留一份 .bak 备份：降级路径（rename 不可用时的原地覆盖）是
	// O_TRUNC 覆写、非原子，写一半被打断 ⇒ 磁盘上留下半截 config.json ⇒
	// 下次启动解析失败、服务起不来。留了备份就能自动回退（见 config.go Load）。
	backupConfigBeforeWrite(path)
	if err := atomicfile.Write(path, out, 0o600, fallbackDirs...); err != nil {
		return nil, err
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(newCfg.Models.Hidden),
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	p.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)                                                                                    // 快过期紧迫度梯度窗口热生效（issue #160）
	p.SetCrossDomainFallback(newCfg.Pool.CrossDomainFallback)                                                                          // 裸名跨域回退热生效（issue #135）
	p.SetBurnGuard(newCfg.Pool.BurnGuardLimit, newCfg.BurnGuardWindowDur, newCfg.BurnGuardCooldownDur, newCfg.BurnGuardCooldownMaxDur) // 积分燃尽熔断热生效（issue #229）
	_ = p.SetScheduling(newCfg.Pool.Strategy, newCfg.Pool.AccountOrder)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.SetKeepaliveRefreshWindow(newCfg.KeepaliveRefreshWindowDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetTaskSweepInterval(newCfg.TaskSweepInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)

	return restartRequiredFields(baseline, newCfg), nil
}

// applyCNChatBase 把 config cn.chat_base 装配进上游 client；空则保留内置默认，
// 老配置（无 cn 段）行为不变。
func applyCNChatBase(up *upstream.Client, cfg *Config) {
	if cfg.CN.ChatBase != "" {
		up.ChatBaseCN = cfg.CN.ChatBase
	}
}

// restartRequiredFields 比较运行中装配值与新配置，也保留此前尚未重启的改动。
//
// current = 进程启动时那份配置（baseline）；next = 本次保存后的新配置。
// 两者不同的字段即「改了但热生效不了、必须重启」的字段。
//
// 恒列出的字段（无论改没改都提示）：这些字段在进程内被监听地址 / HTTP client /
// 目录句柄等**装配期对象**捕获，保存后不会自动生效——面板据此提示用户重启。
// changed() 只负责「新增的」需要重启的字段；恒列字段另外无条件追加。
func restartRequiredFields(current, next *Config) []string {
	var out []string
	changed := func(name string, before, after any) {
		if !reflect.DeepEqual(before, after) {
			out = append(out, name)
		}
	}
	changed("listen", current.Listen, next.Listen)
	changed("auth_dir", current.AuthDir, next.AuthDir)
	changed("state_file", current.StateFile, next.StateFile)
	changed("upstream.timeout_seconds", current.Upstream.TimeoutSeconds, next.Upstream.TimeoutSeconds)
	changed("upstream.header_timeout_seconds", current.Upstream.HeaderTimeoutSeconds, next.Upstream.HeaderTimeoutSeconds)
	changed("upstream.idle_timeout_seconds", current.Upstream.IdleTimeoutSeconds, next.Upstream.IdleTimeoutSeconds)
	// upstream.user_agent 在装配期被写进出站 client（main.go 的 up.UserAgent = ...），
	// 之后不再读取——不在 livecfg 热快照里，也无法热改。此前漏列，导致面板改完
	// 显示"已保存"却不提示需要重启，用户以为没生效（issue #102 附带发现 2）。
	changed("upstream.user_agent", current.Upstream.UserAgent, next.Upstream.UserAgent)
	changed("upstream.client_version", current.Upstream.ClientVersion, next.Upstream.ClientVersion)
	changed("upstream.cli_version", current.Upstream.CliVersion, next.Upstream.CliVersion)
	changed("upstream.client_name", current.Upstream.ClientName, next.Upstream.ClientName)
	changed("upstream.device_token", current.Upstream.DeviceToken, next.Upstream.DeviceToken)
	changed("upstream.device_token_file", current.Upstream.DeviceTokenFile, next.Upstream.DeviceTokenFile)
	changed("upstream.passthrough_ip", current.Upstream.PassthroughIP, next.Upstream.PassthroughIP)
	// 零 tool_calls 停摆重试（issue #149）：装配期读进 server.Config，进程内不再
	// 读取——改了必须重启才生效（漏列会让面板显示「已保存」却不生效）。
	changed("upstream.tool_stall_retry", current.Upstream.ToolStallRetry, next.Upstream.ToolStallRetry)
	changed("global.enabled", current.Global.Enabled, next.Global.Enabled)
	changed("global.chat_base", current.Global.ChatBase, next.Global.ChatBase)
	changed("global.billing_base", current.Global.BillingBase, next.Global.BillingBase)
	changed("prompt.mode", current.Prompt.Mode, next.Prompt.Mode)
	changed("prompt.file", current.Prompt.File, next.Prompt.File)
	changed("prompt.text", current.PromptText, next.PromptText)
	changed("upstash.url", current.Upstash.URL, next.Upstash.URL)
	changed("upstash.token", current.Upstash.Token, next.Upstash.Token)
	changed("session_sticky.enabled", current.SessionSticky.Enabled, next.SessionSticky.Enabled)
	changed("session_sticky.ttl", current.SessionTTL, next.SessionTTL)
	changed("session_sticky.gc_interval", current.SessionGCInterval, next.SessionGCInterval)
	changed("logging.request_archive_enabled", current.Logging.RequestArchiveEnabled, next.Logging.RequestArchiveEnabled)
	changed("logging.request_retention_days", current.Logging.RequestRetentionDays, next.Logging.RequestRetentionDays)
	changed("logging.request_archive_max_mb", current.Logging.RequestArchiveMaxMB, next.Logging.RequestArchiveMaxMB)
	// cn.chat_base 同样只在装配期写进 up.ChatBaseCN（请求路径并发读取，不做热改）。
	changed("cn.chat_base", current.CN.ChatBase, next.CN.ChatBase)
	// server.read_timeout 写进 http.Server.ReadTimeout（装配期）。
	changed("server.read_timeout", current.Server.ReadTimeout, next.Server.ReadTimeout)
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// parseGuardWindows 解析 TRIAL_GUARD_WINDOWS：
//
//	模型|HH:MM-HH:MM|[星期1-7连写]|[block|allow]，多条用逗号分隔
//
//	例：hy4-preview-f|23:00-08:00||allow   （只在夜间允许，白天强制熔断）
//	    step-5-preview|09:00-18:00|12345|block （工作日白天强制熔断）
func parseGuardWindows(spec string) []guard.WindowRule {
	out := make([]guard.WindowRule, 0, 4)
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, "|")
		if len(parts) < 2 {
			continue
		}
		model := strings.TrimSpace(parts[0])
		span := strings.SplitN(strings.TrimSpace(parts[1]), "-", 2)
		if model == "" || len(span) != 2 {
			continue
		}
		rule := guard.WindowRule{
			Model: model,
			Start: strings.TrimSpace(span[0]),
			End:   strings.TrimSpace(span[1]),
			Mode:  "block",
		}
		if len(parts) >= 3 {
			for _, ch := range strings.TrimSpace(parts[2]) {
				if ch >= '1' && ch <= '7' {
					rule.Days = append(rule.Days, int(ch-'0'))
				}
			}
		}
		if len(parts) >= 4 {
			if m := strings.ToLower(strings.TrimSpace(parts[3])); m != "" {
				rule.Mode = m
			}
		}
		out = append(out, rule)
	}
	return out
}
