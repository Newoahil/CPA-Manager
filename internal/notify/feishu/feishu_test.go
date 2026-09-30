package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

const targetChat = "oc_target_group"

// --- fakes -----------------------------------------------------------------

type fakeSender struct {
	// mu guards the slices: query replies now run on detached goroutines, so
	// sends can happen concurrently.
	mu         sync.Mutex
	cards      []map[string]any
	replies    []string
	replyCards []map[string]any
	patchCards []map[string]any
	// failCards makes every send fail. failRichCards makes only cards carrying
	// a chart or collapsible_panel fail, which is how the full-then-simplified
	// fallback is exercised.
	failCards     bool
	failRichCards bool
}

func (f *fakeSender) allow(card map[string]any) error {
	b, _ := json.Marshal(card)
	s := string(b)
	if f.failCards || (f.failRichCards && (strings.Contains(s, `"chart"`) || strings.Contains(s, `"collapsible_panel"`))) {
		return errors.New("feishu: card rejected")
	}
	return nil
}

func (f *fakeSender) SendCard(_ context.Context, _ string, card map[string]any) error {
	if err := f.allow(card); err != nil {
		return err
	}
	f.mu.Lock()
	f.cards = append(f.cards, card)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) ReplyText(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	f.replies = append(f.replies, text)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) ReplyCard(_ context.Context, _ string, card map[string]any) error {
	if err := f.allow(card); err != nil {
		return err
	}
	f.mu.Lock()
	f.replyCards = append(f.replyCards, card)
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) PatchCard(_ context.Context, _ string, card map[string]any) error {
	if err := f.allow(card); err != nil {
		return err
	}
	f.mu.Lock()
	f.patchCards = append(f.patchCards, card)
	f.mu.Unlock()
	return nil
}

// counts returns the number of each kind of send so far.
func (f *fakeSender) counts() (cards, replies, replyCards int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cards), len(f.replies), len(f.replyCards)
}

// snapshotReplies returns a copy of the text replies so far.
func (f *fakeSender) snapshotReplies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.replies...)
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
		FeishuAppID:       "app",
		FeishuAppSecret:   "secret",
		FeishuChatID:      targetChat,
		Tone:              config.ToneCasual,
		CardChartsEnabled: true,
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

// waitAsync blocks until every detached query task has finished, so a test can
// assert on the reply a handler scheduled rather than on the handler's return.
func waitAsync(b *Bot) { b.tasks.wait() }

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
			Context:  &callback.Context{OpenChatID: chatID, OpenMessageID: "om_card_1"},
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
	waitAsync(b)
	if r.callCount() != 1 {
		t.Fatalf("refresh called %d times, want 1", r.callCount())
	}
	if len(s.replyCards) != 1 {
		t.Fatalf("card replies = %d, want 1 (query now answers with a card)", len(s.replyCards))
	}
	reply := jsonText(s.replyCards[0])
	for _, want := range []string{"剩余 7.5%", "最紧剩余 7.5%"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
	if strings.Contains(reply, "已上报") {
		t.Errorf("internal confidence jargon shown to the reader:\n%s", reply)
	}

	single := msgEvent(targetChat, "om_3b", `{"text":"@_user_1 codex"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), single); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	waitAsync(b)
	// The text surface (still used by webhook/logs) keeps the data source; the
	// card puts it on the footer instead of every channel line.
	text := b.buildReply(intent{kind: intentProvider, provider: domain.ProviderCodex}, testReport(), freshness{live: true})
	if !strings.Contains(text, "来源 cpa-v8") {
		t.Errorf("single-channel text lost the data source:\n%s", text)
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
	waitAsync(b)
	if len(s.replyCards) != 1 {
		t.Fatalf("card replies = %d, want 1", len(s.replyCards))
	}
	if !strings.Contains(jsonText(s.replyCards[0]), "未能取到新数据") || !strings.Contains(jsonText(s.replyCards[0]), "可能已过期") {
		t.Errorf("expired reply not warned about:\n%s", jsonText(s.replyCards[0]))
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
	waitAsync(b)
	reply := jsonText(s.replyCards[0])
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
	waitAsync(b)
	if len(s.replyCards) != 1 {
		t.Fatalf("overview card replies = %d, want 1", len(s.replyCards))
	}
	overviewJSON := jsonText(s.replyCards[0])
	// The overview is folded: a normal/warning channel shows one status line
	// and no expanded per-window panel.
	if strings.Contains(overviewJSON, "12.0%") {
		t.Errorf("overview should fold the non-tightest window:\n%s", overviewJSON)
	}
	if strings.Contains(overviewJSON, "\"expanded\":true") {
		t.Errorf("overview should not expand an abnormal panel:\n%s", overviewJSON)
	}

	single := msgEvent(targetChat, "om_11", `{"text":"@_user_1 codex"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), single); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	waitAsync(b)
	detail := jsonText(s.replyCards[1])
	for _, want := range []string{"剩余 7.5%", "88.0%", "账号 · 次额度窗口", "\"expanded\":true"} {
		if !strings.Contains(detail, want) {
			t.Errorf("single-channel card missing %q:\n%s", want, detail)
		}
	}
}

func TestHelpDoesNotCollect(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_5", `{"text":"@_user_1 帮助"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	waitAsync(b)
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
	waitAsync(b)
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
	waitAsync(b)
	if len(s.replyCards) != 1 {
		t.Fatalf("card replies = %d", len(s.replyCards))
	}
	// Warning counts as abnormal, so the provider survives the filter.
	if !strings.Contains(jsonText(s.replyCards[0]), "Codex") {
		t.Errorf("expected Codex retained as abnormal:\n%s", jsonText(s.replyCards[0]))
	}
}

func TestCallbackRefreshReturnsImmediatelyAndPatchesCard(t *testing.T) {
	r := &fakeRefresher{report: testReport(), takes: 10 * time.Millisecond}
	b, s := newTestBot(t, r)

	start := time.Now()
	resp, err := b.HandleCardActionTrigger(context.Background(), cardEvent(targetChat, "ou_user1", render.RefreshAction))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("callback took %v, must return immediately", elapsed)
	}
	if resp.Toast == nil || resp.Toast.Type != "info" || !strings.Contains(resp.Toast.Content, "正在刷新") {
		t.Errorf("toast = %+v", resp.Toast)
	}
	if resp.Card != nil {
		t.Errorf("immediate response must not carry Card, got %+v", resp.Card)
	}

	// Wait for background refresh and card patch
	waitAsync(b)
	if r.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1", r.callCount())
	}
	if len(s.patchCards) != 1 {
		t.Fatalf("patch cards = %d, want 1", len(s.patchCards))
	}
	if len(s.cards) != 0 {
		t.Errorf("callback should not send a new message, got %d", len(s.cards))
	}
}

// TestCallback6SecondsDoesNotTimeout verifies that even if quota collection takes 6s
// (> 3s callback budget), the callback responds immediately and the patch completes in background.
func TestCallback6SecondsDoesNotTimeout(t *testing.T) {
	r := &fakeRefresher{report: testReport(), takes: 100 * time.Millisecond}
	b, s := newTestBot(t, r)
	b.messageBudget = time.Second

	start := time.Now()
	resp, err := b.HandleCardActionTrigger(context.Background(), cardEvent(targetChat, "ou_user1", render.RefreshAction))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("callback took %v, must not block on collection", elapsed)
	}
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "正在刷新") {
		t.Fatalf("unexpected toast: %+v", resp.Toast)
	}

	waitAsync(b)
	if r.callCount() != 1 {
		t.Fatalf("refresh calls = %d, want 1", r.callCount())
	}
	if len(s.patchCards) != 1 {
		t.Fatalf("patch cards = %d, want 1", len(s.patchCards))
	}
}

// TestCallbackDedupPreventsDoubleTask verifies that repeated callback pushes are deduplicated.
func TestCallbackDedupPreventsDoubleTask(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := cardEvent(targetChat, "ou_user1", render.RefreshAction)
	resp1, err1 := b.HandleCardActionTrigger(context.Background(), ev)
	if err1 != nil || resp1.Toast == nil || !strings.Contains(resp1.Toast.Content, "正在刷新") {
		t.Fatalf("first callback failed: resp=%+v err=%v", resp1, err1)
	}

	resp2, err2 := b.HandleCardActionTrigger(context.Background(), ev)
	if err2 != nil || resp2.Toast == nil || !strings.Contains(resp2.Toast.Content, "已处理") {
		t.Fatalf("duplicate callback not rejected: resp=%+v err=%v", resp2, err2)
	}

	waitAsync(b)
	if r.callCount() != 1 {
		t.Fatalf("refresh called %d times, want 1", r.callCount())
	}
	if len(s.patchCards) != 1 {
		t.Fatalf("patchCards = %d, want 1", len(s.patchCards))
	}
}

// TestQueryDetachedFromEventContext: the collection runs on the Bot's own task
// context, not the event's. A cancelled event context (which Feishu cancels as
// soon as the handler returns) must not abort the in-flight query.
func TestQueryDetachedFromEventContext(t *testing.T) {
	r := &fakeRefresher{report: testReport(), takes: 60 * time.Millisecond}
	b, s := newTestBot(t, r)
	b.messageBudget = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	ev := msgEvent(targetChat, "om_21", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(ctx, ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	// Cancel the event context immediately; the async task must finish anyway.
	cancel()
	waitAsync(b)
	if _, _, replyCards := s.counts(); replyCards != 1 {
		t.Fatalf("card replies = %d, want the detached task to complete despite a cancelled event ctx", replyCards)
	}
}

// TestQueryShutdownCancelsInflight: Close cancels in-flight tasks and waits for
// them, leaving no goroutine behind.
func TestQueryShutdownCancelsInflight(t *testing.T) {
	r := &fakeRefresher{block: true, last: testReport(), hasLast: true}
	b, _ := newTestBot(t, r)
	b.messageBudget = time.Hour

	ev := msgEvent(targetChat, "om_22", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	// Close must return promptly (the blocked collect sees ctx cancellation)
	// and must not deadlock waiting for a task that ignores cancellation.
	done := make(chan struct{})
	go func() { b.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked on an in-flight task")
	}
	// After Close, a new event cannot start a task: dispatch reports dropped and
	// the dedup reservation is released.
	before := r.callCount()
	ev2 := msgEvent(targetChat, "om_23", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev2); err != nil {
		t.Fatalf("handler error after close: %v", err)
	}
	if r.callCount() != before {
		t.Error("a task started after Close")
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

// --- card rendering / fallback --------------------------------------------

// TestFullThenSimpleFallback: when the full card (chart/panel) is rejected, the
// bot must resend exactly once as the simplified card and keep the notification.
func TestFullThenSimpleFallback(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	s.failRichCards = true

	if err := b.Notify(context.Background(), domain.Message{Kind: "daily", Report: ptrReport(testReport())}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(s.cards) != 1 {
		t.Fatalf("cards accepted = %d, want 1 (the simplified one)", len(s.cards))
	}
	got := jsonText(s.cards[0])
	for _, banned := range []string{`"chart"`, `"collapsible_panel"`} {
		if strings.Contains(got, banned) {
			t.Errorf("fallback card still contains %q", banned)
		}
	}
	if !strings.Contains(got, "口径：剩余") {
		t.Errorf("fallback card lost the evidence/footer:\n%s", got)
	}
}

// TestNotifyReturnsErrorWhenBothFail: no silent success if neither variant can
// be delivered.
func TestNotifyReturnsErrorWhenBothFail(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	s.failCards = true
	if err := b.Notify(context.Background(), domain.Message{Kind: "daily", Report: ptrReport(testReport())}); err == nil {
		t.Fatal("expected an error when both card variants fail")
	}
}

// TestQuerySendsCardAndFallsBack: an @Bot data query replies with a card, and
// the fallback still delivers when the chart is rejected.
func TestQuerySendsCardAndFallsBack(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	s.failRichCards = true

	ev := msgEvent(targetChat, "om_30", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	waitAsync(b)
	// Help/unknown stay text; the data query answered as a card reply.
	if len(s.replies) != 0 {
		t.Errorf("data query should not reply with text, got %d", len(s.replies))
	}
	if len(s.replyCards) != 1 {
		t.Fatalf("card replies = %d, want 1", len(s.replyCards))
	}
	if strings.Contains(jsonText(s.replyCards[0]), `"chart"`) {
		t.Error("fallback reply card still contains a chart")
	}
}

// TestHelpStaysPlainText: help/unknown replies remain text (no data).
func TestHelpStaysPlainText(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)
	ev := msgEvent(targetChat, "om_31", `{"text":"@_user_1 帮助"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	waitAsync(b)
	if len(s.replies) != 1 || len(s.replyCards) != 0 {
		t.Errorf("help should be text only: replies=%d cards=%d", len(s.replies), len(s.replyCards))
	}
}

// TestMessageReadReceiptIsHandledQuietly: the read receipt must be swallowed
// with no error (the SDK otherwise logs "not found handler") and no audit line.
func TestMessageReadReceiptIsHandledQuietly(t *testing.T) {
	if err := handleMessageRead(context.Background(), &larkim.P2MessageReadV1{}); err != nil {
		t.Fatalf("read receipt returned an error: %v", err)
	}
	// The dispatcher must accept the registration; a duplicate or missing
	// event type would panic or leave the SDK logging errors.
	disp := dispatcher.NewEventDispatcher("", "").OnP2MessageReadV1(handleMessageRead)
	if disp == nil {
		t.Fatal("dispatcher registration returned nil")
	}
}

// TestIgnoredEventsAreSilent: every routine IM event we do not act on must have
// a registered no-op handler, or the SDK logs "not found handler" for it.
func TestIgnoredEventsAreSilent(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"bot_p2p_chat_entered", func() error {
			return ignoreP2ChatAccessEventBotP2pChatEntered(ctx, &larkim.P2ChatAccessEventBotP2pChatEnteredV1{})
		}},
		{"chat_disbanded", func() error { return ignoreP2ChatDisbandedV1(ctx, &larkim.P2ChatDisbandedV1{}) }},
		{"chat_updated", func() error { return ignoreP2ChatUpdatedV1(ctx, &larkim.P2ChatUpdatedV1{}) }},
		{"bot_added", func() error { return ignoreP2ChatMemberBotAddedV1(ctx, &larkim.P2ChatMemberBotAddedV1{}) }},
		{"bot_deleted", func() error { return ignoreP2ChatMemberBotDeletedV1(ctx, &larkim.P2ChatMemberBotDeletedV1{}) }},
		{"user_added", func() error { return ignoreP2ChatMemberUserAddedV1(ctx, &larkim.P2ChatMemberUserAddedV1{}) }},
		{"user_deleted", func() error { return ignoreP2ChatMemberUserDeletedV1(ctx, &larkim.P2ChatMemberUserDeletedV1{}) }},
		{"user_withdrawn", func() error { return ignoreP2ChatMemberUserWithdrawnV1(ctx, &larkim.P2ChatMemberUserWithdrawnV1{}) }},
		{"message_recalled", func() error { return ignoreP2MessageRecalledV1(ctx, &larkim.P2MessageRecalledV1{}) }},
		{"reaction_created", func() error { return ignoreP2MessageReactionCreatedV1(ctx, &larkim.P2MessageReactionCreatedV1{}) }},
		{"reaction_deleted", func() error { return ignoreP2MessageReactionDeletedV1(ctx, &larkim.P2MessageReactionDeletedV1{}) }},
	}
	for _, tc := range cases {
		if err := tc.fn(); err != nil {
			t.Errorf("ignored handler %s returned an error: %v", tc.name, err)
		}
	}

	// All of them register on one dispatcher without panicking on a duplicate
	// event type.
	disp := dispatcher.NewEventDispatcher("", "").
		OnP2ChatAccessEventBotP2pChatEnteredV1(ignoreP2ChatAccessEventBotP2pChatEntered).
		OnP2ChatDisbandedV1(ignoreP2ChatDisbandedV1).
		OnP2ChatUpdatedV1(ignoreP2ChatUpdatedV1).
		OnP2ChatMemberBotAddedV1(ignoreP2ChatMemberBotAddedV1).
		OnP2ChatMemberBotDeletedV1(ignoreP2ChatMemberBotDeletedV1).
		OnP2ChatMemberUserAddedV1(ignoreP2ChatMemberUserAddedV1).
		OnP2ChatMemberUserDeletedV1(ignoreP2ChatMemberUserDeletedV1).
		OnP2ChatMemberUserWithdrawnV1(ignoreP2ChatMemberUserWithdrawnV1).
		OnP2MessageRecalledV1(ignoreP2MessageRecalledV1).
		OnP2MessageReactionCreatedV1(ignoreP2MessageReactionCreatedV1).
		OnP2MessageReactionDeletedV1(ignoreP2MessageReactionDeletedV1)
	if disp == nil {
		t.Fatal("combined registration returned nil")
	}
}

// jsonText renders a card map as compact JSON so a test can assert on the
// serialized content (the same bytes the sender would put on the wire).
func jsonText(card map[string]any) string {
	b, _ := json.Marshal(card)
	return string(b)
}
