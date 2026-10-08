package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// This file holds the shared, channel-agnostic projection behind both the text
// reply and the card.
//
// The guiding rule is that a chat answer is read on a phone: a normal channel
// collapses to a single line, and only the credentials a reader must act on are
// expanded. Nothing precise is dropped to achieve that — percentages, reset
// times, provenance and confidence all survive the collapse; what disappears is
// repetition and internal identifiers.

// credRow is one credential that earned its own line.
type credRow struct {
	label      string
	alias      string
	shortID    string
	state      domain.CredentialState
	windows    []domain.QuotaWindow
	stale      bool
	failure    domain.FailureKind
	lastOK     time.Time
	confidence domain.Confidence
	plan       string
	code       string
}

// WindowSignature returns a deterministic signature of window names for isomorphism grouping.
func (c credRow) WindowSignature() string {
	if len(c.windows) == 0 {
		return ""
	}
	names := make([]string, 0, len(c.windows))
	for _, w := range c.windows {
		names = append(names, strings.TrimSpace(w.Name))
	}
	sort.Strings(names)
	return strings.Join(names, "||")
}

// extraUsageRow is a credential's pay-as-you-go budget. It is real money-ish
// spend, so it gets a line even under an otherwise folded healthy channel.
type extraUsageRow struct {
	label string
	usage domain.ExtraUsage
}

// credBar is one credential's tightest measurement, projected for the card's
// progress chart. Every credential that reported a number gets a bar, not only
// the ones that earned a detail row: a chart that silently omitted a healthy
// account would misrepresent the channel.
type credBar struct {
	label string
	// name is the short label for the chart axis: the credential alias or
	// short id, never the "alias · shortid" form, which gets truncated on the
	// axis and stops identifying the account.
	name  string
	used  *float64
	stale bool
}

// providerView is the compact projection of one provider.
type providerView struct {
	provider  domain.ProviderKind
	direction domain.Direction
	name      string
	total     int
	normal    int
	limited   int
	abnormal  int
	breakdown []string
	// worst is the highest reported usage across this provider's credentials.
	// Separate accounts are not additive, so this is "the tightest single
	// credential", never a sum or an average.
	worst      *float64
	worstStale bool
	// scopedOnly records that nothing account-wide was measured. Without it a
	// provider whose only evidence is model/group scoped would read as if the
	// whole account had that much headroom.
	scopedOnly bool
	// unknownScope records that at least one window did not declare what it
	// applies to. That is evidence in its own right and survives the fold.
	unknownScope bool
	source       string
	// caution is a plain-language warning about the numbers, empty when there
	// is nothing to warn about. It replaces the always-present confidence
	// label, which said "已上报" on nearly every line and taught nobody
	// anything.
	caution string
	rows    []credRow
	allRows []credRow
	extras  []extraUsageRow
	// bars is one entry per credential that reported a usage number. It feeds
	// the card's chart only; the text channel never uses it.
	bars []credBar
	// worstState is the most severe credential state in this provider. It is
	// the header tag's colour source on the card.
	worstState domain.CredentialState
	// plans are the distinct subscription tiers the upstream reported for this
	// provider's credentials. Empty when the response carries none; we never
	// print a "未知套餐" placeholder.
	plans []string
	// recoverAt is the earliest reset among the credentials that are out of
	// quota: the answer to "when can I use this again".
	recoverAt *time.Time
	advice    string
	failure   string
}

// needsDetail decides which credentials are expanded.
//
// Healthy and limited credentials stay folded: they carry a real number and no
// action. Everything else — pressure, exhaustion, a dead credential, missing
// data — is something the reader has to see per credential, because a
// provider-level verdict cannot say which credential it came from.
func needsDetail(s domain.CredentialState) bool {
	switch s {
	case domain.StateHealthy, domain.StateLimited:
		return false
	default:
		return true
	}
}

// breakdownRank orders the printed categories: fine, then constrained, then
// broken. It is display order only and carries no semantics.
func breakdownRank(s domain.CredentialState) int {
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
	default:
		return 8
	}
}

// shortStateLabel is the terse form used in counts and row prefixes.
func shortStateLabel(s domain.CredentialState) string {
	switch s {
	case domain.StateHealthy:
		return "正常"
	case domain.StateLimited:
		return "局部额度"
	case domain.StateNotice:
		return "接近上限"
	case domain.StateWarning:
		return "额度告警"
	case domain.StateExhausted:
		return "已用满"
	case domain.StateInvalid:
		return "凭证失效"
	case domain.StateSuspect:
		return "疑似异常"
	case domain.StateStale:
		return "取不到数据"
	default:
		return "取不到数据"
	}
}

// credentialState resolves one snapshot's projected state.
//
// The evaluated States map is authoritative. When a caller hands us a report
// without it we fall back conservatively: a failed or stale read is never
// promoted to the provider's best-case state.
func credentialState(p domain.ProviderReport, s domain.QuotaSnapshot) domain.CredentialState {
	if st, ok := p.States[s.Credential.Key]; ok && st != "" {
		return st
	}
	switch {
	case s.Failure == domain.FailureUnsupported:
		return domain.StateUnknown
	case !s.OK, s.Stale:
		return domain.StateStale
	case p.WorstState != "":
		return p.WorstState
	default:
		return domain.StateUnknown
	}
}

// classCounts returns the normal/limited/abnormal split.
//
// Healthy/Total is deliberately not used as the headline: a credential whose
// only measurements are model- or group-scoped projects to StateLimited and is
// therefore never "healthy", which rendered three usable Antigravity accounts
// as "0/3 个凭证".
func classCounts(p domain.ProviderReport) (total, normal, limited, abnormal int) {
	total = p.Total
	if n := len(p.Snapshots); n > total {
		total = n
	}
	if len(p.States) > 0 {
		for _, st := range p.States {
			switch domain.ClassOf(st) {
			case domain.ClassNormal:
				normal++
			case domain.ClassLimited:
				limited++
			default:
				abnormal++
			}
		}
		if sum := normal + limited + abnormal; sum > total {
			total = sum
		}
		return total, normal, limited, abnormal
	}
	if total > 0 && p.Normal+p.Limited+p.Abnormal == total {
		return total, p.Normal, p.Limited, p.Abnormal
	}
	// Last resort for hand-built reports: the healthy count plus the worst
	// state's class for the remainder.
	normal = p.Healthy
	if normal > total {
		normal = total
	}
	switch domain.ClassOf(p.WorstState) {
	case domain.ClassNormal:
		normal = total
	case domain.ClassLimited:
		limited = total - normal
	default:
		abnormal = total - normal
	}
	return total, normal, limited, abnormal
}

// summarize builds the compact view of one provider.
func (r *Renderer) summarize(rep *domain.Report, p domain.ProviderReport, detailed bool) providerView {
	v := providerView{provider: p.Provider, name: r.displayName(p.Provider), failure: oneLine(p.Error)}
	v.total, v.normal, v.limited, v.abnormal = classCounts(p)

	byState := map[domain.CredentialState]int{}
	var stateOrder []domain.CredentialState
	measured, accountMeasured := false, false

	// A fresh number always beats a carried-over one; within the same
	// freshness the tightest wins. A stale reading may still be the only thing
	// we have, so it is kept and labelled rather than dropped.
	better := func(val float64, stale bool) bool {
		switch {
		case v.worst == nil:
			return true
		case v.worstStale && !stale:
			return true
		case !v.worstStale && stale:
			return false
		default:
			return val > *v.worst
		}
	}

	for _, s := range p.Snapshots {
		st := credentialState(p, s)
		if v.worstState == "" || breakdownRank(st) > breakdownRank(v.worstState) {
			v.worstState = st
		}
		// EVERY credential is counted, healthy ones included. A breakdown that
		// silently omits a category forces the reader to subtract: "3 个号 ·
		// 1 个凭证失效 · 1 个已用满" leaves the third unaccounted for.
		if _, seen := byState[st]; !seen {
			stateOrder = append(stateOrder, st)
		}
		byState[st]++
		cr := credRow{
			label:      shortCredentialName(s.Credential),
			alias:      s.Credential.Alias,
			shortID:    s.Credential.ShortID,
			state:      st,
			windows:    s.Windows,
			stale:      s.Stale,
			failure:    s.Failure,
			lastOK:     s.LastSuccessAt,
			confidence: s.Confidence,
			plan:       strings.TrimSpace(s.Plan),
			code:       strings.TrimSpace(s.Code),
		}
		v.allRows = append(v.allRows, cr)
		if needsDetail(st) || detailed {
			v.rows = append(v.rows, cr)
		}
		// A bar exists only where a number does. A credential with no reading
		// stays a bar-less gap rather than a fake 0% (or 100%) column.
		if w, ok := tightestWindow(s.Windows); ok && w.UsedPercent != nil {
			val := *w.UsedPercent
			v.bars = append(v.bars, credBar{
				label: shortCredentialName(s.Credential),
				name:  shortCredentialName(s.Credential),
				used:  &val,
				stale: s.Stale,
			})
		}
		if plan := strings.TrimSpace(s.Plan); plan != "" {
			v.plans = appendUnique(v.plans, plan)
		}
		if s.ExtraUsage.Reportable() && !s.Stale {
			v.extras = append(v.extras, extraUsageRow{label: shortCredentialName(s.Credential), usage: *s.ExtraUsage})
		}
		if st == domain.StateExhausted || s.Stale {
			// Recovery time only comes from credentials that are actually out
			// of quota; a healthy credential's reset is not a recovery event.
			if st == domain.StateExhausted {
				v.recoverAt = earlier(v.recoverAt, exhaustedResetOf(s.Windows))
			}
		}
		for _, w := range s.Windows {
			if w.UsedPercent == nil {
				continue
			}
			measured = true
			switch w.Scope.Normalized() {
			case domain.ScopeAccount:
				accountMeasured = true
			case domain.ScopeUnknown:
				v.unknownScope = true
			}
			if better(*w.UsedPercent, s.Stale) {
				val := *w.UsedPercent
				v.worst, v.worstStale = &val, s.Stale
			}
		}
	}
	// Credentials the evaluator judged but that produced no snapshot still
	// belong in the count, or the totals would silently disagree.
	for key, st := range p.States {
		found := false
		for _, s := range p.Snapshots {
			if s.Credential.Key == key {
				found = true
			}
		}
		if found {
			continue
		}
		if _, seen := byState[st]; !seen {
			stateOrder = append(stateOrder, st)
		}
		byState[st]++
		if v.worstState == "" || breakdownRank(st) > breakdownRank(v.worstState) {
			v.worstState = st
		}
	}
	// Order the categories the way a reader scans them: what is fine first,
	// then what is constrained, then what is broken.
	sort.SliceStable(stateOrder, func(i, j int) bool {
		return breakdownRank(stateOrder[i]) < breakdownRank(stateOrder[j])
	})
	counted := 0
	for _, st := range stateOrder {
		v.breakdown = append(v.breakdown, strconv.Itoa(byState[st])+" 个"+shortStateLabel(st))
		counted += byState[st]
	}
	// A hand-built report may carry only aggregate counts and a worst state,
	// and a report can disagree with its own snapshot list. Either way the
	// printed categories must still add up to the stated total, so whatever is
	// unaccounted for is shown rather than dropped.
	if missing := v.total - counted; missing > 0 {
		if counted == 0 && v.limited+v.abnormal > 0 {
			v.breakdown = append(v.breakdown, strconv.Itoa(missing)+" 个"+shortStateLabel(p.WorstState))
		} else {
			v.breakdown = append(v.breakdown, strconv.Itoa(missing)+" 个未统计")
		}
	}
	v.scopedOnly = measured && !accountMeasured

	v.source, _ = r.sourcesAndFetched(p, rep.GeneratedAt)
	v.caution = dataCaution(p)
	for _, rec := range rep.Recommendations {
		if rec.Provider == p.Provider {
			v.advice = oneLine(rec.Reason)
			v.direction = rec.Direction
		}
	}
	return v
}

// usability grades a channel for the "who do I use now" question.
type usability int

const (
	// unusable: nothing left to send traffic to.
	unusable usability = iota
	// undecidable: we cannot honestly grade it (no fresh number, or a window
	// whose applicability the upstream never declared).
	undecidable
	// tight: usable, but the tightest credential is already past halfway.
	tight
	// ample: usable with real headroom.
	ample
)

// halfUsed is the headroom bar. It matches the engine's own use-more rule, so
// the chat conclusion and the evaluated recommendation cannot disagree.
const halfUsed = 50.0

// grade answers "can I send traffic here right now".
//
// A channel measured only at model/group scope can still be graded: the rule
// is that EVERY observed scope must have headroom, and the coverage limit is
// then stated on the provider's own line rather than in the conclusion. What
// can never be graded is a window whose applicability the upstream did not
// declare — unknown scope stays unknown and is not rounded either way.
//
// It is judged on the accounts that can still take traffic: an exhausted or
// broken account does not make its usable siblings unusable.
func (v providerView) grade() usability {
	used, _, usable := channelHeadroom(v)
	switch {
	case v.total == 0:
		return undecidable
	case len(v.allRows) > 0 && !usable:
		return unusable
	case len(v.allRows) == 0 && v.normal+v.limited == 0:
		return unusable
	case len(v.allRows) == 0:
		used = v.worst
	}
	switch {
	case v.unknownScope, used == nil, v.worstStale:
		return undecidable
	case *used >= 100:
		return unusable
	case *used < halfUsed:
		return ample
	default:
		return tight
	}
}

// coverageNote is the caveat that belongs on the provider line, not in the
// conclusion: the numbers are real, but they only cover the scopes we saw.
func (v providerView) coverageNote() string {
	switch {
	case v.unknownScope:
		return "覆盖：存在适用范围未申明的窗口，无法据此判断全账号容量"
	case v.scopedOnly:
		return "覆盖：按已观测的模型/分组统计，未覆盖全账号"
	default:
		return ""
	}
}

// exhaustedResetOf returns the reset instant of the window that is actually
// out of quota, so "when does this come back" is answered by the blocking
// window rather than by whichever window resets first.
func exhaustedResetOf(windows []domain.QuotaWindow) *time.Time {
	var out *time.Time
	for _, w := range windows {
		blocked := w.LimitReached || (w.UsedPercent != nil && *w.UsedPercent >= 100)
		if !blocked || w.ResetAt == nil {
			continue
		}
		out = later(out, w.ResetAt)
	}
	return out
}

// earlier keeps the soonest of two optional instants.
func earlier(a, b *time.Time) *time.Time {
	switch {
	case b == nil:
		return a
	case a == nil:
		return b
	case b.Before(*a):
		return b
	default:
		return a
	}
}

// later keeps the latest of two optional instants: a credential is only usable
// again once its LAST blocking window has reset.
func later(a, b *time.Time) *time.Time {
	switch {
	case b == nil:
		return a
	case a == nil:
		return b
	case b.After(*a):
		return b
	default:
		return a
	}
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// scopeTag is the applicability marker appended to a window cell.
//
// It is suppressed when the display label already carries the same
// information, so a Codex account window reads "账号 · 主额度窗口 100.0%"
// instead of "账号 · 主额度窗口 100.0% [账号]".
func scopeTag(w domain.QuotaWindow) string {
	label := w.DisplayLabel()
	kind := domain.ScopeText(w.Scope, "")
	if strings.HasPrefix(label, kind) {
		return ""
	}
	if id := domain.HumanizeIdentifier(w.ScopeID); id != "" && strings.Contains(label, id) {
		return kind
	}
	return w.ScopeText()
}

// dataCaution warns about the numbers in plain language, or says nothing.
//
// "已上报" was on every line and meant nothing to a reader — the overwhelmingly
// common case does not need a label. Only a reason to distrust a number earns
// one:
//
//   - estimated values (a scraped page rather than a reported figure) are
//     always flagged, because the number itself is derived;
//   - a credential we could not read is flagged only when no per-credential
//     row already explains it. When the breakdown already says "1 个凭证失效",
//     repeating "部分取不到" is the same noise in a different word.
func dataCaution(p domain.ProviderReport) string {
	estimated, unknown, known := 0, 0, 0
	abnormal := 0
	for _, s := range p.Snapshots {
		if !s.OK {
			continue
		}
		switch s.Confidence {
		case domain.ConfidenceEstimated:
			estimated++
		case domain.ConfidenceReported:
			known++
		default:
			unknown++
		}
		if st, ok := p.States[s.Credential.Key]; ok && domain.ClassOf(st) == domain.ClassAbnormal {
			abnormal++
		}
	}
	switch {
	case estimated > 0:
		return "估算值"
	case known == 0 && unknown > 0:
		return "全部取不到"
	case unknown > 0 && abnormal == 0:
		return "部分取不到"
	default:
		return ""
	}
}

// headline is the single line a folded provider collapses to.
//
// detailed adds the provenance. In the overview the data source is the same
// value on every line and explains nothing about what to do, so it is kept for
// the single-channel view where a reader is actually inspecting one channel.
func (v providerView) headline(pad int, detailed bool) string {
	var b strings.Builder
	b.WriteString(padRight(v.name, pad))
	b.WriteString("  ")
	if v.abnormal > 0 || v.failure != "" {
		b.WriteString("⚠️ ")
	}
	parts := []string{strconv.Itoa(v.total) + " 个号"}
	parts = append(parts, v.breakdown...)
	switch {
	case v.worst != nil && v.worstStale:
		parts = append(parts, "最紧 "+fmt.Sprintf("%.1f%%", *v.worst)+"（旧值）")
	case v.worst != nil:
		parts = append(parts, "最紧 "+fmt.Sprintf("%.1f%%", *v.worst))
	default:
		parts = append(parts, "用量未上报")
	}
	if v.scopedOnly && v.limited == 0 {
		// Never let scoped headroom read as account-wide headroom. When the
		// state counts already say "局部额度" this would only repeat them.
		parts = append(parts, "无账号级测量")
	}
	if v.unknownScope {
		// Unknown applicability is preserved, never rounded to account scope.
		parts = append(parts, "含范围未知窗口")
	}
	if len(v.plans) > 0 {
		// Only shown when the upstream actually reported a tier. No placeholder.
		parts = append(parts, "套餐 "+strings.Join(v.plans, "/"))
	}
	if v.caution != "" {
		// Evidence quality is not dropped, only reworded: it appears when — and
		// only when — there is a reason to distrust the numbers above.
		parts = append(parts, v.caution)
	}
	if detailed {
		parts = append(parts, "来源 "+v.source)
	}
	b.WriteString(strings.Join(parts, " · "))
	return b.String()
}

// extraLines renders pay-as-you-go spend verbatim, in the upstream's own unit.
func (r *Renderer) extraLines(v providerView) []string {
	out := make([]string, 0, len(v.extras))
	for _, e := range v.extras {
		var facts []string
		switch {
		case e.usage.UsedCredits != nil && e.usage.MonthlyLimit != nil:
			facts = append(facts, "已用 "+amount(e.usage.UsedCredits)+" / 上限 "+amount(e.usage.MonthlyLimit)+" credits")
		case e.usage.UsedCredits != nil:
			facts = append(facts, "已用 "+amount(e.usage.UsedCredits)+" credits")
		case e.usage.MonthlyLimit != nil:
			facts = append(facts, "月度上限 "+amount(e.usage.MonthlyLimit)+" credits")
		}
		if e.usage.UsedPercent != nil {
			facts = append(facts, percent(e.usage.UsedPercent))
		}
		if len(facts) == 0 {
			continue
		}
		out = append(out, "  · 额外用量："+e.label+"  "+strings.Join(facts, "  "))
	}
	return out
}

// amount prints an upstream credit amount without inventing precision or a
// currency symbol: the payload names no currency anywhere.
func amount(v *float64) string {
	if v == nil {
		return "未上报"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// failureReasonWithCode formats the failure reason with an optional error code.
func failureReasonWithCode(f domain.FailureKind, code string) string {
	reason := domain.FailureReason(f)
	if code = strings.TrimSpace(code); code != "" {
		return code + " · " + reason
	}
	return reason
}

// rowLines renders the expanded credentials. detailed lists every window;
// otherwise only the tightest one is shown, which is the number that drives the
// decision.
func (r *Renderer) rowLines(v providerView, detailed bool) []string {
	var out []string
	for _, row := range v.rows {
		head := "  · " + row.label + "  " + shortStateLabel(row.state)
		if detailed && row.plan != "" {
			head += "  套餐 " + row.plan
		}
		windows := row.windows
		folded := 0
		if !detailed {
			if w, ok := tightestWindow(windows); ok {
				folded = len(windows) - 1
				windows = []domain.QuotaWindow{w}
			}
		}
		if len(windows) == 0 {
			// No numbers at all: say why, and give this credential's own last
			// success. Borrowing a sibling credential's success time would
			// claim data we never had for this one.
			if row.code != "" {
				out = append(out, head+" · "+row.code+" · "+domain.FailureReason(row.failure)+"  最后成功 "+r.lastSuccessText(row.lastOK))
			} else {
				out = append(out, head+"  依据："+domain.FailureReason(row.failure)+"  最后成功 "+r.lastSuccessText(row.lastOK))
			}
			continue
		}
		first := head + "  " + r.windowCell(windows[0]) + r.staleTag(row)
		if folded > 0 {
			// The folded windows are not discarded evidence, just deferred:
			// say how many there are and how to see them.
			first += "  · 另 " + strconv.Itoa(folded) + " 个窗口"
		}
		out = append(out, first)
		for _, w := range windows[1:] {
			out = append(out, "    "+r.windowCell(w)+r.staleTag(row))
		}
		if row.failure != domain.FailureNone && row.failure != "" {
			out = append(out, "    依据："+failureReasonWithCode(row.failure, row.code))
		}
	}
	return out
}

// staleTag marks carried-over numbers and stamps the credential's OWN last
// success, never the provider-wide latest.
func (r *Renderer) staleTag(row credRow) string {
	if !row.stale {
		return ""
	}
	return "［旧值 · 最后成功 " + r.lastSuccessText(row.lastOK) + "］"
}

// windowCell is one window's full evidence: readable name, exact percentage,
// applicability and reset time.
func (r *Renderer) windowCell(w domain.QuotaWindow) string {
	cell := w.DisplayLabel() + " " + percent(w.UsedPercent)
	if tag := scopeTag(w); tag != "" {
		cell += "  [" + tag + "]"
	}
	if w.RemainingAmount != "" {
		// The unit is the token type the label already names, so the number is
		// shown verbatim rather than converted.
		cell += "  剩余 " + w.RemainingAmount
	}
	if rt := r.resetText(w); rt != "未上报" {
		cell += "  " + rt + " 刷新"
	}
	// An explicit upstream limit marker is kept even when the percentage does
	// not look maxed out: the two are independent signals and disagreeing with
	// the upstream silently would hide a real block.
	if w.LimitReached && (w.UsedPercent == nil || *w.UsedPercent < 100) {
		cell += "  [上游标记已达上限]"
	}
	return cell
}

// tightestWindow is the highest reported usage of one credential.
func tightestWindow(windows []domain.QuotaWindow) (domain.QuotaWindow, bool) {
	var best domain.QuotaWindow
	found := false
	for _, w := range windows {
		if w.UsedPercent == nil {
			continue
		}
		if !found || *w.UsedPercent > *best.UsedPercent {
			best, found = w, true
		}
	}
	if !found && len(windows) > 0 {
		return windows[0], true
	}
	return best, found
}

func (r *Renderer) lastSuccessText(t time.Time) string {
	if t.IsZero() {
		return "从未成功"
	}
	return r.formatTime(t)
}

// padRight pads to a display width that counts CJK characters as two columns,
// so provider names line up in a monospaced chat client.
func padRight(s string, width int) string {
	w := displayWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r > 0x2E80 {
			w += 2
		} else {
			w++
		}
	}
	return w
}
