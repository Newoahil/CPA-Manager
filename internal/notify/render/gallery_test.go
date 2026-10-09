package render

import (
	"encoding/json"
	"os"
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
	repHealthy := &domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{healthyClaude, ag, healthyCodex}}

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
	agScoped := ag.Snapshots[0]

	type entry struct {
		Group, Name, Desc string
		Card              map[string]any
	}
	var entries []entry
	add := func(group, name, desc string, card map[string]any) {
		entries = append(entries, entry{group, name, desc, card})
	}

	q := func(rep *domain.Report) domain.Message {
		return domain.Message{Kind: KindQuery, Report: rep, Freshness: "实时"}
	}
	add("查询 / 日报", "查询 · 全部正常", "@机器人 额度；所有渠道都有余量", r.Card(q(repHealthy)))
	add("查询 / 日报", "查询 · 有号接近上限", "Claude External0.2 的 5 小时窗口只剩 6.5%", r.Card(q(repNotice)))
	add("查询 / 日报", "查询 · 用满 + 凭证失效", "Claude 一个号 5h 用满；Codex 一个号 401 失效（显示「去 CPA」按钮）", r.Card(q(repBad)))
	add("查询 / 日报", "查询 · 有号数据过期", "Claude External 连续取不到额度（上游 429）", r.Card(q(repStale)))
	cached := q(repDegraded)
	cached.Freshness = "缓存 · 约 40 分钟前"
	cached.Notice = "实时采集失败，以下是 40 分钟前的数据"
	add("查询 / 日报", "查询 · 实时采集失败，用旧数据", "CPA 暂时连不上，回退到缓存并显著标注", r.Card(cached))
	single := q(&domain.Report{GeneratedAt: gen, Providers: []domain.ProviderReport{noticeClaude}})
	single.Detailed = true
	add("查询 / 日报", "查询 · 单渠道（@机器人 claude）", "只看一个渠道时展开窗口明细", r.Card(single))
	add("查询 / 日报", "日报（10:00 / 17:00）", "定时日报，和查询同一版式", r.Card(domain.Message{Title: "额度日报", Kind: string(domain.AlertBootstrap), Report: repHealthy}))

	add("告警", "额度已刷新", "一个号的 5h 窗口刷新", r.Card(alertMsg(repHealthy,
		withWindowFact(alert(domain.AlertQuotaReset, domain.SeverityInfo, domain.EvidenceConfirmed, resetSnap, nil), w0(resetSnap, 0)))))
	add("告警", "接近上限（90% / 95%）", "已用超过阈值；即便严重度为 Info，标题仍按告警类型显示橙色", r.Card(alertMsg(repNotice,
		alert(domain.AlertQuotaThreshold, domain.SeverityInfo, domain.EvidenceConfirmed, warnSnap, w0(warnSnap, 0)))))
	add("告警", "已用满", "附恢复时间和建议", r.Card(alertMsg(repBad,
		withWindowFact(alert(domain.AlertQuotaExhausted, domain.SeverityUrgent, domain.EvidenceConfirmed, fullSnap, nil), w0(fullSnap, 0)))))
	add("告警", "凭证失效（确认）", "上游明确 401，显示错误码和「去 CPA」", r.Card(alertMsg(repBad,
		alert(domain.AlertCredential, domain.SeverityUrgent, domain.EvidenceConfirmed, badSnap, nil))))
	add("告警", "疑似凭证失效", "证据不足时标「疑似」", r.Card(alertMsg(repBad,
		alert(domain.AlertCredential, domain.SeverityWarn, domain.EvidenceSuspected, badSnap, nil))))
	add("告警", "疑似异常", "连续 3 次取不到额度", r.Card(alertMsg(repStale,
		alert(domain.AlertSuspect, domain.SeverityWarn, domain.EvidenceSuspected, staleSnap, nil))))
	add("告警", "数据过期", "连续 2 次取不到额度", r.Card(alertMsg(repStale,
		alert(domain.AlertStale, domain.SeverityInfo, domain.EvidenceConfirmed, staleSnap, nil))))
	add("告警", "已恢复", "之前异常的号恢复正常", r.Card(alertMsg(repHealthy,
		withWindowFact(alert(domain.AlertRecovered, domain.SeverityInfo, domain.EvidenceConfirmed, resetSnap, nil), w0(resetSnap, 0)))))
	add("告警", "分组范围（只影响某个模型组）", "Antigravity 的 Gemini 组接近上限，不代表整个号", r.Card(alertMsg(repHealthy,
		withWindowFact(alert(domain.AlertQuotaThreshold, domain.SeverityWarn, domain.EvidenceConfirmed, agScoped, nil), w0(agScoped, 0)))))
	add("告警", "同一轮多条变化合并", "一张卡，按严重程度排序，每号一行", r.Card(alertMsg(repBad,
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
	add("告警", "限流冷却中（仅当 COOLDOWN_ALERT_AFTER 调低时出现）", "默认不推送：CPA 30 分钟内的日常冷却只计入日报", r.Card(alertMsg(repHealthy,
		rlAlert(domain.AlertRateLimited, domain.SeverityWarn, resetSnap, "claude-sonnet-4-5",
			domain.FactPrefixStatus+" 429", domain.FactPrefixRecovery+" "+recoverAt, domain.FactPrefixDuration+" 5m"))))
	far := gen.Add(3 * time.Hour).UTC().Format(time.RFC3339)
	add("告警", "额度用满（CPA 冷却到上游刷新时间）", "冷却超过 30 分钟 = 上游给了刷新时间，视为用满；1 分钟内发现", r.Card(alertMsg(repHealthy,
		rlAlert(domain.AlertRateLimited, domain.SeverityWarn, resetSnap, "",
			domain.FactPrefixStatus+" 429", domain.FactPrefixRecovery+" "+far, domain.FactPrefixDuration+" 0m"))))
	add("告警", "限流已解除", "之前告警过的限流恢复", r.Card(alertMsg(repHealthy,
		rlAlert(domain.AlertRateLimitCleared, domain.SeverityInfo, resetSnap, "",
			domain.FactPrefixStatus+" 429", domain.FactPrefixDuration+" 17m"))))
	digest := *repHealthy
	digest.RateLimits = []domain.RateLimitTally{
		{Credential: resetSnap.Credential, Count: 3, Longest: 4 * time.Minute},
		{Credential: ag.Snapshots[1].Credential, Count: 1, Longest: 50 * time.Second},
	}
	add("查询 / 日报", "日报 · 含短暂限流汇总", "没满 5 分钟的限流只在日报里汇总", r.Card(domain.Message{Title: "额度日报", Kind: string(domain.AlertBootstrap), Report: &digest}))

	add("兜底", "简版卡 · 告警", "完整卡被拒时的告警", r.CardSimple(alertMsg(repBad,
		alert(domain.AlertQuotaExhausted, domain.SeverityUrgent, domain.EvidenceConfirmed, fullSnap, w0(fullSnap, 0)),
		alert(domain.AlertCredential, domain.SeverityUrgent, domain.EvidenceConfirmed, badSnap, nil))))
	add("兜底", "简版卡（完整卡被飞书拒绝时自动改发）", "无折叠面板、无图表；用「用满 + 凭证失效」的数据", r.CardSimple(q(repBad)))

	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	html := strings.Replace(galleryHTML, "/*CARDS*/null", string(data), 1)
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

// galleryHTML is an approximation of the Feishu client, good enough to judge
// layout and wording. Colours are Feishu's light-theme palette.
const galleryHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>CPA-buddy 卡片样式一览</title>
<style>
body{margin:0;background:#f2f3f5;font:14px/1.6 -apple-system,"PingFang SC","Microsoft YaHei",sans-serif;color:#1f2329}
h1{font-size:20px;margin:24px 24px 4px}
.lead{margin:0 24px 8px;color:#646a73}
h2{font-size:16px;margin:28px 24px 12px;color:#1f2329}
.grid{display:flex;flex-wrap:wrap;gap:24px;padding:0 24px}
.item{width:375px}
.item h3{font-size:14px;margin:0 0 2px}
.item p{margin:0 0 8px;color:#646a73;font-size:12px}
.card{background:#fff;border-radius:8px;box-shadow:0 1px 4px rgba(0,0,0,.08);overflow:hidden}
.hd{padding:12px 16px}
.hd .t{font-size:16px;font-weight:600}
.hd .st{font-size:12px;opacity:.8}
.hd .tags{margin-top:4px}
.bd{padding:12px 16px}
.md{word-break:break-word}
.md code{background:#f2f3f5;border-radius:3px;padding:0 3px;font-family:inherit;font-size:13px}
.tag{display:inline-block;font-size:12px;line-height:18px;padding:0 6px;border-radius:4px;margin:0 2px;vertical-align:1px}
.cs{display:flex;align-items:stretch}
.col{min-width:0}
.btns{display:flex;gap:8px;margin:12px 0}
.btn{border:1px solid #d0d3d6;border-radius:6px;padding:4px 12px;background:#fff;font-size:14px}
.btn.primary{background:#3370ff;border-color:#3370ff;color:#fff}
details.panel{border-top:1px solid #dee0e3;margin-top:8px}
details.panel>summary{cursor:pointer;color:#3370ff;padding:8px 0;list-style:none}
hr{border:0;border-top:1px solid #dee0e3;margin:8px 0}
pre{font-size:11px;max-height:280px;overflow:auto;background:#fafafa;padding:8px;white-space:pre-wrap;word-break:break-all}
.json summary{font-size:12px;color:#8f959e;cursor:pointer;margin-top:6px}
.raw{color:#d83931;font-size:12px}
body.dark{background:#141414;color:#e6e6e6}body.dark .card{background:#292929;box-shadow:none}body.dark .lead,body.dark .item p{color:#a6a6a6}body.dark h1,body.dark h2,body.dark .item h3{color:#e6e6e6}body.dark .md code{background:#3a3a3a}body.dark .btn{background:#2f2f2f;color:#e6e6e6;border-color:#4a4a4a}body.dark pre{background:#1d1d1d;color:#ccc}
</style></head><body>
<h1>CPA-buddy 卡片样式一览</h1>
<div class="lead">每张卡都由线上同一份渲染代码生成，数据为示例。浏览器只是近似还原飞书客户端，深色模式和换行以真机为准。点「卡片 JSON」可看实际发出的内容。</div>
<div class="lead"><label><input type="checkbox" id="dark"> 深色模式（近似飞书深色）</label></div><div id="root"></div>
<script>
var CARDS = /*CARDS*/null;
var FONT = {violet:'#7f3bf5',blue:'#245bdb',turquoise:'#078372',indigo:'#4954e6',purple:'#8d55ed',red:'#d83931',orange:'#de7802',green:'#2ea121',grey:'#8f959e',neutral:'#646a73',yellow:'#ad8b00',wathet:'#1f8ec5',lime:'#669900',carmine:'#c71b67'};
var TAG = {neutral:['#eff0f1','#646a73'],blue:['#e1eaff','#245bdb'],turquoise:['#d5f6f2','#078372'],lime:['#eef6c6','#669900'],orange:['#feead2','#de7802'],violet:['#ece2fe','#7f3bf5'],indigo:['#e0e9ff','#4954e6'],wathet:['#d9f3fd','#1f8ec5'],green:['#d9f5d6','#2ea121'],yellow:['#faf1d1','#ad8b00'],red:['#fde2e2','#d83931'],purple:['#efe6fe','#8d55ed'],carmine:['#fde0ec','#c71b67']};
var HEAD = {blue:['#e1eaff','#1f2329'],wathet:['#d9f3fd','#1f2329'],turquoise:['#d5f6f2','#1f2329'],green:['#d9f5d6','#1f2329'],yellow:['#faf1d1','#1f2329'],orange:['#feead2','#1f2329'],red:['#fde2e2','#1f2329'],carmine:['#fde0ec','#1f2329'],violet:['#ece2fe','#1f2329'],purple:['#efe6fe','#1f2329'],indigo:['#e0e9ff','#1f2329'],grey:['#eff0f1','#1f2329'],'default':['#fff','#1f2329']};
var BGD = {'grey-50':'#1a1a1a','grey-100':'#292929','grey-200':'#333333','red-50':'#3d1a19','red-100':'#5a1f1d','default':'transparent'};
var HEADD = {blue:'#1e2c4a',red:'#4a1f1f',orange:'#4a3018',green:'#1d3a1f',grey:'#333','default':'#232323'};
var BG = {'grey-50':'#f8f9fa','grey-100':'#f5f6f7','grey-200':'#eff0f1','red-50':'#fef1f1','red-100':'#fde2e2','default':'transparent'};
var SIZE = {'heading-1':'24px','heading-2':'20px','heading-3':'18px','heading-4':'16px',heading:'18px','normal':'14px','notation':'12px','xxxx-large':'26px','xxx-large':'24px','xx-large':'22px','x-large':'20px','large':'16px','medium':'14px','small':'12px','x-small':'11px'};
function esc(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
function tagHTML(color,text){var c=TAG[color]||TAG.neutral;return '<span class="tag" style="background:'+c[0]+';color:'+c[1]+'">'+text+'</span>';}
function mdHTML(s){
  s = esc(s);
  s = s.replace(/&lt;font color='([a-z-]+)'&gt;/g,function(_,c){return '<span style="color:'+(FONT[c]||c)+'">';}).replace(/&lt;\/font&gt;/g,'</span>');
  s = s.replace(/&lt;text_tag color='([a-z-]+)'&gt;([\s\S]*?)&lt;\/text_tag&gt;/g,function(_,c,t){return tagHTML(c,t);});
  s = s.replace(/\x60([^\x60]+)\x60/g,'<code>$1</code>');
  s = s.replace(/\*\*([\s\S]+?)\*\*/g,'<b>$1</b>');
  s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g,'<a href="$2">$1</a>');
  return s.replace(/\n/g,'<br>');
}
function box(el,css){var d=document.createElement('div');if(css)d.style.cssText=css;return d;}
function render(els,parent){(els||[]).forEach(function(e){parent.appendChild(one(e));});}
function one(e){
  var d;
  switch(e.tag){
  case 'markdown':
    d=box(null,'');d.className='md';
    d.style.fontSize=SIZE[e.text_size]||'14px';
    if(/heading/.test(e.text_size||''))d.style.fontWeight='600';
    if(e.text_align)d.style.textAlign=e.text_align;
    if(e.margin)d.style.margin=e.margin;
    d.innerHTML=mdHTML(e.content||'');return d;
  case 'div':
    d=box();d.className='md';d.innerHTML=mdHTML((e.text&&e.text.content)||'');return d;
  case 'hr': return document.createElement('hr');
  case 'column_set':
    d=box();d.className='cs';
    if(e.margin)d.style.margin=e.margin;
    if(e.background_style)d.style.background=(DARK?BGD:BG)[e.background_style]||'';
    var btnRow=(e.columns||[]).every(function(c){return (c.elements||[]).every(function(x){return x.tag==='button';});});
    if(btnRow){d.className='btns';}
    (e.columns||[]).forEach(function(c){
      var cd=box();cd.className='col';
      if(c.width==='weighted'){cd.style.flex=(c.weight||1)+' 1 0';}else{cd.style.flex='0 0 auto';}
      if(c.background_style)cd.style.background=(DARK?BGD:BG)[c.background_style]||'';
      if(c.padding)cd.style.padding=c.padding;
      if(c.margin)cd.style.margin=c.margin;
      cd.style.display='flex';cd.style.flexDirection='column';
      cd.style.justifyContent=c.vertical_align==='center'?'center':(c.vertical_align==='bottom'?'flex-end':'flex-start');
      render(c.elements,cd);d.appendChild(cd);
    });
    return d;
  case 'interactive_container':
    d=box();d.style.cssText='background:'+((DARK?BGD:BG)[e.background_style]||'')+';border-radius:'+(e.corner_radius||0)+';padding:'+(e.padding||'')+';margin:'+(e.margin||'');
    render(e.elements,d);return d;
  case 'collapsible_panel':
    d=document.createElement('details');d.className='panel';if(e.expanded)d.open=true;
    var s=document.createElement('summary');
    s.textContent='▸ '+((e.header&&e.header.title&&e.header.title.content)||'展开');d.appendChild(s);
    render(e.elements,d);return d;
  case 'button':
    d=document.createElement('button');d.className='btn'+(e.type&&/primary/.test(e.type)?' primary':'');
    d.textContent=(e.text&&e.text.content)||'';return d;
  case 'text_tag':
    d=document.createElement('span');d.innerHTML=tagHTML(e.color,esc((e.text&&e.text.content)||''));return d;
  default:
    d=box();d.className='raw';d.textContent='[未预览的组件: '+e.tag+']';return d;
  }
}
function cardEl(card){
  var c=box();c.className='card';
  var h=card.header||{};var hc=HEAD[h.template]||HEAD['default'];
  var hd=box('', 'background:'+(DARK?(HEADD[h.template]||HEADD['default']):hc[0])+';color:'+(DARK?'#e6e6e6':hc[1]));hd.className='hd';
  hd.innerHTML='<div class="t">'+esc((h.title&&h.title.content)||'')+'</div>'+
    (h.subtitle?'<div class="st">'+esc(h.subtitle.content)+'</div>':'')+
    (h.text_tag_list?'<div class="tags">'+h.text_tag_list.map(function(t){return tagHTML(t.color,esc(t.text.content));}).join('')+'</div>':'');
  c.appendChild(hd);
  var bd=box();bd.className='bd';render((card.body||{}).elements,bd);c.appendChild(bd);
  return c;
}
var DARK=false;function draw(){var root=document.getElementById('root');root.innerHTML='';var group=null,grid=null;
CARDS.forEach(function(x){
  if(x.Group!==group){group=x.Group;var h2=document.createElement('h2');h2.textContent=group;root.appendChild(h2);grid=box();grid.className='grid';root.appendChild(grid);}
  var it=box();it.className='item';
  it.innerHTML='<h3>'+esc(x.Name)+'</h3><p>'+esc(x.Desc)+'</p>';
  it.appendChild(cardEl(x.Card));
  var j=document.createElement('details');j.className='json';
  j.innerHTML='<summary>卡片 JSON</summary><pre>'+esc(JSON.stringify(x.Card,null,1))+'</pre>';
  it.appendChild(j);grid.appendChild(it);
});}
document.getElementById('dark').onchange=function(){DARK=this.checked;document.body.classList.toggle('dark',DARK);draw();};draw();
</script></body></html>
`
