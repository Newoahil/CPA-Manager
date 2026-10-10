package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// TestCardGallery renders every card situation through the real renderer and
// writes an HTML preview. It only runs when GALLERY_OUT names the output file:
//
//	GALLERY_OUT=/out/card-gallery.html go test ./internal/notify/render -run TestCardGallery
func TestCardGallery(t *testing.T) {
	out := os.Getenv("GALLERY_OUT")
	if out == "" {
		t.Skip("GALLERY_OUT not set")
	}
	t.Setenv("NOTIFY_CPA_PAGE_URL", "https://cpa.example.com/management.html")

	loc, _ := time.LoadLocation("Asia/Shanghai")
	gen := time.Date(2026, 10, 8, 6, 30, 0, 0, time.UTC) // 14:30 Shanghai
	r := New("casual").WithLocation(loc).WithIgnoredGroups([]string{"Claude and GPT models"})
	at := func(h time.Duration) *time.Time { tm := gen.Add(h); return &tm }

	win := func(name, label string, scope domain.QuotaScope, scopeID string, used float64, reset time.Duration) domain.QuotaWindow {
		return domain.QuotaWindow{Name: name, Label: label, Scope: scope, ScopeID: scopeID, UsedPercent: pctp(used), ResetAt: at(reset)}
	}
	okSnap := func(p domain.ProviderKind, key, alias, plan string, ws ...domain.QuotaWindow) domain.QuotaSnapshot {
		return domain.QuotaSnapshot{
			Credential: domain.Credential{Key: key, Provider: p, Alias: alias},
			Plan:       plan, OK: true, Confidence: domain.ConfidenceReported,
			FetchedAt: gen, LastSuccessAt: gen, Windows: ws,
		}
	}
	claude5h := func(used float64, reset time.Duration) domain.QuotaWindow {
		return win("claude/five_hour", "账号 · 5小时", domain.ScopeAccount, "", used, reset)
	}
	claude7d := func(used float64, reset time.Duration) domain.QuotaWindow {
		return win("claude/seven_day", "账号 · 7天", domain.ScopeAccount, "", used, reset)
	}
	const gemGroup = "antigravity/groups/Gemini%20Models#1"
	const cgGroup = "antigravity/groups/Claude%20and%20GPT%20Models#1"
	agSnap := func(key, alias string, used5h, usedWeek float64) domain.QuotaSnapshot {
		return okSnap(domain.ProviderAntigravity, key, alias, "Pro",
			win(gemGroup+"/buckets/g5h/five_hour#1", "Gemini Models · 5小时", domain.ScopeGroup, gemGroup, used5h, 3*time.Hour),
			win(gemGroup+"/buckets/gw/weekly#1", "Gemini Models · 周", domain.ScopeGroup, gemGroup, usedWeek, 5*24*time.Hour),
			win(cgGroup+"/buckets/c5h/five_hour#1", "Claude and GPT models · 5小时", domain.ScopeGroup, cgGroup, 40, 4*time.Hour),
		)
	}
	codexWeek := func(used float64) domain.QuotaWindow {
		return win("codex/weekly", "账号 · 周窗口", domain.ScopeAccount, "", used, 4*24*time.Hour)
	}

	provider := func(p domain.ProviderKind, states map[string]domain.CredentialState, snaps ...domain.QuotaSnapshot) domain.ProviderReport {
		pr := domain.ProviderReport{Provider: p, Total: len(snaps), States: states, Snapshots: snaps}
		worst := domain.StateHealthy
		for _, s := range snaps {
			st := states[s.Credential.Key]
			switch domain.ClassOf(st) {
			case domain.ClassNormal:
				pr.Normal++
			case domain.ClassLimited:
				pr.Limited++
			default:
				pr.Abnormal++
			}
			if stateRankForGallery(st) > stateRankForGallery(worst) {
				worst = st
			}
		}
		pr.WorstState = worst
		return pr
	}

	// --- baseline: everything healthy --------------------------------------
	healthyClaude := provider(domain.ProviderClaude,
		map[string]domain.CredentialState{"cl1": domain.StateHealthy, "cl2": domain.StateHealthy},
		okSnap(domain.ProviderClaude, "cl1", "claude-External0.2", "团队版", claude5h(1, 5*time.Hour), claude7d(30, 3*24*time.Hour)),
		okSnap(domain.ProviderClaude, "cl2", "claude-External", "团队版", claude5h(17, 2*time.Hour), claude7d(44, 2*24*time.Hour)),
	)
	ag := provider(domain.ProviderAntigravity,
		map[string]domain.CredentialState{"ag1": domain.StateLimited, "ag2": domain.StateLimited, "ag3": domain.StateLimited, "ag4": domain.StateLimited},
		agSnap("ag1", "antigravity-hongwane3", 7, 4),
		agSnap("ag2", "antigravity-leacanva92", 7, 4),
		agSnap("ag3", "antigravity-leejhyijiaace5", 5, 2),
		agSnap("ag4", "antigravity-newoahil", 3, 1),
	)
	healthyCodex := provider(domain.ProviderCodex,
		map[string]domain.CredentialState{"cx1": domain.StateHealthy},
		okSnap(domain.ProviderCodex, "cx1", "codex-vinsprite78", "Pro 20x", codexWeek(16)),
	)
	healthyOllama := provider(domain.ProviderOllama,
		map[string]domain.CredentialState{"ol1": domain.StateHealthy},
		okSnap(domain.ProviderOllama, "ol1", "ollama-main", "",
			win("ollama/five_hour", "账号 · 5小时", domain.ScopeAccount, "", 23.7, 3*time.Hour),
			win("ollama/seven_day", "账号 · 7天", domain.ScopeAccount, "", 33.8, 6*24*time.Hour),
		),
	)
	repHealthy := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{healthyClaude, ag, healthyCodex, healthyOllama}}

	// --- one account close to its limit ------------------------------------
	noticeClaude := provider(domain.ProviderClaude,
		map[string]domain.CredentialState{"cl1": domain.StateWarning, "cl2": domain.StateHealthy},
		okSnap(domain.ProviderClaude, "cl1", "claude-External0.2", "团队版", claude5h(93.5, 2*time.Hour), claude7d(60, 3*24*time.Hour)),
		healthyClaude.Snapshots[1],
	)
	repNotice := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{noticeClaude, ag, healthyCodex}}

	// --- exhausted + invalid credential ------------------------------------
	badCodex := provider(domain.ProviderCodex,
		map[string]domain.CredentialState{"cx-bad": domain.StateInvalid, "cx1": domain.StateHealthy},
		domain.QuotaSnapshot{
			Credential: domain.Credential{Key: "cx-bad", Provider: domain.ProviderCodex, Alias: "codex-design"},
			Failure:    domain.FailureAuth, Code: "401", FetchedAt: gen, LastSuccessAt: gen.Add(-26 * time.Hour),
		},
		healthyCodex.Snapshots[0],
	)
	fullClaude := provider(domain.ProviderClaude,
		map[string]domain.CredentialState{"cl1": domain.StateExhausted, "cl2": domain.StateHealthy},
		okSnap(domain.ProviderClaude, "cl1", "claude-External0.2", "团队版", claude5h(100, 90*time.Minute), claude7d(71, 3*24*time.Hour)),
		healthyClaude.Snapshots[1],
	)
	repBad := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{fullClaude, ag, badCodex}}

	// --- stale / degraded ---------------------------------------------------
	staleSnap := domain.QuotaSnapshot{
		Credential: domain.Credential{Key: "cl2", Provider: domain.ProviderClaude, Alias: "claude-External"},
		Failure:    domain.FailureTransport, Code: "429", FetchedAt: gen, LastSuccessAt: gen.Add(-50 * time.Minute),
	}
	staleClaude := provider(domain.ProviderClaude,
		map[string]domain.CredentialState{"cl1": domain.StateHealthy, "cl2": domain.StateStale},
		healthyClaude.Snapshots[0], staleSnap,
	)
	repStale := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{staleClaude, ag, healthyCodex}}
	repDegraded := &domain.Report{
		GeneratedAt: gen.Add(-40 * time.Minute), Providers: []domain.ProviderReport{healthyClaude, ag, healthyCodex},
		Degraded: true, Notes: []string{"本轮采集未完成，以下为上一轮结果"},
	}

	alert := func(kind domain.AlertKind, sev domain.Severity, ev domain.EvidenceLevel, snap domain.QuotaSnapshot, w *domain.QuotaWindow) domain.Alert {
		a := domain.Alert{Kind: kind, Severity: sev, Evidence: ev, Credential: snap.Credential, OccurredAt: gen}
		if w != nil {
			a.Scope, a.ScopeID = w.Scope, w.ScopeID
			// The evaluator prepends a "窗口: <DisplayLabel>" fact (see
			// internal/evaluate/scope.go). The gallery deliberately does NOT
			// inject it, so it exercises the real evaluate-produced shape:
			// cards must still find the window from the snapshot via the
			// tightest-reading fallback.
		}
		return a
	}
	alertMsg := func(rep *domain.Report, alerts ...domain.Alert) domain.Message {
		kind := string(domain.AlertBootstrap)
		worst := -1
		for _, a := range alerts {
			if rk := severityRankForGallery(a.Severity); rk > worst {
				worst, kind = rk, string(a.Kind)
			}
		}
		return domain.Message{Kind: kind, Alerts: alerts, Report: rep}
	}
	// withWindowFact mirrors the evaluator: internal/evaluate/scope.go prepends
	// "窗口: <w.DisplayLabel()>". Only the entries that model the real engine
	// output use it; the plain threshold/exhausted cards deliberately keep no
	// fact so the gallery also proves the tightest-window fallback.
	withWindowFact := func(a domain.Alert, w *domain.QuotaWindow) domain.Alert {
		if w == nil {
			return a
		}
		a.Scope, a.ScopeID = w.Scope, w.ScopeID
		a.Facts = append([]string{"窗口: " + w.DisplayLabel()}, a.Facts...)
		return a
	}
	w0 := func(s domain.QuotaSnapshot, i int) *domain.QuotaWindow { w := s.Windows[i]; return &w }

	resetSnap := healthyClaude.Snapshots[0]
	warnSnap := noticeClaude.Snapshots[0]
	fullSnap := fullClaude.Snapshots[0]
	badSnap := badCodex.Snapshots[0]

	type entry struct {
		Group    string         `json:"Group"`
		Name     string         `json:"Name"`
		Desc     string         `json:"Desc"`
		Intent   string         `json:"Intent"`
		Coverage string         `json:"Coverage"`
		Mode     string         `json:"Mode"`
		Card     map[string]any `json:"Card"`
	}
	var entries []entry
	add := func(group, name, desc, intent, cov, mode string, card map[string]any) {
		entries = append(entries, entry{
			Group:    group,
			Name:     name,
			Desc:     desc,
			Intent:   intent,
			Coverage: cov,
			Mode:     mode,
			Card:     card,
		})
	}

	q := func(rep *domain.Report) domain.Message {
		return domain.Message{Kind: KindQuery, Report: rep, Freshness: "实时"}
	}
	add("查询 / 日报", "查询 · 全部正常（全渠道概览）", "@机器人 额度；所有渠道都有充裕余量",
		"用户主动 @机器人 查询全渠道可用容量与刷新节奏",
		"全覆盖：Claude/Antigravity/Ollama 均展示 5h + 7d 垂直事实行及重置时间；Codex 仅有周窗口，如实展示周余量，不捏造 5h",
		"完整卡 (交互容器 + 折叠面板)",
		r.Card(q(repHealthy)))

	add("查询 / 日报", "查询 · 有号接近上限", "Claude External0.2 的 5 小时窗口只剩 6.5%",
		"用户查看额度时迅速定位告急渠道并获知各账号紧绷程度",
		"首屏标出 5h 最紧账号 (External0.2 剩 6.5% · 16:30 刷新) 与 7d 余量 (56.0%)，明确账号归属",
		"完整卡",
		r.Card(q(repNotice)))

	add("查询 / 日报", "查询 · 用满 + 凭证失效", "Claude 一个号 5h 用满；Codex 一个号 401 失效（显示「去 CPA」按钮）",
		"展示多重故障下的渠道可用性，引导点击「去 CPA」处置失效凭证",
		"Claude 标明 1 号用满并给出其余可用号余量 (83.0%)；Codex 报 401 认证失败，不捏造额度数值",
		"完整卡",
		r.Card(q(repBad)))

	add("查询 / 日报", "查询 · 有号数据过期", "Claude External 连续取不到额度（上游 429）",
		"提示采集受阻，展示上次成功采集时间与旧额度",
		"标出 1 号数据过期，其余正常号如实显示 5h/7d 窗口及刷新时间",
		"完整卡",
		r.Card(q(repStale)))

	cached := q(repDegraded)
	cached.Freshness = "缓存 · 约 40 分钟前"
	cached.Notice = "实时采集失败，以下是 40 分钟前的数据"
	add("查询 / 日报", "查询 · 降级（缓存数据回退）", "CPA 暂时连不上，回退到 40 分钟前缓存并显著标注",
		"采集器故障时兜底保障业务连续性，明确标示数据新鲜度",
		"顶部醒目黄色警告条，折叠明细内保留上一轮 5h/7d 完整数据供查",
		"完整卡",
		r.Card(cached))

	failedMsg := domain.Message{Notice: "连接 CPA 失败 (502 Bad Gateway)"}
	add("查询 / 日报", "查询 · 采集完全失败（无缓存）", "网络异常或 502 错误，无可用缓存",
		"明确告知采集失败，避免用户误读旧数据或盲目重试",
		"无配额窗口数据，不捏造任何 0.0% 或默认值，仅提供上次成功时间",
		"完整卡",
		r.Card(failedMsg))

	single := q(&domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{noticeClaude}})
	single.Detailed = true
	add("查询 / 日报", "查询 · 单渠道详查（@机器人 claude）", "只看一个渠道时展开窗口明细",
		"深度排查特定渠道各个账号的各窗口余量分布",
		"首屏展示 5h+7d 概览与账号归属，下方自动展开（expanded: true）每个账号的 5h/7d 进度条与刷新时间",
		"完整卡（默认展开折叠面板）",
		r.Card(single))

	add("查询 / 日报", "定时日报 · 正常全景", "定时推送，和查询同一版式",
		"团队每日例行广播（10:00 / 17:00），全员掌握各渠道配额水位",
		"全覆盖：多账号渠道采用全宽垂直 5h/7d 事实行，兼顾 375px 手机阅读无截断",
		"完整卡",
		r.Card(domain.Message{Title: "额度日报", Kind: string(domain.AlertBootstrap), Report: repHealthy}))

	add("告警", "告警 · 额度已刷新", "一个号的 5h 窗口刷新",
		"额度恢复喜报，通知团队账号已回血可恢复高频使用",
		"聚焦触发窗口：Option B 并排/双事实展示 5h 刷新与 7d 次级窗口（若存在），注明刷新时间",
		"完整告警卡",
		r.Card(alertMsg(repHealthy,
			withWindowFact(alert(domain.AlertQuotaReset, domain.SeverityInfo, domain.EvidenceConfirmed, resetSnap, nil), w0(resetSnap, 0)))))

	add("告警", "告警 · 接近上限（90% / 95%）", "已用超过阈值；即便严重度为 Info，标题仍按告警类型显示橙色",
		"预警容量告急，给出切换指引（改用其他号暂用）",
		"双事实聚焦：Option B 突显 5h 触发窗口（6.5% 触发），次级 7d 窗口如实展现，结论行明确标明受阻窗口与备选渠道",
		"完整告警卡",
		r.Card(alertMsg(repNotice,
			alert(domain.AlertQuotaThreshold, domain.SeverityInfo, domain.EvidenceConfirmed, warnSnap, w0(warnSnap, 0)))))

	add("告警", "告警 · 单号额度用满", "附恢复时间和建议",
		"明确单号断流，指引切号；说明渠道内仍有余量，避免全员恐慌",
		"双事实聚焦：Option B 突显 5h 耗尽（0.0% 触发），7d 备用余量如实呈现；次级行标明其余号剩 83.0%",
		"完整告警卡",
		r.Card(alertMsg(repBad,
			withWindowFact(alert(domain.AlertQuotaExhausted, domain.SeverityUrgent, domain.EvidenceConfirmed, fullSnap, nil), w0(fullSnap, 0)))))

	add("告警", "告警 · 凭证失效（确认 401）", "上游明确 401，显示错误码和「去 CPA」",
		"运维干预警报，必须重新登录/更换 Token；提供「去 CPA」直达按钮",
		"无窗口数据：显示 401 与认证失败，绝不捏造额度数值；展示渠道内其余可用号余量",
		"完整告警卡",
		r.Card(alertMsg(repBad,
			alert(domain.AlertCredential, domain.SeverityUrgent, domain.EvidenceConfirmed, badSnap, nil))))

	add("告警", "告警 · 疑似凭证失效（合成用例）", "合成用例 / 仅渲染边界：证据不足时标「疑似」（线上仅确认失效才推凭证告警）",
		"提醒观察，非确定性故障不乱发严重报警，防狼来了",
		"无窗口数据：标签注明「疑似失效」，不凭空填补额度",
		"完整告警卡",
		r.Card(alertMsg(repBad,
			alert(domain.AlertCredential, domain.SeverityWarn, domain.EvidenceSuspected, badSnap, nil))))

	add("告警", "告警 · 疑似异常", "连续 3 次取不到额度",
		"采集链路或临时抖动预警，引起注意但不过度报警",
		"无窗口数据：展示上次成功时间与 HTTP 状态码",
		"完整告警卡",
		r.Card(alertMsg(repStale,
			alert(domain.AlertSuspect, domain.SeverityWarn, domain.EvidenceSuspected, staleSnap, nil))))

	add("告警", "告警 · 数据过期", "连续 2 次取不到额度",
		"数据保鲜预警，说明展示数据可能存在延迟",
		"无窗口数据：灰色卡片，展示上次成功时间",
		"完整告警卡",
		r.Card(alertMsg(repStale,
			alert(domain.AlertStale, domain.SeverityWarn, domain.EvidenceSuspected, staleSnap, nil))))

	add("告警", "告警 · 异常已恢复", "之前异常的号恢复正常",
		"故障解除闭环通知",
		"状态恢复：展示绿色「已恢复」，结论行「xx 已恢复」，不捏造额度刷新",
		"完整告警卡",
		r.Card(alertMsg(repHealthy,
			alert(domain.AlertRecovered, domain.SeverityInfo, domain.EvidenceConfirmed, resetSnap, nil))))

	agWarnSnap := okSnap(domain.ProviderAntigravity, "ag1", "antigravity-hongwane3", "Pro",
		win(gemGroup+"/buckets/g5h/five_hour#1", "Gemini Models · 5小时", domain.ScopeGroup, gemGroup, 92, 3*time.Hour),
		win(gemGroup+"/buckets/gw/weekly#1", "Gemini Models · 周", domain.ScopeGroup, gemGroup, 4, 5*24*time.Hour),
		win(cgGroup+"/buckets/c5h/five_hour#1", "Claude and GPT models · 5小时", domain.ScopeGroup, cgGroup, 40, 4*time.Hour),
	)
	add("告警", "告警 · 分组范围限制", "Antigravity 的 Gemini 组接近上限，不代表整个号",
		"精准定界故障影响范围，避免用户误以为整个 Antigravity 渠道不可用",
		"聚焦模型组窗口：明确注明「仅 Gemini Models · 5小时」，不混淆账号级窗口",
		"完整告警卡",
		r.Card(alertMsg(repHealthy,
			withWindowFact(alert(domain.AlertQuotaThreshold, domain.SeverityWarn, domain.EvidenceConfirmed, agWarnSnap, nil), w0(agWarnSnap, 0)))))

	add("告警", "告警 · 多条变化合并（Mixed-Batch）", "一张卡，按严重程度排序，每号一行",
		"防消息刷屏轰炸，一张卡聚合所有事件，按严重度（用满>失效>刷新）严格排序",
		"按账号逐行呈现各自核心触发事件，用满与刷新各自呈现 5h 触发窗口，失效呈现 401",
		"完整告警卡",
		r.Card(alertMsg(repBad,
			withWindowFact(alert(domain.AlertQuotaReset, domain.SeverityInfo, domain.EvidenceConfirmed, healthyClaude.Snapshots[1], nil), w0(healthyClaude.Snapshots[1], 0)),
			withWindowFact(alert(domain.AlertQuotaExhausted, domain.SeverityUrgent, domain.EvidenceConfirmed, fullSnap, nil), w0(fullSnap, 0)),
			alert(domain.AlertCredential, domain.SeverityUrgent, domain.EvidenceConfirmed, badSnap, nil))))

	rlAlert := func(kind domain.AlertKind, sev domain.Severity, snap domain.QuotaSnapshot, model string, facts ...string) domain.Alert {
		a := domain.Alert{Kind: kind, Severity: sev, Evidence: domain.EvidenceConfirmed, Credential: snap.Credential, OccurredAt: gen, Facts: facts, Scope: domain.ScopeAccount}
		if model != "" {
			a.Scope, a.ScopeID = domain.ScopeModel, model
		}
		return a
	}
	recoverAt := gen.Add(12 * time.Minute).UTC().Format(time.RFC3339)
	add("告警", "告警 · CPA 限流冷却中 (<30m)（仅渲染边界 / 不会线上推送）", "仅渲染边界 / 不会线上推送：CPA 30 分钟内的日常冷却只计入日报",
		"告知调度系统已自动熔断分流，用户无需人工介入",
		"无配额窗口：核心展示预计恢复时间点（14:42）与 429 状态码，底部说明自动分流机制",
		"完整告警卡",
		r.Card(alertMsg(repHealthy,
			rlAlert(domain.AlertRateLimited, domain.SeverityWarn, resetSnap, "claude-sonnet-4-5",
				domain.FactPrefixStatus+" 429", domain.FactPrefixRecovery+" "+recoverAt, domain.FactPrefixDuration+" 5m"))))

	far := gen.Add(3 * time.Hour).UTC().Format(time.RFC3339)
	add("告警", "告警 · 账号级较长冷却 (>30m)（可能等待上游刷新）", "冷却超过 30 分钟 = 上游给了刷新时间，审慎定性为较长暂停；1 分钟内发现",
		"快速识别事实上的较长暂停，按审慎定性提示，避免用户误判全账号永久报废",
		"聚焦恢复时间点（17:30），注明可能等待上游刷新，次级行说明其余号可用余量",
		"完整告警卡",
		r.Card(alertMsg(repHealthy,
			rlAlert(domain.AlertRateLimited, domain.SeverityWarn, resetSnap, "",
				domain.FactPrefixStatus+" 429", domain.FactPrefixRecovery+" "+far, domain.FactPrefixDuration+" 0m"))))

	add("告警", "告警 · 限流已解除", "之前告警过的限流恢复",
		"流量重均衡广播，确认该号已重新承接生产请求",
		"无配额数值：绿色「已恢复」，注明停用时长",
		"完整告警卡",
		r.Card(alertMsg(repHealthy,
			rlAlert(domain.AlertRateLimitCleared, domain.SeverityInfo, resetSnap, "",
				domain.FactPrefixStatus+" 429", domain.FactPrefixDuration+" 17m"))))

	digest := *repHealthy
	digest.RateLimits = []domain.RateLimitTally{
		{Credential: resetSnap.Credential, Count: 3, Longest: 4 * time.Minute},
		{Credential: ag.Snapshots[1].Credential, Count: 1, Longest: 50 * time.Second},
	}
	add("查询 / 日报", "定时日报 · 含短暂限流汇总", "没满 5 分钟的限流只在日报里汇总",
		"防狼来了，过滤掉日常秒级/分级限流，仅在日报中汇总统计",
		"首屏渠道展示 5h+7d，尾部附加独立限流统计块（次数、最长时长）",
		"完整卡",
		r.Card(domain.Message{Title: "额度日报", Kind: string(domain.AlertBootstrap), Report: &digest}))

	add("兜底降级", "兜底 · 简版告警卡（用满+失效）", "完整卡被拒时的告警",
		"完整卡遭飞书 2.0 校验拦截时无损降级改发；剥离 interactive_container/panel/chart",
		"保持与完整卡相同的告警信息要素，纯 column_set 渲染，无交互行为",
		"兜底简版卡",
		r.CardSimple(alertMsg(repBad,
			alert(domain.AlertQuotaExhausted, domain.SeverityUrgent, domain.EvidenceConfirmed, fullSnap, w0(fullSnap, 0)),
			alert(domain.AlertCredential, domain.SeverityUrgent, domain.EvidenceConfirmed, badSnap, nil))))

	add("兜底降级", "兜底 · 简版查询卡（用满+失效）", "无折叠面板、无图表；用「用满 + 凭证失效」的数据",
		"严苛网络/飞书客户端限制下的保底查询卡",
		"各渠道保持 5h/7d 核心指标，异常号直出，去除折叠面板以符规范",
		"兜底简版卡",
		r.CardSimple(q(repBad)))

	add("兜底降级", "兜底 · 简版查询卡（全渠道多账号）", "全量多账号健康状态在简版卡下的表现",
		"验证垂直 5h/7d facts 结构在简版卡（无 interactive_container）下的渲染稳定性",
		"全覆盖：垂直 facts 行在纯 column_set 下完美居中与展示，绝无横向溢出",
		"兜底简版卡",
		r.CardSimple(q(repHealthy)))

	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	html := strings.Replace(galleryHTML, "/*CARDS*/null", string(data), 1)
	if out == "" {
		out = "card-gallery.html"
	}
	tmpOut := filepath.Join(os.TempDir(), "opencode", "all-card-variants.html")
	if err := os.MkdirAll(filepath.Dir(tmpOut), 0o755); err == nil {
		_ = os.WriteFile(tmpOut, []byte(html), 0o644)
	}
	if err := os.WriteFile(out, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d cards to %s", len(entries), out)
}

func stateRankForGallery(s domain.CredentialState) int {
	switch s {
	case domain.StateHealthy:
		return 0
	case domain.StateLimited:
		return 1
	case domain.StateNotice:
		return 2
	case domain.StateWarning:
		return 3
	case domain.StateStale, domain.StateUnknown:
		return 4
	case domain.StateSuspect:
		return 5
	case domain.StateExhausted:
		return 6
	case domain.StateInvalid:
		return 7
	}
	return 0
}

func severityRankForGallery(s domain.Severity) int {
	switch s {
	case domain.SeverityUrgent:
		return 3
	case domain.SeverityWarn:
		return 2
	case domain.SeverityInfo:
		return 1
	}
	return 0
}

// galleryHTML is a high-fidelity visual simulator of the Feishu 2.0 client,
// displaying cards within a 375px mobile viewport frame with light/dark theme toggle,
// category filtering, primary intent annotations, and 5h/7d quota coverage analysis.
const galleryHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>CPA-Manager 消息卡片全矩阵画廊 · 375px 真机全景</title>
<style>
:root {
  --bg-page: #f0f2f5;
  --bg-card: #ffffff;
  --bg-meta: #f7f8fa;
  --text-main: #1f2329;
  --text-sub: #646a73;
  --text-dim: #8f959e;
  --border-light: #dee0e3;
  --border-card: rgba(0,0,0,0.06);
  --shadow-card: 0 4px 16px rgba(31,35,41,0.08), 0 1px 3px rgba(31,35,41,0.04);
  --accent-blue: #3370ff;
  --accent-orange: #de7802;
  --accent-green: #2ea121;
  --accent-red: #d83931;
}

body.dark {
  --bg-page: #111317;
  --bg-card: #1c1f24;
  --bg-meta: #252930;
  --text-main: #ebeef5;
  --text-sub: #a6abb2;
  --text-dim: #6e737d;
  --border-light: #323740;
  --border-card: rgba(255,255,255,0.08);
  --shadow-card: 0 4px 20px rgba(0,0,0,0.4);
}

* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--bg-page);
  font: 14px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  color: var(--text-main);
  transition: background 0.2s ease, color 0.2s ease;
}

header.gallery-header {
  position: sticky;
  top: 0;
  z-index: 100;
  background: var(--bg-card);
  border-bottom: 1px solid var(--border-light);
  padding: 16px 28px;
  box-shadow: 0 2px 8px rgba(0,0,0,0.04);
}
.header-wrap {
  max-width: 1680px;
  margin: 0 auto;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
}
.header-titles h1 {
  font-size: 20px;
  font-weight: 700;
  margin: 0 0 4px;
  display: flex;
  align-items: center;
  gap: 10px;
}
.header-badge {
  font-size: 11px;
  font-weight: 600;
  background: #feead2;
  color: #de7802;
  padding: 2px 8px;
  border-radius: 12px;
  vertical-align: middle;
}
body.dark .header-badge {
  background: #4a3018;
  color: #ffaa55;
}
.header-titles .desc {
  font-size: 12px;
  color: var(--text-sub);
  margin: 0;
}

.controls {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.filter-tabs {
  display: inline-flex;
  background: var(--bg-meta);
  padding: 3px;
  border-radius: 8px;
  border: 1px solid var(--border-light);
}
.tab-btn {
  background: none;
  border: none;
  padding: 5px 12px;
  font-size: 13px;
  border-radius: 6px;
  cursor: pointer;
  color: var(--text-sub);
  font-weight: 500;
  transition: all 0.15s ease;
}
.tab-btn:hover { color: var(--text-main); }
.tab-btn.active {
  background: var(--bg-card);
  color: var(--text-main);
  box-shadow: 0 1px 3px rgba(0,0,0,0.1);
  font-weight: 600;
}
.toggle-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  background: var(--bg-meta);
  border: 1px solid var(--border-light);
  color: var(--text-main);
  padding: 6px 14px;
  border-radius: 8px;
  cursor: pointer;
  font-size: 13px;
  font-weight: 500;
}

main.gallery-content {
  max-width: 1680px;
  margin: 0 auto;
  padding: 24px 28px 60px;
}

.group-section {
  margin-bottom: 40px;
}
.group-title {
  font-size: 18px;
  font-weight: 700;
  margin: 0 0 16px;
  display: flex;
  align-items: center;
  gap: 10px;
  padding-bottom: 8px;
  border-bottom: 2px solid var(--border-light);
}
.group-count {
  font-size: 12px;
  font-weight: 500;
  color: var(--text-dim);
  background: var(--bg-meta);
  padding: 2px 8px;
  border-radius: 10px;
}

.cards-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(375px, 1fr));
  gap: 28px;
  justify-content: start;
}
@media (min-width: 1280px) {
  .cards-grid { grid-template-columns: repeat(3, 375px); }
}
@media (min-width: 1650px) {
  .cards-grid { grid-template-columns: repeat(4, 375px); }
}

.variant-card {
  width: 375px;
  display: flex;
  flex-direction: column;
}

.card-meta-box {
  background: var(--bg-card);
  border: 1px solid var(--border-light);
  border-bottom: none;
  border-radius: 12px 12px 0 0;
  padding: 12px 14px 10px;
}
.meta-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 6px;
}
.meta-header h3 {
  font-size: 14px;
  font-weight: 700;
  margin: 0;
  color: var(--text-main);
  line-height: 1.4;
}
.mode-badge {
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 4px;
  white-space: nowrap;
  font-weight: 600;
}
.mode-full {
  background: #e1eaff;
  color: #245bdb;
}
body.dark .mode-full {
  background: #1e2c4a;
  color: #88b0ff;
}
.mode-simple {
  background: #eff0f1;
  color: #646a73;
}
body.dark .mode-simple {
  background: #2b2e36;
  color: #a6abb2;
}

.meta-field {
  font-size: 12px;
  line-height: 1.45;
  margin: 4px 0;
  color: var(--text-sub);
}
.meta-field strong {
  color: var(--text-main);
}
.meta-cov {
  background: var(--bg-meta);
  border-left: 3px solid #3370ff;
  padding: 4px 8px;
  border-radius: 0 4px 4px 0;
  margin-top: 6px;
  font-size: 11.5px;
}

/* 375px Mobile Frame Simulator */
.phone-frame {
  width: 375px;
  background: var(--bg-card);
  border: 1px solid var(--border-light);
  border-radius: 0 0 12px 12px;
  box-shadow: var(--shadow-card);
  overflow: hidden;
  position: relative;
}

/* Feishu Card 2.0 Mock */
.fs-card {
  width: 100%;
  background: var(--bg-card);
  color: var(--text-main);
}
.fs-hd {
  padding: 12px 16px;
  border-bottom: 1px solid transparent;
}
.fs-hd .t {
  font-size: 16px;
  font-weight: 600;
  letter-spacing: -0.2px;
  line-height: 1.4;
}
.fs-hd .st {
  font-size: 12px;
  opacity: 0.85;
  margin-top: 2px;
}
.fs-hd .tags { margin-top: 4px; }

.fs-bd { padding: 12px 16px 16px; }

.md {
  word-break: break-word;
  line-height: 1.5;
}
.md code {
  background: #eff0f1;
  border-radius: 3px;
  padding: 1px 4px;
  font-family: inherit;
  font-size: 12px;
  color: #1f2329;
}
body.dark .md code {
  background: #2b2f36;
  color: #ebeef5;
}

.tag {
  display: inline-block;
  font-size: 11px;
  font-weight: 500;
  line-height: 17px;
  padding: 0 6px;
  border-radius: 4px;
  margin: 0 2px;
  vertical-align: 1px;
}

.cs {
  display: flex;
  align-items: stretch;
}
.cs.flow {
  flex-wrap: wrap;
  gap: 8px;
}
.col { min-width: 0; }

.btns {
  display: flex;
  gap: 8px;
  margin: 12px 0 6px;
}
.btn {
  border: 1px solid #d0d3d6;
  border-radius: 6px;
  padding: 5px 14px;
  background: var(--bg-card);
  color: var(--text-main);
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  transition: all 0.15s ease;
}
.btn.primary {
  background: #3370ff;
  border-color: #3370ff;
  color: #ffffff;
}
body.dark .btn {
  border-color: #424752;
  background: #262930;
  color: #ebeef5;
}

details.panel {
  border-top: 1px solid var(--border-light);
  margin-top: 10px;
}
details.panel > summary {
  cursor: pointer;
  color: #3370ff;
  padding: 8px 0;
  list-style: none;
  font-size: 13px;
  font-weight: 500;
  display: flex;
  align-items: center;
  gap: 4px;
}
details.panel[open] > summary { color: var(--text-dim); }

hr {
  border: 0;
  border-top: 1px solid var(--border-light);
  margin: 10px 0;
}

.json-inspect {
  margin-top: 6px;
  font-size: 12px;
}
.json-inspect summary {
  cursor: pointer;
  color: var(--text-dim);
  font-size: 11px;
  padding: 4px 0;
}
.json-inspect pre {
  font-size: 11px;
  max-height: 240px;
  overflow: auto;
  background: var(--bg-meta);
  border: 1px solid var(--border-light);
  border-radius: 6px;
  padding: 8px;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 4px 0 0;
}

.raw { color: #d83931; font-size: 12px; }
</style>
</head>
<body>
<header class="gallery-header">
  <div class="header-wrap">
    <div class="header-titles">
      <h1>CPA-Manager 卡片矩阵视觉总览 <span class="header-badge">375px 窄屏适配</span></h1>
      <p class="desc">全量 25 种卡片变体 · 5h/7d 首屏覆盖决策 · Claude 专属橙色品牌保留 · 飞书 2.0 规范</p>
    </div>
    <div class="controls">
      <div class="filter-tabs">
        <button class="tab-btn active" data-filter="all">全部 (25)</button>
        <button class="tab-btn" data-filter="查询 / 日报">查询 / 日报 (8)</button>
        <button class="tab-btn" data-filter="告警">告警通知 (14)</button>
        <button class="tab-btn" data-filter="兜底降级">兜底降级 (3)</button>
      </div>
      <button class="toggle-btn" id="dark-toggle">🌓 深色模式</button>
    </div>
  </div>
</header>

<main class="gallery-content">
  <div id="root"></div>
</main>

<script>
var CARDS = /*CARDS*/null;
var FONT = {
  violet:'#7f3bf5', blue:'#245bdb', turquoise:'#078372', indigo:'#4954e6',
  purple:'#8d55ed', red:'#d83931', orange:'#de7802', green:'#2ea121',
  grey:'#8f959e', neutral:'#646a73', yellow:'#ad8b00', wathet:'#1f8ec5',
  lime:'#669900', carmine:'#c71b67'
};
var TAG = {
  neutral:['#eff0f1','#646a73'], blue:['#e1eaff','#245bdb'], turquoise:['#d5f6f2','#078372'],
  lime:['#eef6c6','#669900'], orange:['#feead2','#de7802'], violet:['#ece2fe','#7f3bf5'],
  indigo:['#e0e9ff','#4954e6'], wathet:['#d9f3fd','#1f8ec5'], green:['#d9f5d6','#2ea121'],
  yellow:['#faf1d1','#ad8b00'], red:['#fde2e2','#d83931'], purple:['#efe6fe','#8d55ed'],
  carmine:['#fde0ec','#c71b67']
};
var TAG_DARK = {
  neutral:['#2b2f36','#a6abb2'], blue:['#1e2c4a','#88b0ff'], turquoise:['#173d36','#4cd3be'],
  lime:['#2a3b17','#a3d944'], orange:['#4a3018','#ffaa55'], violet:['#352054','#bc99fc'],
  indigo:['#242a59','#949eff'], wathet:['#1a384d','#69c8f5'], green:['#1d3a1f','#6dd463'],
  yellow:['#3e3518','#fad866'], red:['#4a1f1f','#f77974'], purple:['#3b2354','#c79eff'],
  carmine:['#45192c','#f56ea4']
};
var HEAD = {
  blue:['#e1eaff','#1f2329'], wathet:['#d9f3fd','#1f2329'], turquoise:['#d5f6f2','#1f2329'],
  green:['#d9f5d6','#1f2329'], yellow:['#faf1d1','#1f2329'], orange:['#feead2','#1f2329'],
  red:['#fde2e2','#1f2329'], carmine:['#fde0ec','#1f2329'], violet:['#ece2fe','#1f2329'],
  purple:['#efe6fe','#1f2329'], indigo:['#e0e9ff','#1f2329'], grey:['#eff0f1','#1f2329'],
  'default':['#ffffff','#1f2329']
};
var HEADD = {
  blue:'#1e2c4a', wathet:'#1a384d', turquoise:'#173d36', green:'#1d3a1f',
  yellow:'#3e3518', orange:'#4a3018', red:'#4a1f1f', carmine:'#45192c',
  violet:'#352054', purple:'#3b2354', indigo:'#242a59', grey:'#2b2f36',
  'default':'#23262d'
};
var BG = {
  'grey-50':'#f8f9fa', 'grey-100':'#f5f6f7', 'grey-200':'#eff0f1',
  'red-50':'#fef1f1', 'red-100':'#fde2e2', 'default':'transparent'
};
var BGD = {
  'grey-50':'#21242b', 'grey-100':'#282c34', 'grey-200':'#313640',
  'red-50':'#3b1b1a', 'red-100':'#52201e', 'default':'transparent'
};
var SIZE = {
  'heading-1':'22px','heading-2':'19px','heading-3':'17px','heading-4':'15px',
  'heading':'16px','normal':'14px','notation':'12px','xxxx-large':'24px',
  'xxx-large':'22px','xx-large':'20px','x-large':'18px','large':'15px',
  'medium':'14px','small':'12px','x-small':'11px'
};

function esc(s){ return String(s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
function tagHTML(color,text){
  var map = DARK ? TAG_DARK : TAG;
  var c = map[color] || map.neutral;
  return '<span class="tag" style="background:'+c[0]+';color:'+c[1]+'">'+esc(text)+'</span>';
}
function mdHTML(s){
  s = esc(s);
  s = s.replace(/&lt;font color='([a-z-]+)'&gt;/g, function(_,c){
    return '<span style="color:'+(FONT[c]||c)+'">';
  }).replace(/&lt;\/font&gt;/g,'</span>');
  s = s.replace(/&lt;text_tag color='([a-z-]+)'&gt;([\s\S]*?)&lt;\/text_tag&gt;/g, function(_,c,t){
    return tagHTML(c, t);
  });
  s = s.replace(/\x60([^\x60]+)\x60/g,'<code>$1</code>');
  s = s.replace(/\*\*([\s\S]+?)\*\*/g,'<b>$1</b>');
  s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g,'<a href="$2" target="_blank" style="color:#3370ff;text-decoration:none;">$1</a>');
  return s.replace(/\n/g,'<br>');
}
function box(el,css){ var d=document.createElement('div'); if(css) d.style.cssText=css; return d; }
function render(els, parent){ (els||[]).forEach(function(e){ parent.appendChild(one(e)); }); }

function one(e){
  var d;
  switch(e.tag){
  case 'markdown':
    d = box(); d.className = 'md';
    d.style.fontSize = SIZE[e.text_size] || '14px';
    if(/heading/.test(e.text_size||'')) d.style.fontWeight = '600';
    if(e.text_align) d.style.textAlign = e.text_align;
    if(e.margin) d.style.margin = e.margin;
    d.innerHTML = mdHTML(e.content||'');
    return d;
  case 'div':
    d = box(); d.className = 'md';
    d.innerHTML = mdHTML((e.text && e.text.content) || '');
    return d;
  case 'hr': return document.createElement('hr');
  case 'column_set':
    d = box(); d.className = 'cs';
    if(e.flex_mode === 'flow') d.className += ' flow';
    if(e.margin) d.style.margin = e.margin;
    if(e.background_style) d.style.background = (DARK?BGD:BG)[e.background_style] || '';
    var btnRow = (e.columns||[]).every(function(c){
      return (c.elements||[]).every(function(x){ return x.tag==='button'; });
    });
    if(btnRow) d.className += ' btns';
    (e.columns||[]).forEach(function(c){
      var cd = box(); cd.className = 'col';
      if(c.width==='weighted'){ cd.style.flex = (c.weight||1) + ' 1 0'; }
      else if(e.flex_mode === 'flow') { cd.style.flex = '1 1 120px'; }
      else { cd.style.flex = '0 0 auto'; }
      if(c.background_style) cd.style.background = (DARK?BGD:BG)[c.background_style] || '';
      if(c.padding) cd.style.padding = c.padding;
      if(c.margin) cd.style.margin = c.margin;
      cd.style.display = 'flex';
      cd.style.flexDirection = 'column';
      cd.style.justifyContent = c.vertical_align==='center'?'center':(c.vertical_align==='bottom'?'flex-end':'flex-start');
      render(c.elements, cd);
      d.appendChild(cd);
    });
    return d;
  case 'interactive_container':
    d = box();
    var bg = (DARK?BGD:BG)[e.background_style] || '';
    d.style.cssText = 'background:'+bg+';border-radius:'+(e.corner_radius||0)+';padding:'+(e.padding||'')+';margin:'+(e.margin||'')+';border:1px solid '+(DARK?'rgba(255,255,255,0.06)':'rgba(0,0,0,0.04)')+';';
    render(e.elements, d);
    return d;
  case 'collapsible_panel':
    d = document.createElement('details');
    d.className = 'panel';
    if(e.expanded) d.open = true;
    var s = document.createElement('summary');
    s.textContent = '▸ ' + ((e.header && e.header.title && e.header.title.content) || '展开明细');
    d.appendChild(s);
    render(e.elements, d);
    return d;
  case 'button':
    d = document.createElement('button');
    d.className = 'btn' + (e.type && /primary/.test(e.type) ? ' primary' : '');
    d.textContent = (e.text && e.text.content) || '';
    return d;
  case 'text_tag':
    d = document.createElement('span');
    d.innerHTML = tagHTML(e.color, (e.text && e.text.content) || '');
    return d;
  default:
    d = box(); d.className = 'raw';
    d.textContent = '[未解析组件: ' + e.tag + ']';
    return d;
  }
}

function cardEl(card){
  var c = box(); c.className = 'fs-card';
  var h = card.header || {};
  var tpl = h.template || 'default';
  var hdColor = DARK ? (HEADD[tpl]||HEADD['default']) : ((HEAD[tpl]||HEAD['default'])[0]);
  var txtColor = DARK ? '#ebeef5' : ((HEAD[tpl]||HEAD['default'])[1]);
  var hd = box('', 'background:'+hdColor+';color:'+txtColor);
  hd.className = 'fs-hd';
  hd.innerHTML = '<div class="t">' + esc((h.title && h.title.content) || '') + '</div>' +
    (h.subtitle ? '<div class="st">' + esc(h.subtitle.content) + '</div>' : '') +
    (h.text_tag_list ? '<div class="tags">' + h.text_tag_list.map(function(t){ return tagHTML(t.color, t.text.content); }).join('') + '</div>' : '');
  c.appendChild(hd);
  var bd = box(); bd.className = 'fs-bd';
  render((card.body || {}).elements, bd);
  c.appendChild(bd);
  return c;
}

var DARK = false;
var ACTIVE_FILTER = 'all';

function draw(){
  var root = document.getElementById('root');
  root.innerHTML = '';
  var groups = {};
  CARDS.forEach(function(item){
    if(ACTIVE_FILTER !== 'all' && item.Group !== ACTIVE_FILTER) return;
    if(!groups[item.Group]) groups[item.Group] = [];
    groups[item.Group].push(item);
  });

  Object.keys(groups).forEach(function(groupName){
    var list = groups[groupName];
    var sec = box(); sec.className = 'group-section';
    var title = document.createElement('h2');
    title.className = 'group-title';
    title.innerHTML = esc(groupName) + ' <span class="group-count">' + list.length + ' 张</span>';
    sec.appendChild(title);

    var grid = box(); grid.className = 'cards-grid';
    list.forEach(function(x, idx){
      var vcard = box(); vcard.className = 'variant-card';

      // Meta Box
      var meta = box(); meta.className = 'card-meta-box';
      var mhdr = box(); mhdr.className = 'meta-header';
      mhdr.innerHTML = '<h3>' + esc(x.Name) + '</h3>' +
        '<span class="mode-badge ' + (/简版/.test(x.Mode)?'mode-simple':'mode-full') + '">' + esc(x.Mode) + '</span>';
      meta.appendChild(mhdr);

      var pDesc = box(); pDesc.className = 'meta-field';
      pDesc.innerHTML = '<strong>场景说明：</strong>' + esc(x.Desc);
      meta.appendChild(pDesc);

      if(x.Intent){
        var pIntent = box(); pIntent.className = 'meta-field';
        pIntent.innerHTML = '<strong>业务意图：</strong>' + esc(x.Intent);
        meta.appendChild(pIntent);
      }
      if(x.Coverage){
        var pCov = box(); pCov.className = 'meta-field meta-cov';
        pCov.innerHTML = '<strong>首屏 5h/7d 决策：</strong>' + esc(x.Coverage);
        meta.appendChild(pCov);
      }
      vcard.appendChild(meta);

      // Phone Frame
      var phone = box(); phone.className = 'phone-frame';
      phone.appendChild(cardEl(x.Card));

      // JSON Inspector
      var j = document.createElement('details');
      j.className = 'json-inspect';
      j.innerHTML = '<summary>▸ 查看卡片 JSON 结构</summary><pre>' + esc(JSON.stringify(x.Card, null, 2)) + '</pre>';
      phone.appendChild(j);

      vcard.appendChild(phone);
      grid.appendChild(vcard);
    });

    sec.appendChild(grid);
    root.appendChild(sec);
  });
}

document.querySelectorAll('.tab-btn').forEach(function(btn){
  btn.onclick = function(){
    document.querySelectorAll('.tab-btn').forEach(function(b){ b.classList.remove('active'); });
    btn.classList.add('active');
    ACTIVE_FILTER = btn.getAttribute('data-filter');
    draw();
  };
});

document.getElementById('dark-toggle').onclick = function(){
  DARK = !DARK;
  document.body.classList.toggle('dark', DARK);
  this.textContent = DARK ? '☀️ 浅色模式' : '🌓 深色模式';
  draw();
};

draw();
</script>
</body>
</html>
`
