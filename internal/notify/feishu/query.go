package feishu

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

const helpText = `用法：
- @我 额度 / 状态：整体额度与结论
- @我 codex|claude|gemini|gemini-cli|antigravity|ollama：只看某个渠道（gemini 与 gemini-cli 均指 Gemini CLI，Antigravity 是独立渠道）
- @我 异常 / 告警：只看异常项
- @我 帮助：显示本提示
查询及「刷新额度」不主动改启停/策略；查询可能触发CPA自动OAuth续期。`

// defaultFreshWindow is used when no poll interval is configured. Cached data
// younger than the collection cadence is simply the newest data that exists,
// so it is not a fault worth warning about.
const defaultFreshWindow = 15 * time.Minute

// freshness describes how current an answer is.
//
// The distinction that matters to a reader is three-way, not two-way:
//
//	live    — this answer was collected just now
//	cached  — we did not re-collect (rate limit, 2.5s budget), but the cached
//	          report is younger than one poll interval, so it IS the current
//	          picture; saying "采集失败" here is a false alarm
//	expired — we could not get new data AND the cache is older than a poll
//	          interval, so the numbers may genuinely have moved
type freshness struct {
	live      bool
	expired   bool
	age       time.Duration
	available bool
}

// freshnessOf classifies an answer. cached is true when the report came from
// the fallback path rather than a completed collection.
func freshnessOf(rep domain.Report, cached bool, window time.Duration, now time.Time) freshness {
	if window <= 0 {
		window = defaultFreshWindow
	}
	f := freshness{live: !cached, available: !rep.GeneratedAt.IsZero()}
	if f.available {
		f.age = now.Sub(rep.GeneratedAt)
		if f.age < 0 {
			f.age = 0
		}
	}
	if cached && (!f.available || f.age > window) {
		f.expired = true
	}
	return f
}

// label is the short suffix shown next to the data time.
func (f freshness) label() string {
	switch {
	case f.live:
		return "实时"
	case !f.available:
		return "缓存 · 时间未知"
	case f.expired:
		return "缓存 · " + humanAge(f.age)
	default:
		return "缓存 · " + humanAge(f.age) + "（本次未实时刷新）"
	}
}

// notice is the prominent warning line. It is empty for the ordinary
// "served from a still-current cache" case: that is not a failure.
func (f freshness) notice() string {
	if !f.expired {
		return ""
	}
	if !f.available {
		return "未能取到新数据，且缓存数据没有时间戳，请谨慎使用。"
	}
	return "未能取到新数据，以下为 " + humanAge(f.age) + "的缓存，可能已过期。"
}

// humanAge renders an age the way a person would say it.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return "约 " + strconv.Itoa(int(d.Minutes())) + " 分钟前"
	case d < 24*time.Hour:
		return "约 " + strconv.Itoa(int(d.Hours())) + " 小时前"
	default:
		return "约 " + strconv.Itoa(int(d.Hours()/24)) + " 天前"
	}
}

// buildReply renders the text answer for a parsed intent.
func (b *Bot) buildReply(in intent, rep domain.Report, fresh freshness) string {
	switch in.kind {
	case intentHelp, intentUnknown:
		if in.kind == intentUnknown {
			return "没看懂这条问法。\n" + helpText
		}
		return helpText
	}

	filtered, note := filterReport(rep, in)
	msg := domain.Message{
		Kind:      render.KindQuery,
		Report:    &filtered,
		Body:      note,
		Freshness: fresh.label(),
		Notice:    fresh.notice(),
		// A single-channel question is a request for that channel's detail, so
		// every window is expanded there and nowhere else.
		Detailed: in.kind == intentProvider,
	}
	out := b.renderer.Text(msg)
	if hint := channelHint(filtered, in); hint != "" {
		out += hint + "\n"
	}
	return out
}

// channelHint tells the reader how to get the detail the compact view folded
// away. It is pointless on a reply that is already the detailed view.
func channelHint(rep domain.Report, in intent) string {
	if in.kind == intentProvider || len(rep.Providers) == 0 {
		return ""
	}
	names := make([]string, 0, len(rep.Providers))
	for _, p := range rep.Providers {
		if p.Provider != "" {
			names = append(names, string(p.Provider))
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return "\n看单渠道明细：@我 " + strings.Join(names, " / ")
}

// filterReport narrows a report for a query and returns a short human note when
// the result is empty or the request could not be satisfied.
func filterReport(rep domain.Report, in intent) (domain.Report, string) {
	switch in.kind {
	case intentAbnormal:
		out := rep
		out.Providers = nil
		for _, p := range rep.Providers {
			if isAbnormal(p.WorstState) {
				out.Providers = append(out.Providers, p)
			}
		}
		if len(out.Providers) == 0 {
			return domain.Report{GeneratedAt: rep.GeneratedAt}, "当前没有异常项。"
		}
		return keepMatchingRecommendations(out), ""
	case intentProvider:
		out := rep
		out.Providers = nil
		for _, p := range rep.Providers {
			if p.Provider == in.provider {
				out.Providers = append(out.Providers, p)
			}
		}
		if len(out.Providers) == 0 {
			return domain.Report{GeneratedAt: rep.GeneratedAt}, "未找到该渠道的数据。"
		}
		return keepMatchingRecommendations(out), ""
	default:
		return rep, ""
	}
}

// keepMatchingRecommendations drops advice for providers the filter removed.
//
// The conclusion line is built from the recommendations, so leaving them in
// made a "@我 codex" answer open with a verdict about Claude — a channel the
// reader had just excluded and whose data is not shown below it.
func keepMatchingRecommendations(rep domain.Report) domain.Report {
	kept := map[domain.ProviderKind]bool{}
	for _, p := range rep.Providers {
		kept[p.Provider] = true
	}
	var recs []domain.Recommendation
	for _, rec := range rep.Recommendations {
		if kept[rec.Provider] {
			recs = append(recs, rec)
		}
	}
	rep.Recommendations = recs
	return rep
}

func isAbnormal(s domain.CredentialState) bool {
	switch s {
	case domain.StateHealthy, domain.StateNotice:
		return false
	case domain.StateLimited:
		// Include limited coverage in the attention view, without calling it
		// an account-wide failure. Rendering retains the scoped state label.
		return true
	default:
		return true
	}
}
