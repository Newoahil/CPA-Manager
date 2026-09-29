package feishu

import (
	"context"
	"strings"
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
	calls    int
	report   domain.Report
	last     domain.Report
	hasLast  bool
	refreshE error
	block    bool
}

func (f *fakeRefresher) RefreshNow(ctx context.Context) (domain.Report, error) {
	f.calls++
	if f.block {
		<-ctx.Done()
		return domain.Report{}, ctx.Err()
	}
	if f.refreshE != nil {
		return domain.Report{}, f.refreshE
	}
	return f.report, nil
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
			Healthy:     1,
			Total:       2,
			WorstState:  domain.StateWarning,
			BestWindows: []domain.QuotaWindow{{Name: "5h", UsedPercent: &used, ResetAt: &reset}},
			Snapshots: []domain.QuotaSnapshot{{
				Credential: cred,
				Windows:    []domain.QuotaWindow{{Name: "5h", UsedPercent: &used, ResetAt: &reset}},
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
	if len(s.replies) != 0 || r.calls != 0 {
		t.Fatalf("foreign chat handled: replies=%d refreshCalls=%d", len(s.replies), r.calls)
	}
}

func TestNonMentionIgnored(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_2", `{"text":"额度"}`, larkim.MsgTypeText, nil)
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(s.replies) != 0 || r.calls != 0 {
		t.Fatalf("non-mentioned message was handled: replies=%d refreshCalls=%d", len(s.replies), r.calls)
	}
}

func TestQueryRepliesWithEvidence(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_3", `{"text":"@_user_1 额度"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if r.calls != 1 {
		t.Fatalf("refresh called %d times, want 1", r.calls)
	}
	if len(s.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(s.replies))
	}
	reply := s.replies[0]
	for _, want := range []string{"92.5%", "cpa-v8", "5h"} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q:\n%s", want, reply)
		}
	}
}

func TestQueryFallsBackToLastReport(t *testing.T) {
	r := &fakeRefresher{refreshE: context.DeadlineExceeded, last: testReport(), hasLast: true}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_4", `{"text":"@_user_1 状态"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if len(s.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(s.replies))
	}
	if !strings.Contains(s.replies[0], "数据可能过期") {
		t.Errorf("stale reply not annotated:\n%s", s.replies[0])
	}
}

func TestHelpDoesNotCollect(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, s := newTestBot(t, r)

	ev := msgEvent(targetChat, "om_5", `{"text":"@_user_1 帮助"}`, larkim.MsgTypeText, []*larkim.MentionEvent{botMention()})
	if err := b.HandleMessageV1(context.Background(), ev); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if r.calls != 0 {
		t.Fatalf("help triggered a collection: %d", r.calls)
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
	if r.calls != 0 {
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
	if r.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", r.calls)
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

func TestCallbackForeignChatDenied(t *testing.T) {
	r := &fakeRefresher{report: testReport()}
	b, _ := newTestBot(t, r)

	resp, err := b.HandleCardActionTrigger(context.Background(), cardEvent("oc_other_group", "ou_user1", render.RefreshAction))
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}
	if r.calls != 0 {
		t.Fatalf("foreign-chat callback triggered refresh: %d", r.calls)
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
	if r.calls != 0 {
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
