package feishu

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

const targetChat = "oc_target_group"

// --- fakes -----------------------------------------------------------------

type fakeSender struct {
	cards   []map[string]any
	replies []string
}

func (f *fakeSender) SendCard(_ context.Context, _ string, card map[string]any) error {
	f.cards = append(f.cards, card)
	return nil
}

func (f *fakeSender) ReplyText(_ context.Context, _ string, text string) error {
	f.replies = append(f.replies, text)
	return nil
}

type fakeRefresher struct {
	mu       sync.Mutex
	calls    int
	report   domain.Report
	last     domain.Report
	hasLast  bool
	refreshE error
	block    bool
	// takes models a real collection's duration. On timeout the refresher
	// returns the cached report with the context error, exactly as App does.
	takes time.Duration
}

func (f *fakeRefresher) RefreshNow(ctx context.Context) (domain.Report, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return domain.Report{}, ctx.Err()
	}
	if f.takes > 0 {
		select {
		case <-time.After(f.takes):
		case <-ctx.Done():
			return domain.Report{}, ctx.Err()
		}
	}
	if f.refreshE != nil {
		return domain.Report{}, f.refreshE
	}
	return f.report, nil
}

func (f *fakeRefresher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRefresher) LastReport() (domain.Report, bool) {
	return f.last, f.hasLast
}

// --- fixtures --------------------------------------------------------------

func testReport() domain.Report {
	used := 92.5
	reset := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	fetched := time.Date(2026, 3, 13, 17, 0, 0, 0, time.UTC)
	cred := domain.Credential{Key: "codex-1", Provider: domain.ProviderCodex, Alias: "主号", ShortID: "a1b2"}
	return domain.Report{
		GeneratedAt: fetched,
		Providers: []domain.ProviderReport{{
			Provider:    domain.ProviderCodex,
			Total:       1,
			Limited:     1,
			WorstState:  domain.StateWarning,
			States:      map[string]domain.CredentialState{"codex-1": domain.StateWarning},
			BestWindows: []domain.QuotaWindow{{Name: "5h", Scope: domain.ScopeAccount, UsedPercent: &used, ResetAt: &reset}},
			Snapshots: []domain.QuotaSnapshot{{
				Credential: cred,
				Windows:    []domain.QuotaWindow{{Name: "5h", Scope: domain.ScopeAccount, UsedPercent: &used, ResetAt: &reset}},
				Source:     domain.SourceCPA,
				Confidence: domain.ConfidenceReported,
				FetchedAt:  fetched,
				OK:         true,
			}},
		}},
		Recommendations: []domain.Recommendation{
			{Provider: domain.ProviderCodex, Direction: domain.DirectionEaseOff, Reason: "接近阈值"},
		},
	}
}

func newTestBot(t *testing.T, r domain.QuotaRefresher) (*Bot, *fakeSender) {
	t.Helper()
	cfg := config.Config{
		FeishuAppID:     "app",
		FeishuAppSecret: "secret",
		FeishuChatID:    targetChat,
		Tone:            config.ToneCasual,
	}
	b, err := New(cfg, r)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := &fakeSender{}
	b.sender = s
	b.replyBudget = time.Second
	b.refreshBudget = time.Second
	return b, s
}

func strptr(s string) *string { return &s }

func msgEvent(chatID, messageID, content, msgType string, mentions []*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: strptr("ou_user1")}},
			Message: &larkim.EventMessage{
				ChatId:      strptr(chatID),
				MessageId:   strptr(messageID),
				MessageType: strptr(msgType),
				Content:     strptr(content),
				Mentions:    mentions,
			},
		},
	}
}

func botMention() *larkim.MentionEvent {
	return &larkim.MentionEvent{Key: strptr("@_user_1"), MentionedType: strptr("bot")}
}

func cardEvent(chatID, openID, action string) *callback.CardActionTriggerEvent {
	return &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: openID},
			Action:   &callback.CallBackAction{Value: map[string]interface{}{"action": action}},
			Context:  &callback.Context{OpenChatID: chatID},
		},
	}
}

// --- tests -----------------------------------------------------------------

func TestMentionDetection(t *testing.T) {
	bot := msgEvent(targetChat, "om_1", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if !parseMessage(bot).botMentioned {
		t.Fatal("bot mention not detected")
	}
	user := msgEvent(targetChat, "om_1", `{"text":"@_user_1 hi"}`, larkim.MsgTypeText, []*larkim.MentionEvent{
		{Key: strptr("@_user_1"), MentionedType: strptr("user")},
	})
	if parseMessage(user).botMentioned {
		t.Fatal("user mention wrongly treated as bot mention")
	}
	none := msgEvent(targetChat, "om_1", `{"text":"额度"}`, larkim.MsgTypeText, nil)
	if parseMessage(none).botMentioned {
		t.Fatal("no mention wrongly reported as bot mention")
	}
}

func TestTextExtraction(t *testing.T) {
	in := parseMessage(msgEvent(targetChat, "om_1", `{"text":"@_user_1 codex 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()}))
	if !strings.Contains(in.text, "codex") {
		t.Fatalf("text not extracted: %q", in.text)
	}
	if got := stripMentions(in.text); strings.Contains(got, "@_user_1") {
		t.Fatalf("mention token not stripped: %q", got)
	}
}

func TestIntentParsing(t *testing.T) {
	cases := []struct {
		text     string
		wantKind intentKind
		wantProv domain.ProviderKind
	}{
		{"额度", intentStatus, ""},
		{"quota", intentStatus, ""},
		{"状态", intentStatus, ""},
		{"codex", intentProvider, domain.ProviderCodex},
		{"claude 额度", intentProvider, domain.ProviderClaude},
		{"gemini", intentProvider, domain.ProviderGeminiCLI},
		{"gemini-cli", intentProvider, domain.ProviderGeminiCLI},
		{"antigravity 状态", intentProvider, domain.ProviderAntigravity},
		{"ollama", intentProvider, domain.ProviderOllama},
		{"异常", intentAbnormal, ""},
		{"告警", intentAbnormal, ""},
		{"帮助", intentHelp, ""},
		{"help", intentHelp, ""},
		{"今天天气不错", intentUnknown, ""},
		{"", intentStatus, ""},
	}
	for _, tc := range cases {
		got := parseIntent(tc.text)
		if got.kind != tc.wantKind || got.provider != tc.wantProv {
			t.Errorf("parseIntent(%q) = {%v,%q}, want {%v,%q}", tc.text, got.kind, got.provider, tc.wantKind, tc.wantProv)
		}
	}
}

func TestProviderTokenBoundary(t *testing.T) {
	// "antigravity" must not be mistaken for the "anti" alias.
	if got := parseIntent("antigravity"); got.provider != domain.ProviderAntigravity {
		t.Fatalf("antigravity parsed as %q", got.provider)
	}
	// "quota" inside another latin word must not trigger.
	if got := parseIntent("quotax"); got.kind == intentStatus {
		t.Fatal("substring 'quota' inside 'quotax' wrongly matched")
	}
}

func TestNonTargetChatIgnored(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent("oc_other_group", "om_9", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(s.replies) != 0 || r.callCount() != 0 {
		t.Fatalf("foreign chat handled: replies=%d refreshCalls=%d", len(s.replies), r.callCount())
	}
}

func TestNonMentionIgnored(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_2", `{"text":"额度"}`, larkim.MsgTypeText, nil)
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(s.replies) != 0 || r.callCount() != 0 {
		t.Fatalf("non-mentioned message was handled: replies=%d refreshCalls=%d", len(s.replies), r.callCount())
	}
}

func TestQueryRepliesWithEvidence(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_3", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if r.callCount() != 1 {
		t.Fatalf("refresh called %d times, want 1", r.callCount())
	}
	if len(s.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(s.replies))
	}
	reply := s.replies[0]
	for _, want := range []string{"92.5%", "5h"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	// The overview no longer repeats the data source on every line: it is the
	// same value everywhere and says nothing about what to do. It is still
	// reachable, in the single-channel view.
	if strings.Contains(reply, "已上报") {
		t.Errorf("internal confidence jargon shown to the reader:\n%s", reply)
	}

	single := msgEvent(targetChat, "om_3b", `{"text":"@_user_1 codex"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), single); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !strings.Contains(s.replies[1], "来源 cpa-v8") {
		t.Errorf("single-channel view lost the data source:\n%s", s.replies[1])
	}
}

// TestBreakdownAccountsForEveryCredential: the printed categories must add up
// to the stated total. "3 个号 · 1 个凭证失效 · 1 个已用满" left the reader to
// work out what happened to the third.
func TestBreakdownAccountsForEveryCredential(t *testing.T) {
	ok, full := 20.0, 100.0
	win := func(v *float64) []domain.QuotaWindow {
		return []domain.QuotaWindow{{Name: "w", Label: "账号 · 周窗口", Scope: domain.ScopeAccount, UsedPercent: v}}
	}
	snap := func(key string, w []domain.QuotaWindow, okFlag bool, failure domain.FailureKind) domain.QuotaSnapshot {
		return domain.QuotaSnapshot{
			Credential: domain.Credential{Key: key, Provider: domain.ProviderCodex, Alias: key},
			Windows:    w, OK: okFlag, Failure: failure,
			Source: domain.SourceCPAV0, Confidence: domain.ConfidenceReported,
		}
	}
	rep := domain.Report{Providers: []domain.ProviderReport{{
		Provider: domain.ProviderCodex, Total: 3,
		WorstState: domain.StateInvalid,
		States: map[string]domain.CredentialState{
			"a": domain.StateHealthy, "b": domain.StateExhausted, "c": domain.StateInvalid,
		},
		Snapshots: []domain.QuotaSnapshot{
			snap("a", win(&ok), true, domain.FailureNone),
			snap("b", win(&full), true, domain.FailureNone),
			snap("c", nil, false, domain.FailureAuth),
		},
	}}}
	b, _ := newTestBot(t, &fakeRefresher{})
	out := b.buildReply(intent{kind: intentStatus}, rep, freshness{live: true})

	headline := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Codex") {
			headline = line
		}
	}
	for _, want := range []string{"3 个号", "1 个正常", "1 个已用满", "1 个凭证失效"} {
		if !strings.Contains(headline, want) {
			t.Errorf("headline missing %q: %q", want, headline)
		}
	}
	if sum := breakdownSum(t, headline); sum != 3 {
		t.Errorf("categories sum to %d, want 3 (total): %q", sum, headline)
	}
}

// breakdownSum adds every "N 个X" category except the "N 个号" total itself.
func breakdownSum(t *testing.T, headline string) int {
	t.Helper()
	re := regexp.MustCompile(`(\d+) 个([^\s·]+)`)
	sum := 0
	for _, m := range re.FindAllStringSubmatch(headline, -1) {
		if m[2] == "号" {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad count %q", m[1])
		}
		sum += n
	}
	return sum
}

// TestQueryFallsBackToExpiredReport: when we could not re-collect AND the
// cache is older than a poll interval, the numbers may genuinely have moved, so
// the answer carries a real warning.
func TestQueryFallsBackToExpiredReport(t *testing.T) {
	r := &fakeRefresher{refreshE: context.DeadlineExceeded, last: testReport(), hasLast: true}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_4", `{"text":"@_user_1 状态"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(s.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(s.replies))
	}
	if !strings.Contains(s.replies[0], "未能取到新数据") || !strings.Contains(s.replies[0], "可能已过期") {
		t.Errorf("expired reply not warned about:\n%s", s.replies[0])
	}
}

// TestQueryFromCurrentCacheIsNotAnAlarm is the regression for the false alarm
// seen in production: a throttled or budget-limited query that falls back to a
// cache from the same minute was announced as "实时采集失败", even though the
// cached numbers were the newest that existed.
func TestQueryFromCurrentCacheIsNotAnAlarm(t *testing.T) {
	last := testReport()
	last.GeneratedAt = time.Now().Add(-90 * time.Second)
	r := &fakeRefresher{refreshE: context.DeadlineExceeded, last: last, hasLast: true}
	b, s := newTestBot(t, r)
	b.cfg.PollInterval = 15 * time.Minute

	ev := msgEvent(targetChat, "om_4b", `{"text":"@_user_1 状态"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	reply := s.replies[0]
	for _, banned := range []string{"实时采集失败", "未能取到新数据", "可能已过期"} {
		if strings.Contains(reply, banned) {
			t.Errorf("current cache reported as a failure (%q):\n%s", banned, reply)
		}
	}
	if !strings.Contains(reply, "本次未实时刷新") || !strings.Contains(reply, "约 1 分钟前") {
		t.Errorf("cached-but-current answer not described by data age:\n%s", reply)
	}
}

// TestFreshnessWording pins the three-way distinction.
func TestFreshnessWording(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	rep := func(age time.Duration) domain.Report {
		return domain.Report{GeneratedAt: now.Add(-age)}
	}
	cases := []struct {
		name       string
		cached     bool
		age        time.Duration
		wantLabel  string
		wantNotice bool
	}{
		{"live", false, 0, "实时", false},
		{"cached but current", true, 3 * time.Minute, "缓存 · 约 3 分钟前（本次未实时刷新）", false},
		{"cached and expired", true, 3 * time.Hour, "缓存 · 约 3 小时前", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := freshnessOf(rep(tc.age), tc.cached, 15*time.Minute, now)
			if f.label() != tc.wantLabel {
				t.Errorf("label = %q, want %q", f.label(), tc.wantLabel)
			}
			if got := f.notice() != ""; got != tc.wantNotice {
				t.Errorf("notice presence = %v, want %v (%q)", got, tc.wantNotice, f.notice())
			}
		})
	}
}

// TestSingleChannelQueryExpandsWindows: the folded overview is the default,
// and asking for one channel is what opens up its per-window detail.
func TestSingleChannelQueryExpandsWindows(t *testing.T) {
	low := 12.0
	rep := testReport()
	rep.Providers[0].Snapshots[0].Windows = append(rep.Providers[0].Snapshots[0].Windows,
		domain.QuotaWindow{Name: "codex/rate_limit/secondary_window", Label: "账号 · 次额度窗口", Scope: domain.ScopeAccount, UsedPercent: &low})
	r := &fakeRefresher{report: rep}
	b, s := newTestBot(t, r)

	overview := msgEvent(targetChat, "om_10", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), overview); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if strings.Contains(s.replies[0], "12.0%") {
		t.Errorf("overview should fold the non-tightest window:\n%s", s.replies[0])
	}
	if !strings.Contains(s.replies[0], "看单渠道明细：@我 codex") {
		t.Errorf("overview should say how to get the detail:\n%s", s.replies[0])
	}

	single := msgEvent(targetChat, "om_11", `{"text":"@_user_1 codex"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), single); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	detail := s.replies[1]
	for _, want := range []string{"92.5%", "12.0%", "账号 · 次额度窗口"} {
		if !strings.Contains(detail, want) {
			t.Errorf("single-channel reply missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "看单渠道明细") {
		t.Errorf("detail view should not re-offer itself:\n%s", detail)
	}
}

func TestHelpDoesNotCollect(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_5", `{"text":"@_user_1 帮助"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if r.callCount() != 0 {
		t.Fatalf("help triggered a collection: %d", r.callCount())
	}
	if len(s.replies) != 1 || !strings.Contains(s.replies[0], "不主动改启停/策略") || !strings.Contains(s.replies[0], "查询可能触发CPA自动OAuth续期") {
		t.Errorf("help reply unexpected: %v", s.replies)
	}
}

func TestUnknownIntentRepliesHint(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_6", `{"text":"@_user_1 讲个笑话"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if r.callCount() != 0 {
		t.Fatalf("unknown intent triggered a collection")
	}
	if len(s.replies) != 1 || !strings.Contains(s.replies[0], "没看懂") {
		t.Errorf("unknown intent reply unexpected: %v", s.replies)
	}
}

func TestAbnormalFilterEmpty(t *testing.T) {
	r := &fakeRefresher{report: testReport()} // codex is only StateWarning -> abnormal
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_7", `{"text":"@_user_1 异常"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(s.replies) != 1 {
		t.Fatalf("replies = %d", len(s.replies))
	}
	// Warning counts as abnormal, so the provider survives the filter.
	if !strings.Contains(s.replies[0], "Codex") {
		t.Errorf("expected Codex retained as abnormal:\n%s", s.replies[0])
	}
}

func TestCallbackRefreshReturnsCard(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	resp, err := b.HandleCardActionTrigger(context.Background(), cardEvent(targetChat, "ou_user1", render.RefreshAction))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if r.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1", r.callCount())
	}
	if resp.Toast == nil || resp.Toast.Type != "success" {
		t.Errorf("toast = %+v", resp.Toast)
	}
	if resp.Card == nil || resp.Card.Type != "card_json" {
		t.Fatalf("card response = %+v", resp.Card)
	}
	if len(s.cards) != 0 {
		t.Errorf("callback should not send a new message, got %d", len(s.cards))
	}
}

func TestCallbackTimeoutDegrades(t *testing.T) {
	r := &fakeRefresher{block: true}
	b, s := newTestBot(t, r)
	b.refreshBudget = 15 * time.Millisecond

	start := time.Now()
	resp, err := b.HandleCardActionTrigger(context.Background(), cardEvent(targetChat, "ou_user1", render.RefreshAction))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("callback took %v, must stay within budget", elapsed)
	}
	if resp.Card != nil {
		t.Fatalf("timeout must keep the original card (Card should be nil), got %+v", resp.Card)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "正在刷新") {
		t.Errorf("timeout toast unexpected: %+v", resp.Toast)
	}
	_ = s
}

// TestMessageAndCardBudgetsAreIndependent is the regression for "queries never
// get live data": the callback's 2.5-second ceiling was shared with the message
// reply, and a real collection takes several seconds, so every @Bot query fell
// back to cache.
//
// A card callback must answer inside Feishu's 3-second window. A message reply
// is sent through the message API instead, so it is not subject to that
// deadline and may wait for the collection to finish.
func TestMessageAndCardBudgetsAreIndependent(t *testing.T) {
	// The production constants are the point of the fix, so pin them.
	if cardRefreshBudget != 2500*time.Millisecond {
		t.Errorf("card budget = %v, must stay inside Feishu's 3s callback window", cardRefreshBudget)
	}
	if messageRefreshBudget < 20*time.Second {
		t.Errorf("message budget = %v, too small for a real collection", messageRefreshBudget)
	}

	// One collection duration, two paths. The message path must complete it;
	// the card path must give up and fall back.
	const collection = 120 * time.Millisecond
	newBot := func() (*Bot, *fakeSender, *fakeRefresher) {
		r := &fakeRefresher{report: testReport(), last: testReport(), hasLast: true, takes: collection}
		b, s := newTestBot(t, r)
		// Same ratio as production: the card cannot outlast the collection,
		// the message comfortably can.
		b.refreshBudget = collection / 4
		b.messageBudget = collection * 10
		return b, s, r
	}

	b, s, _ := newBot()
	ev := msgEvent(targetChat, "om_20", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(s.replies) != 1 {
		t.Fatalf("replies = %d", len(s.replies))
	}
	if !strings.Contains(s.replies[0], "· 实时") {
		t.Errorf("message reply did not get live data despite an ample budget:\n%s", s.replies[0])
	}
	for _, banned := range []string{"缓存", "未实时刷新"} {
		if strings.Contains(s.replies[0], banned) {
			t.Errorf("message reply fell back to cache (%q):\n%s", banned, s.replies[0])
		}
	}

	cb, _, _ := newBot()
	resp, err := cb.HandleCardActionTrigger(context.Background(), cardEvent(targetChat, "ou_user1", render.RefreshAction))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if resp.Card != nil {
		t.Fatalf("callback outlasted its budget instead of falling back: %+v", resp.Card)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "正在刷新") {
		t.Errorf("callback fallback toast unexpected: %+v", resp.Toast)
	}
}

// TestMessageRefreshStillRespectsTheCallerContext: the larger budget is a
// ceiling, not a licence to hang. A cancelled caller wins immediately.
func TestMessageRefreshStillRespectsTheCallerContext(t *testing.T) {
	r := &fakeRefresher{block: true, last: testReport(), hasLast: true}
	b, s := newTestBot(t, r)
	b.messageBudget = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	ev := msgEvent(targetChat, "om_21", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(ctx, ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("handler ignored the caller's deadline: %v", elapsed)
	}
	if len(s.replies) != 1 {
		t.Fatalf("replies = %d, want a cached answer rather than silence", len(s.replies))
	}
}

func TestCallbackForeignChatDenied(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, _ := newTestBot(t, r)

	resp, err := b.HandleCardActionTrigger(context.Background(), cardEvent("oc_other_group", "ou_user1", render.RefreshAction))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if r.callCount() != 0 {
		t.Fatalf("foreign-chat callback triggered refresh: %d", r.callCount())
	}
	if resp.Toast == nil || resp.Toast.Type != "error" {
		t.Errorf("foreign chat should be denied, got %+v", resp.Toast)
	}
}

func TestCallbackUnsupportedAction(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, _ := newTestBot(t, r)

	resp, err := b.HandleCardActionTrigger(context.Background(), cardEvent(targetChat, "ou_user1", "disable_credential"))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if r.callCount() != 0 {
		t.Fatalf("unsupported action triggered refresh")
	}
	if resp.Toast == nil || resp.Toast.Type != "info" {
		t.Errorf("unsupported action toast unexpected: %+v", resp.Toast)
	}
}

func TestNotifierSendsCardToConfiguredChat(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	if b.Name() != "feishu" {
		t.Fatalf("Name() = %q", b.Name())
	}
	if err := b.Notify(context.Background(), domain.Message{Kind: "daily", Report: ptrReport(testReport())}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(s.cards) != 1 {
		t.Fatalf("cards sent = %d", len(s.cards))
	}
}

func ptrReport(r domain.Report) *domain.Report { return &r }
