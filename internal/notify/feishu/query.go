package feishu

import (
	"strings"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

const helpText = `用法：
- @我 额度 / 状态：整体额度与结论
- @我 codex|claude|gemini|gemini-cli|antigravity|ollama：只看某个渠道（gemini 与 gemini-cli 均指 Gemini CLI，Antigravity 是独立渠道）
- @我 异常 / 告警：只看异常项
- @我 帮助：显示本提示
查询及「刷新额度」不主动改启停/策略；查询可能触发CPA自动OAuth续期。`

// buildReply renders the text answer for a parsed intent. stale marks a
// degraded answer taken from the last cached report instead of a fresh
// collection.
func (b *Bot) buildReply(in intent, rep domain.Report, stale bool) string {
	switch in.kind {
	case intentHelp, intentUnknown:
		if in.kind == intentUnknown {
			return "没看懂这条问法。\n" + helpText
		}
		return helpText
	}

	filtered, note := filterReport(rep, in)
	var body strings.Builder
	if stale {
		body.WriteString("⚠️ 实时采集失败，以下为上次成功采集的数据，数据可能过期。\n")
	}
	if note != "" {
		body.WriteString(note)
		body.WriteString("\n")
	}
	msg := domain.Message{Kind: render.KindQuery, Report: &filtered}
	out := b.renderer.Text(msg)
	return body.String() + out
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
		return out, ""
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
		return out, ""
	default:
		return rep, ""
	}
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
