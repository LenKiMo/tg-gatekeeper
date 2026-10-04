package gatekeeper

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
	"github.com/LenKiMo/tg-gatekeeper/internal/store"
	"github.com/LenKiMo/tg-gatekeeper/internal/telegram"
)

// ---------------------------------------------------------------- 假实现

type apiCall struct {
	name    string
	chatID  int64
	userID  int64
	message int64
	text    string
	buttons int      // 按钮个数
	labels  []string // 按钮文案（按行展开，用于核对最后一行是不是放行/封禁）
}

type fakeAPI struct {
	restricted  []apiCall
	restored    []apiCall
	banned      []apiCall
	unbanned    []apiCall
	approved    []apiCall
	declined    []apiCall
	deleted     []apiCall
	challenges  []apiCall
	texts       []apiCall
	tags        []apiCall
	bioErr      error
	nextMessage int64
	admins      map[int64]bool
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{nextMessage: 1000, admins: map[int64]bool{}}
}

func (f *fakeAPI) next() int64 {
	f.nextMessage++
	return f.nextMessage
}

func (f *fakeAPI) RestrictNoMessages(_ context.Context, chatID, userID int64) error {
	f.restricted = append(f.restricted, apiCall{name: "restrict", chatID: chatID, userID: userID})
	return nil
}

func (f *fakeAPI) RestorePermissions(_ context.Context, chatID, userID int64) error {
	f.restored = append(f.restored, apiCall{name: "restore", chatID: chatID, userID: userID})
	return nil
}

func (f *fakeAPI) Ban(_ context.Context, chatID, userID int64, _ bool) error {
	f.banned = append(f.banned, apiCall{name: "ban", chatID: chatID, userID: userID})
	return nil
}

func (f *fakeAPI) Unban(_ context.Context, chatID, userID int64, _ bool) error {
	f.unbanned = append(f.unbanned, apiCall{name: "unban", chatID: chatID, userID: userID})
	return nil
}

func (f *fakeAPI) ApproveJoinRequest(_ context.Context, chatID, userID int64) error {
	f.approved = append(f.approved, apiCall{name: "approve", chatID: chatID, userID: userID})
	return nil
}

func (f *fakeAPI) DeclineJoinRequest(_ context.Context, chatID, userID int64) error {
	f.declined = append(f.declined, apiCall{name: "decline", chatID: chatID, userID: userID})
	return nil
}

func (f *fakeAPI) SendChallenge(_ context.Context, chatID int64, _ ports.ResolvedImage,
	_ string, buttons ports.ButtonGrid) (domain.MessageRef, error) {
	count, labels := 0, make([]string, 0, 8)
	for _, row := range buttons {
		count += len(row)
		for _, btn := range row {
			labels = append(labels, btn.Text)
		}
	}
	id := f.next()
	f.challenges = append(f.challenges, apiCall{
		name: "challenge", chatID: chatID, message: id,
		text: string(rune('0' + count)), buttons: count, labels: labels,
	})
	return domain.MessageRef{ChatID: chatID, MessageID: id}, nil
}

func (f *fakeAPI) SendText(_ context.Context, chatID int64, text string, _ bool, buttons ports.ButtonGrid) (domain.MessageRef, error) {
	id := f.next()
	count := 0
	for _, row := range buttons {
		count += len(row)
	}
	f.texts = append(f.texts, apiCall{name: "text", chatID: chatID, message: id, text: text, buttons: count})
	return domain.MessageRef{ChatID: chatID, MessageID: id}, nil
}

func (f *fakeAPI) DeleteMessage(_ context.Context, ref domain.MessageRef) error {
	f.deleted = append(f.deleted, apiCall{name: "delete", chatID: ref.ChatID, message: ref.MessageID})
	if ref.MessageID == 66 {
		return errFakePermanent
	}
	if ref.MessageID == 55 {
		return errFakeTransient
	}
	return nil
}

func (f *fakeAPI) AnswerCallback(_ context.Context, _, _ string, _ bool) error { return nil }

func (f *fakeAPI) GetBio(_ context.Context, userID int64) (string, error) {
	if f.bioErr != nil {
		return "", f.bioErr
	}
	return "", nil
}

func (f *fakeAPI) IsAdminWithPermissions(_ context.Context, _ int64, userID int64, _ uint16) (bool, error) {
	return f.admins[userID], nil
}

func (f *fakeAPI) GetMemberStatus(_ context.Context, _, _ int64) (string, error) {
	return "member", nil
}

func (f *fakeAPI) SetMemberTag(_ context.Context, chatID, userID int64, tag string) error {
	f.tags = append(f.tags, apiCall{name: "tag", chatID: chatID, userID: userID, text: tag})
	return nil
}

func (f *fakeAPI) Self() (int64, string) { return 42, "gatekeeper_bot" }
func (f *fakeAPI) Close() error          { return nil }

var (
	errFakePermanent = errors.New("message to delete not found")
	errFakeTransient = errors.New("telegram 500 internal server error")
)

type fakeSource struct{ snap domain.Snapshot }

func (f *fakeSource) ID() string { return f.snap.SourceID }
func (f *fakeSource) Snapshot(context.Context) (domain.Snapshot, error) {
	return f.snap, nil
}
func (f *fakeSource) Close() error { return nil }

// ---------------------------------------------------------------- 测试脚手架

func dataPNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func testService(t *testing.T, mutate func(*config.Config)) (*Service, *fakeAPI, *store.MemRegistry, *store.MemDeletionQueue) {
	t.Helper()
	cfg := config.Default()
	cfg.Gatekeeper.OptionCount = 4
	cfg.Gatekeeper.MinOptionCount = 2
	cfg.Gatekeeper.TimeoutSeconds = 60
	cfg.Gatekeeper.KickUnbanAfterSeconds = 30
	cfg.Gatekeeper.CaptionJoin = "欢迎 {mention}，{timeout} 秒内作答。"
	cfg.Commands.WelcomeDeleteAfterSeconds = 60
	// 测试不要往仓库里写图片缓存：默认路径是相对 CWD 的 ./data/cache/images，
	// 而 go test 的 CWD 是包目录。
	cfg.Images.Cache.Enabled = false
	cfg.Images.Cache.Dir = t.TempDir()
	if mutate != nil {
		mutate(cfg)
	}

	ref := dataPNG(t)
	entries := []domain.Entry{
		{ID: "a", Label: "甲", ImageRef: ref, Pool: "p", Enabled: true},
		{ID: "b", Label: "乙", ImageRef: ref, Pool: "p", Enabled: true},
		{ID: "c", Label: "丙", ImageRef: ref, Pool: "p", Enabled: true},
		{ID: "d", Label: "丁", ImageRef: ref, Pool: "p", Enabled: true},
		{ID: "e", Label: "戊", ImageRef: ref, Pool: "p", Enabled: true},
	}
	api := newFakeAPI()
	registry := store.NewMemRegistry()
	deletion := store.NewMemDeletionQueue()
	groups := store.NewMemGroupStore()

	svc, err := New(Deps{
		Config:   cfg,
		API:      api,
		Groups:   groups,
		Registry: registry,
		Deletion: deletion,
		Dispatch: nil,
		Audit:    nil,
		Provider: &fakeSource{snap: domain.Snapshot{SourceID: "test", Entries: entries, LoadedAt: time.Now()}},
		Logger:   slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RefreshDataset(context.Background()); err != nil {
		t.Fatalf("加载题库失败: %v", err)
	}
	return svc, api, registry, deletion
}

// correctToken 从会话里找出正确答案对应的 token（仅测试可用：生产不存明文）。
func correctToken(t *testing.T, sess domain.Session) string {
	t.Helper()
	for _, o := range sess.Options {
		if domain.MatchesHash(o.Token, sess.CorrectTokenHash) {
			return o.Token
		}
	}
	t.Fatal("会话里找不到正确答案 token")
	return ""
}

func wrongToken(t *testing.T, sess domain.Session) string {
	t.Helper()
	for _, o := range sess.Options {
		if !domain.MatchesHash(o.Token, sess.CorrectTokenHash) {
			return o.Token
		}
	}
	t.Fatal("会话里找不到错误选项 token")
	return ""
}

// runBackground 手动推进后台循环（测试里不用 ticker，保证确定性）。
func runBackground(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	svc.scanExpired(ctx)
	svc.runDueEffects(ctx)
	svc.processDeletions(ctx, time.Minute)
}

// ---------------------------------------------------------------- 测试

// welcomeOf 取出欢迎语。
//
// 普通入群模式现在也会发"管理员处置卡片"，它同样走 SendText，所以欢迎语不一定是 texts[0]。
func welcomeOf(t *testing.T, api *fakeAPI) string {
	t.Helper()
	for _, x := range api.texts {
		if x.buttons == 0 && strings.Contains(x.text, "欢迎") {
			return x.text
		}
	}
	t.Fatalf("没有找到欢迎语，实际 %+v", api.texts)
	return ""
}

// TestJoinModeChallengeCarriesAdminButtons 验证入群模式的放行/封禁键就在题目卡片上：
// 选项 + 末行两个管理员键（同一张卡片），并且不再额外发一条管理员消息。
func TestJoinModeChallengeCarriesAdminButtons(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()
	if err := svc.OnMembersJoined(ctx, -100, 1, []Member{{UserID: 7, DisplayName: "新人"}}); err != nil {
		t.Fatalf("处理入群失败: %v", err)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("应当发出 1 道题，实际 %d", len(api.challenges))
	}
	if n := api.challenges[0].buttons; n != svc.cfg.Gatekeeper.OptionCount+2 {
		t.Fatalf("题目卡片应有 %d 个按钮（选项+封禁+放行），实际 %d",
			svc.cfg.Gatekeeper.OptionCount+2, n)
	}
	labels := api.challenges[0].labels
	if len(labels) < 2 || labels[len(labels)-2] != "🚫 封禁" || labels[len(labels)-1] != "✅ 放行" {
		t.Fatalf("题目卡片最后一行应为 封禁/放行，实际 %v", labels)
	}
	// 关键：不再另发一条管理员消息（那样在群里是冗余的）。
	if len(api.texts) != 0 {
		t.Fatalf("入群模式不应再额外发消息，实际 %+v", api.texts)
	}
}

// TestJoinModeAdminButtonsStillEnforceRights 验证题目卡片上的管理员键同样只认管理员：
// 普通成员点"放行"必须被拒。
func TestJoinModeAdminButtonsStillEnforceRights(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()
	if err := svc.OnMembersJoined(ctx, -100, 1, []Member{{UserID: 7, DisplayName: "新人"}}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	// user 7 自己（非管理员）去点"放行"：应当被拒，且不改状态、不恢复权限。
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb-self", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAdmin(sess.ID, false),
	}); err != nil {
		t.Fatalf("非管理员点击不应报错，只应被拒: %v", err)
	}
	runBackground(t, svc)
	if len(api.restored) != 0 {
		t.Fatalf("非管理员不得放行，实际 %+v", api.restored)
	}
	now, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatalf("会话应仍在进行中: %v", err)
	}
	if now.State != domain.StatePending {
		t.Fatalf("会话状态应保持 pending，实际 %s", now.State)
	}
}

// TestJoinModeAdminPassUnblocks 验证入群模式下管理员点"放行"能直接放行并发欢迎语。
func TestJoinModeAdminPassUnblocks(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Welcome = "欢迎{mention}"
	})
	ctx := context.Background()
	if err := svc.OnMembersJoined(ctx, -100, 1, []Member{{UserID: 7, DisplayName: "新人"}}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	// 管理员（user 99）点放行：注意判定不依赖"是不是本人"。
	api.admins[99] = true
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb-admin", ChatID: -100, UserID: 99, DisplayName: "管理",
		Data: telegram.EncodeAdmin(sess.ID, false),
	}); err != nil {
		t.Fatalf("管理员放行失败: %v", err)
	}
	runBackground(t, svc)
	if len(api.restored) != 1 {
		t.Fatalf("放行后应恢复权限，实际 %+v", api.restored)
	}
	if got := welcomeOf(t, api); !strings.Contains(got, "欢迎") {
		t.Fatalf("放行后应发欢迎语，实际 %+v", api.texts)
	}
	// 终态会话已从"活动索引"里摘除（MemRegistry 的行为），用 GetByID 读回来核对状态。
	after, err := registry.GetByID(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.StateAdminPass {
		t.Fatalf("会话状态应为 admin_pass，实际 %s", after.State)
	}
}

// TestDeleteJoinMessageSwitch 验证开关关闭后不再删"XX 加入群组"服务消息。
func TestDeleteJoinMessageSwitch(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.DeleteJoinMessage = false
	})
	ctx := context.Background()
	join := domain.MessageRef{ChatID: -100, MessageID: 900}
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, join); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	// 答对 → 终态 → 应当只删题目消息，不删入群消息。
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb-keep", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	runBackground(t, svc)
	for _, d := range api.deleted {
		if d.message == join.MessageID {
			t.Fatalf("关闭 delete_join_message 后不应删入群消息，实际删了 %+v", api.deleted)
		}
	}
}

// TestWelcomeNicknameAndRulesLink 覆盖欢迎语的两个真实问题：
//  1. {mention} 必须用入群时记录的真实昵称（不能用占位文字"新成员"）；
//  2. 群规消息存在、且模板没自带链接时，追加的是一条 MarkdownV2 链接（可点击）。
func TestWelcomeNicknameAndRulesLink(t *testing.T) {
	const chatID int64 = -1001796791307
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Welcome = "欢迎{mention}"
	})
	ctx := context.Background()

	group, err := svc.Group(ctx, chatID)
	if err != nil {
		t.Fatalf("读取群配置失败: %v", err)
	}
	group.RulesMessageID = 37057
	if err := svc.SaveGroup(ctx, group); err != nil {
		t.Fatalf("保存群配置失败: %v", err)
	}

	if err := svc.OnMembersJoined(ctx, chatID, 1, []Member{{UserID: 7, DisplayName: "漠伦"}}); err != nil {
		t.Fatalf("处理入群失败: %v", err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: chatID, UserID: 7})
	if err != nil {
		t.Fatalf("会话应当存在: %v", err)
	}
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb-welcome", ChatID: chatID, UserID: 7, DisplayName: "漠伦",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatalf("答题失败: %v", err)
	}
	runBackground(t, svc) // Claim 只把副作用写进 outbox，跑一次 worker 才真正发送
	got := welcomeOf(t, api)
	if !strings.Contains(got, "漠伦") || !strings.Contains(got, "tg://user?id=7") {
		t.Fatalf("欢迎语应包含真实昵称的可点击提及，实际 %q", got)
	}
	want := "[点击阅读](https://t.me/c/1796791307/37057)"
	if !strings.Contains(got, want) {
		t.Fatalf("欢迎语应包含群规链接 %s，实际 %q", want, got)
	}
}

// TestWelcomeTemplateLinkIsNotDuplicated 验证模板自己写了链接时不再追加群规链接。
func TestWelcomeTemplateLinkIsNotDuplicated(t *testing.T) {
	const chatID int64 = -1001796791307
	const link = "https://t.me/ArknightsEndfieldCN/37057"
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Welcome = "欢迎{mention}\n建议阅读群公约：[点击阅读](" + link + ")"
	})
	ctx := context.Background()
	group, err := svc.Group(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	group.RulesMessageID = 37057
	if err := svc.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnMembersJoined(ctx, chatID, 1, []Member{{UserID: 7, DisplayName: "漠伦"}}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: chatID, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb-link", ChatID: chatID, UserID: 7, DisplayName: "漠伦",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	runBackground(t, svc)
	got := welcomeOf(t, api)
	if n := strings.Count(got, "t.me/"); n != 1 {
		t.Fatalf("模板自带链接时不应再追加群规链接（t.me 出现 %d 次）: %q", n, got)
	}
	if strings.Contains(got, "t.me/c/") {
		t.Fatalf("不应出现自动追加的 t.me/c 链接: %q", got)
	}
}

// TestDuplicateJoinSignalDoesNotSendSecondChallenge 是生产事故的第二道回归。
//
// 入群信号有两条通道（new_chat_members 服务消息 + chat_member 状态跃迁），Telegram 可能
// 两条都发。同一用户第二次信号必须被忽略：否则会同时挂两道题，旧题没人清理。
func TestDuplicateJoinSignalDoesNotSendSecondChallenge(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()
	members := []Member{{UserID: 7, DisplayName: "新人"}}

	if err := svc.OnMembersJoined(ctx, -100, 1, members); err != nil {
		t.Fatalf("第一次入群失败: %v", err)
	}
	if err := svc.OnMembersJoined(ctx, -100, 2, members); err != nil {
		t.Fatalf("第二次入群信号应被忽略而不是报错: %v", err)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("同一用户只应出 1 道题，实际 %d", len(api.challenges))
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatalf("会话应当仍在: %v", err)
	}
	if sess.UpdateID != 1 {
		t.Fatalf("会话不应被第二条信号替换，update_id = %d", sess.UpdateID)
	}
}

// TestJoinFlowRestrictsAndSendsChallenge 验证入群后先禁言再出题，并登记会话。
func TestJoinFlowRestrictsAndSendsChallenge(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()

	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{ChatID: -100, MessageID: 900}); err != nil {
		t.Fatalf("处理入群失败: %v", err)
	}
	if len(api.restricted) != 1 || api.restricted[0].userID != 7 {
		t.Fatalf("应当禁言新成员，实际 %+v", api.restricted)
	}
	if len(api.challenges) != 1 {
		t.Fatalf("应当发出 1 道题，实际 %d", len(api.challenges))
	}
	// 题目按钮数量 = 配置的选项数。
	// 入群模式的题目卡片 = 选项 + 末行两个管理员键（封禁/放行）。
	if got := api.challenges[0].text; got != string(rune('0'+svc.cfg.Gatekeeper.OptionCount+2)) {
		t.Fatalf("按钮数量异常: %q", got)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatalf("会话应当存在: %v", err)
	}
	if sess.State != domain.StatePending {
		t.Fatalf("会话状态 = %s，期望 pending", sess.State)
	}
	if !sess.ChallengeMessage.Valid() || !sess.JoinMessage.Valid() {
		t.Fatalf("会话应当记录题目消息与入群消息: %+v", sess)
	}
	if len(sess.Options) != svc.cfg.Gatekeeper.OptionCount {
		t.Fatalf("选项数 = %d", len(sess.Options))
	}
}

// TestCorrectAnswerPasses 验证答对后恢复权限、发欢迎语并清理消息。
func TestCorrectAnswerPasses(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Welcome = "欢迎 {mention} 加入！"
	})
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{ChatID: -100, MessageID: 900}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	token := correctToken(t, sess)

	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb1", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAnswer(sess.ID, token),
	}); err != nil {
		t.Fatalf("处理回调失败: %v", err)
	}
	// Claim 已经把副作用写进 outbox；跑一次 worker 让它们生效。
	runBackground(t, svc)

	if len(api.restored) != 1 || api.restored[0].userID != 7 {
		t.Fatalf("答对后应当恢复权限，实际 %+v", api.restored)
	}
	if len(api.banned) != 0 {
		t.Fatalf("答对不应封禁: %+v", api.banned)
	}
	if got := welcomeOf(t, api); !bytes.Contains([]byte(got), []byte("欢迎")) {
		t.Fatalf("应当发送欢迎语，实际 %+v", api.texts)
	}
	// 昵称必须是新成员本人：曾经因为渲染时传空字符串而退化成"新成员"占位。
	if got := welcomeOf(t, api); !strings.Contains(got, "新人") || !strings.Contains(got, "tg://user?id=7") {
		t.Fatalf("欢迎语的提及应带上真实昵称，实际 %q", got)
	} else if strings.Contains(got, "新成员") {
		t.Fatalf("欢迎语不该出现占位昵称，实际 %q", got)
	}
	// 题目消息与入群消息都应当被清理。
	if len(api.deleted) < 2 {
		t.Fatalf("应当删除题目消息与入群消息，实际 %+v", api.deleted)
	}
}

// TestWrongAnswerKicksAndSchedulesUnban 验证答错后踢出，并在配置时间后解封。
func TestWrongAnswerKicksAndSchedulesUnban(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb1", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAnswer(sess.ID, wrongToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	runBackground(t, svc)

	if len(api.banned) != 1 {
		t.Fatalf("答错应当封禁，实际 %+v", api.banned)
	}
	if len(api.restored) != 0 {
		t.Fatalf("答错不应恢复权限: %+v", api.restored)
	}
	if len(api.unbanned) != 0 {
		t.Fatalf("解封尚未到点，不应执行: %+v", api.unbanned)
	}
	// 到点之后再跑一次，解封生效。
	time.Sleep(10 * time.Millisecond)
	svc.runDueEffects(ctx)
	if len(api.unbanned) != 0 {
		t.Fatalf("解封时间未到（30 秒）不应执行: %+v", api.unbanned)
	}
}

// TestTimeoutKicks 验证超时踢出（启动前错过的超时也能被扫描补上）。
func TestTimeoutKicks(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.TimeoutSeconds = 1
	})
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	// 把 deadline 拨到过去，模拟"进程重启后才扫到这条会话"。
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	sess.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := registry.AttachMessages(ctx, sess.Key, sess.ID, sess.ChallengeMessage, domain.MessageRef{},
		sess.ExpiresAt, sess.Version); err != nil {
		t.Fatalf("回写过期时间失败: %v", err)
	}
	runBackground(t, svc)

	if len(api.banned) != 1 {
		t.Fatalf("超时应当踢出，实际 %+v", api.banned)
	}
	if _, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7}); err == nil {
		t.Fatal("超时后不应还留有活动会话")
	}
}

// TestPendingUserSpeakingIsPunished 验证"待验证用户发言"会被删消息 + 踢出。
//
// 未通过验证就发言必须被立刻处置；本测试确保这条拦截真的生效。
func TestPendingUserSpeakingIsPunished(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	pending, err := svc.HasPendingSession(ctx, -100, 7)
	if err != nil || !pending {
		t.Fatalf("应当处于待验证状态: pending=%v err=%v", pending, err)
	}
	handled, err := svc.OnGroupMessage(ctx, -100, 7, 555, "新人")
	if err != nil || !handled {
		t.Fatalf("待验证用户的发言应当被拦截: handled=%v err=%v", handled, err)
	}
	runBackground(t, svc)

	if len(api.banned) != 1 {
		t.Fatalf("待验证期间发言应当被踢出，实际 %+v", api.banned)
	}
	found := false
	for _, d := range api.deleted {
		if d.message == 555 {
			found = true
		}
	}
	if !found {
		t.Fatalf("应当删除该用户发言，实际 %+v", api.deleted)
	}
	if pending, _ := svc.HasPendingSession(ctx, -100, 7); pending {
		t.Fatal("处置后不应还留有活动会话")
	}
}

// TestNonPendingUserMessageIsIgnored 验证普通成员发言不受影响。
func TestNonPendingUserMessageIsIgnored(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()
	handled, err := svc.OnGroupMessage(ctx, -100, 99, 700, "老成员")
	if err != nil || handled {
		t.Fatalf("普通成员发言不应被拦截: handled=%v err=%v", handled, err)
	}
	if len(api.banned) != 0 {
		t.Fatalf("普通成员不应被封禁: %+v", api.banned)
	}
}

// TestAdWordRejectsNewMember 验证昵称命中广告词时直接封禁且不出题。
func TestAdWordRejectsNewMember(t *testing.T) {
	svc, api, _, _ := testService(t, func(cfg *config.Config) {
		cfg.AdFilter.GlobalWords = []string{"代肝"}
	})
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 8, DisplayName: "专业代　肝"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 0 {
		t.Fatalf("广告号不应收到验证题: %+v", api.challenges)
	}
	if len(api.banned) != 1 || api.banned[0].userID != 8 {
		t.Fatalf("广告号应当被直接封禁，实际 %+v", api.banned)
	}
}

// TestBotAndAdminAndDisabledGroupSkipped 验证跳过策略。
func TestBotAndAdminAndDisabledGroupSkipped(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()

	// bot 成员按策略放行。
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 9, DisplayName: "某机器人", IsBot: true}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	// 管理员按策略放行。
	api.admins[10] = true
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 2,
		[]Member{{UserID: 10, DisplayName: "管理员"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	if len(api.restricted) != 0 || len(api.challenges) != 0 {
		t.Fatalf("bot/管理员不应被验证: restricted=%+v challenges=%+v", api.restricted, api.challenges)
	}

	// 关闭验证的群不出题。
	group, err := svc.Group(ctx, -200)
	if err != nil {
		t.Fatal(err)
	}
	group.Enabled = false
	if err := svc.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -200, 3,
		[]Member{{UserID: 11, DisplayName: "路人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	if len(api.challenges) != 0 {
		t.Fatalf("关闭验证的群不应出题: %+v", api.challenges)
	}
}

// TestRequestModeApprovesOnCorrectAnswer 验证申请模式：私聊出题 + 群内管理卡片 + 答对放行。
func TestRequestModeApprovesOnCorrectAnswer(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Mode = string(domain.VerifyModeRequest)
	})
	ctx := context.Background()
	group, err := svc.Group(ctx, -300)
	if err != nil {
		t.Fatal(err)
	}
	group.Mode = domain.VerifyModeRequest
	if err := svc.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}

	if err := svc.OnJoinRequest(ctx, JoinRequest{
		ChatID: -300, UserID: 20, UserChatID: 20, DisplayName: "申请者", UpdateID: 5,
	}); err != nil {
		t.Fatal(err)
	}
	// 题目应发到申请者私聊，管理员卡片发到群里。
	if len(api.challenges) != 1 || api.challenges[0].chatID != 20 {
		t.Fatalf("题目应发到申请者私聊: %+v", api.challenges)
	}
	if len(api.texts) != 1 || api.texts[0].chatID != -300 {
		t.Fatalf("应当给群里发管理员卡片: %+v", api.texts)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -300, UserID: 20})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Mode != domain.SessionRequest || !sess.AdminMessage.Valid() {
		t.Fatalf("会话应当记录申请模式与管理员卡片: %+v", sess)
	}
	// 私聊里的点击：chatID 是申请者私聊，但会话 key 仍是群。
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb2", ChatID: 20, UserID: 20, DisplayName: "申请者",
		Data: telegram.EncodeAnswer(sess.ID, correctToken(t, sess)),
	}); err != nil {
		t.Fatal(err)
	}
	runBackground(t, svc)

	if len(api.approved) != 1 || api.approved[0].userID != 20 {
		t.Fatalf("答对应通过申请，实际 %+v", api.approved)
	}
	if len(api.restored) != 0 {
		t.Fatalf("申请模式不应调用禁言/恢复权限: %+v", api.restored)
	}
}

// TestAdminBanCancelsTemporaryUnban 验证管理员封禁优先于临时解封。
func TestAdminBanCancelsTemporaryUnban(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	api.admins[999] = true
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb3", ChatID: -100, UserID: 999, DisplayName: "管理员",
		Data: telegram.EncodeAdmin(sess.ID, true),
	}); err != nil {
		t.Fatal(err)
	}
	runBackground(t, svc)

	if len(api.banned) != 1 {
		t.Fatalf("管理员封禁应当生效: %+v", api.banned)
	}
	if len(api.unbanned) != 0 {
		t.Fatalf("管理员封禁后不应当有解封: %+v", api.unbanned)
	}
}

// TestCorrectAnswerAfterDeadlineIsRejected 验证到点后的答题不会放行。
func TestCorrectAnswerAfterDeadlineIsRejected(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.Gatekeeper.TimeoutSeconds = 1
	})
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	token := correctToken(t, sess)
	sess.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := registry.AttachMessages(ctx, sess.Key, sess.ID, sess.ChallengeMessage, domain.MessageRef{},
		sess.ExpiresAt, sess.Version); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb4", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAnswer(sess.ID, token),
	}); err != nil {
		t.Fatal(err)
	}
	if len(api.restored) != 0 {
		t.Fatalf("迟到答对不应放行: %+v", api.restored)
	}
	runBackground(t, svc)
	if len(api.banned) != 1 {
		t.Fatalf("超时应当踢出: %+v", api.banned)
	}
}

// TestStaleCallbackIgnored 验证旧题按钮不会作用到新会话。
func TestStaleCallbackIgnored(t *testing.T) {
	svc, api, registry, _ := testService(t, nil)
	ctx := context.Background()
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 1,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	old, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	// 用户退群后重新加入：新一代会话出现。
	if err := svc.OnMemberLeft(ctx, -100, 7); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnMembersJoinedWithJoinMessage(ctx, -100, 2,
		[]Member{{UserID: 7, DisplayName: "新人"}}, domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	api.challenges = nil
	// 点旧题的按钮。
	if err := svc.OnCallback(ctx, CallbackEvent{
		ID: "cb5", ChatID: -100, UserID: 7, DisplayName: "新人",
		Data: telegram.EncodeAnswer(old.ID, old.Options[0].Token),
	}); err != nil {
		t.Fatal(err)
	}
	if len(api.restored) != 0 {
		t.Fatalf("旧题按钮不应放行: %+v", api.restored)
	}
	cur, err := registry.Get(ctx, domain.SessionKey{ChatID: -100, UserID: 7})
	if err != nil {
		t.Fatalf("新会话应当仍然有效: %v", err)
	}
	if cur.ID == old.ID {
		t.Fatal("新会话应当是新的 session id")
	}
	if cur.State != domain.StatePending {
		t.Fatalf("新会话状态 = %s，期望 pending", cur.State)
	}
}

// TestCommandsRequireAdmin 验证管理命令的权限控制。
func TestCommandsRequireAdmin(t *testing.T) {
	svc, api, _, _ := testService(t, nil)
	ctx := context.Background()
	if err := svc.HandleCommand(ctx, -100, 7, "普通成员", "/request_mode", "/request_mode request", domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	group, err := svc.Group(ctx, -100)
	if err != nil {
		t.Fatal(err)
	}
	if group.Mode == domain.VerifyModeRequest {
		t.Fatal("非管理员不应能切换验证模式")
	}
	if len(api.texts) == 0 || !bytes.Contains([]byte(api.texts[len(api.texts)-1].text), []byte("管理员")) {
		t.Fatalf("应当提示需要管理员权限: %+v", api.texts)
	}

	// 管理员可以切换。
	api.admins[8] = true
	if err := svc.HandleCommand(ctx, -100, 8, "管理员", "/request_mode", "/request_mode request", domain.MessageRef{}); err != nil {
		t.Fatal(err)
	}
	group, err = svc.Group(ctx, -100)
	if err != nil {
		t.Fatal(err)
	}
	if group.Mode != domain.VerifyModeRequest {
		t.Fatalf("管理员切换后模式 = %s", group.Mode)
	}
}

// TestTimeoutThenRequestModeDeclines 验证申请模式超时会被拒绝。
func TestTimeoutThenRequestModeDeclines(t *testing.T) {
	svc, api, registry, _ := testService(t, func(cfg *config.Config) {
		cfg.GroupDefaults.Mode = string(domain.VerifyModeRequest)
		cfg.Gatekeeper.TimeoutSeconds = 1
	})
	ctx := context.Background()
	group, err := svc.Group(ctx, -400)
	if err != nil {
		t.Fatal(err)
	}
	group.Mode = domain.VerifyModeRequest
	if err := svc.SaveGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnJoinRequest(ctx, JoinRequest{ChatID: -400, UserID: 30, UserChatID: 30, DisplayName: "申请者", UpdateID: 1}); err != nil {
		t.Fatal(err)
	}
	sess, err := registry.Get(ctx, domain.SessionKey{ChatID: -400, UserID: 30})
	if err != nil {
		t.Fatal(err)
	}
	sess.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := registry.AttachMessages(ctx, sess.Key, sess.ID, sess.ChallengeMessage, sess.AdminMessage, sess.ExpiresAt, sess.Version); err != nil {
		t.Fatal(err)
	}
	runBackground(t, svc)
	if len(api.declined) != 1 || api.declined[0].userID != 30 {
		t.Fatalf("申请超时应当被拒绝，实际 %+v", api.declined)
	}
}
