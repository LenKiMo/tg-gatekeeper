// Package domain 定义与题材、存储、Telegram 无关的核心模型。
//
// 这里刻意不出现任何具体游戏/角色词汇：一个 Entry 就是"一张图 + 一个标签"，
// 一个 GroupConfig 就是"某个群该不该验证、怎么验证"，一个 Session 就是"某人正在
// 通过一道题接受验证"。因此同一份二进制可以部署到任意"看图选名字"的场景。
package domain

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"
)

// ---------------------------------------------------------------- 数据集

// Entry 是数据集里的一条可出题记录。
type Entry struct {
	// ID 在数据集内稳定且唯一，只用于日志与审计，绝不发给客户端。
	ID string `json:"id" yaml:"id"`
	// Label 是展示标签，也是正确答案。
	Label string `json:"label" yaml:"label"`
	// ImageRef 是图片引用：file:、http(s): 等。
	ImageRef string `json:"image" yaml:"image"`
	// Pool 是"选项池"：干扰项必须与正确项来自同一 Pool，避免跨世界观混搭。
	Pool string `json:"pool" yaml:"pool"`
	// Groups 是通用分组（阵营/类别等），只用于难度策略，不展示给用户。
	Groups []string `json:"groups" yaml:"groups"`
	// Difficulty 是难度标注，0 表示未标注；只影响抽题偏好。
	Difficulty int `json:"difficulty" yaml:"difficulty"`
	// Weight 是抽中权重，<=0 视为 1；用于让运营压低冷门条目的出现率。
	Weight float64 `json:"weight" yaml:"weight"`
	// Enabled 为 false 的条目不会参与出题。
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Alias 是同一实体的其它写法；这些写法不会出现在选项里。
	Alias []string `json:"alias" yaml:"alias"`
}

// Snapshot 是数据集的不可变快照。调用者可独立持有，热更新时整体替换。
type Snapshot struct {
	// SourceID 是数据来源标识（provider 名 + 路径/URL）。
	SourceID string
	// Revision 是内容指纹（内容哈希前缀），用于审计"这题的题库是哪一版"。
	Revision string
	// LoadedAt 是快照生成时间。
	LoadedAt time.Time
	// Entries 是全部条目。
	Entries []Entry
	// Dropped 是被丢弃的条目的数量（缺 ID/标签/图片、重复 ID）。
	Dropped int
}

// Size 返回条目数量。
func (s Snapshot) Size() int { return len(s.Entries) }

// ---------------------------------------------------------------- 群配置

// VerifyMode 是群的验证模式。
type VerifyMode string

const (
	// VerifyModeJoin 表示普通入群模式：人已经进群，先禁言再在群里出题。
	VerifyModeJoin VerifyMode = "join"
	// VerifyModeRequest 表示申请审批模式：人还没进群，用 ChatJoinRequest 私聊出题。
	VerifyModeRequest VerifyMode = "request"
)

// Valid 判断模式取值是否合法。
func (m VerifyMode) Valid() bool {
	return m == VerifyModeJoin || m == VerifyModeRequest
}

// GroupConfig 是单个群的配置。
type GroupConfig struct {
	ChatID         int64
	Enabled        bool
	Mode           VerifyMode
	RulesMessageID int64
	// RulesLink 是群规的显式链接（运营用 /reg <url> 设置，任意 URL）。
	// 设置后 {rules} 占位符用它；为空则用 RulesMessageID 生成 t.me/c 链接。
	RulesLink string
	Welcome   string
	// Tag 是验证通过后给成员打的标签（Telegram 的 tag 功能），空表示不打。
	Tag     string
	AdWords []string
	// Revision 用于乐观并发：保存时必须带着读到的 revision，冲突则重试。
	Revision  uint64
	UpdatedAt time.Time
}

// ---------------------------------------------------------------- 会话

// SessionKey 是"谁在哪个群"验证。用结构体做 key，杜绝字符串拼接撞键。
type SessionKey struct {
	ChatID int64
	UserID int64
}

func (k SessionKey) String() string { return fmt.Sprintf("%d:%d", k.ChatID, k.UserID) }

// SessionMode 是会话来源。
type SessionMode string

const (
	SessionJoin    SessionMode = "join"
	SessionRequest SessionMode = "request"
)

// SessionState 是会话状态机。
type SessionState string

const (
	// StatePreparing 表示已开始处理（限制/下载图片中），此时还没有可点击的题。
	StatePreparing SessionState = "preparing"
	// StatePending 表示题已发出，等待作答。
	StatePending   SessionState = "pending"
	StatePassed                 = SessionState("passed")
	StateWrong                  = SessionState("wrong")
	StateTimedOut               = SessionState("timed_out")
	StateAdminPass              = SessionState("admin_pass")
	StateAdminBan               = SessionState("admin_ban")
	StateSpoke                  = SessionState("spoke")
	StateLeft                   = SessionState("left")
	StateCancelled              = SessionState("cancelled")
	// StateFiltered 表示入群前就被广告词拦截。
	StateFiltered = SessionState("filtered")
	// StateSkipped 表示按策略跳过验证（新加入的是 bot / 管理员 / 被邀请白名单）。
	StateSkipped = SessionState("skipped")
)

// Terminal 判断是否为终态。
func (s SessionState) Terminal() bool { return s != StatePreparing && s != StatePending }

// Active 判断是否为活动（未终结）状态。
func (s SessionState) Active() bool { return !s.Terminal() }

// Option 是一个选项：随机短 token + 展示标签。
// callback_data 里只放 token，不放标签，避免答案进入 Telegram 的按钮数据。
type Option struct {
	Token string `json:"t"`
	Label string `json:"l"`
}

// MessageRef 指向一条 Telegram 消息。MessageID 用 int64 与库和 Telegram 保持一致。
type MessageRef struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

// Valid 判断引用是否有效。
func (m MessageRef) Valid() bool { return m.ChatID != 0 && m.MessageID != 0 }

// Session 是一次验证的完整状态。
type Session struct {
	ID       string // 128 bit CSPRNG，base64url，无填充
	Key      SessionKey
	UpdateID int // 触发本会话的 Telegram update id，用于幂等
	Mode     SessionMode
	State    SessionState
	// DisplayName 是入群时新成员的昵称：欢迎语里的 {mention} 必须用它，
	// 否则只能退化成"新成员"这种占位文字（欢迎语在答题通过时才发，那时
	// 事件里只有点击者/管理员的信息，拿不到新成员本人）。
	DisplayName string
	// CorrectTokenHash 是正确答案 token 的 sha256，不存明文标签。
	CorrectTokenHash []byte
	Options          []Option
	DatasetRevision  string
	// ChallengeMessage 是题目消息（普通模式在群里，申请模式在申请者私聊）。
	ChallengeMessage MessageRef
	// AdminMessage 是申请模式下的群内管理员处置卡片。
	AdminMessage MessageRef
	// JoinMessage 是 Telegram 的"XX 加入群组"服务消息。
	JoinMessage MessageRef
	// RequestUserChatID 是申请者的私聊 ID（申请模式）。
	RequestUserChatID int64
	StartedAt         time.Time
	ExpiresAt         time.Time
	Version           uint64
}

// CorrectOption 返回正确答案对应的选项（仅用于内存中发送前的一次性比对）。
func (s Session) CorrectOption() (Option, bool) {
	for _, o := range s.Options {
		if MatchesHash(o.Token, s.CorrectTokenHash) {
			return o, true
		}
	}
	return Option{}, false
}

// IsCorrectToken 判断 token 是否命中本题正确答案（常数时间比较）。
func (s Session) IsCorrectToken(token string) bool {
	return MatchesHash(token, s.CorrectTokenHash)
}

// FindOption 按 token 查找选项。
func (s Session) FindOption(token string) (Option, bool) {
	for _, o := range s.Options {
		if o.Token == token {
			return o, true
		}
	}
	return Option{}, false
}

// ---------------------------------------------------------------- 事件与副作用

// EventKind 是驱动状态机的输入事件。
type EventKind string

const (
	EventAnswer    EventKind = "answer"
	EventTimeout   EventKind = "timeout"
	EventAdminPass EventKind = "admin_pass"
	EventAdminBan  EventKind = "admin_ban"
	EventSpoke     EventKind = "spoke"
	EventLeft      EventKind = "left"
	EventCancel    EventKind = "cancel"
)

// Event 是提交给注册表的一次输入。
type Event struct {
	Kind      EventKind
	SessionID string
	ActorID   int64
	// OptionToken 仅 EventAnswer 使用。
	OptionToken string
	// Message 是触发事件的消息（用于"待验证发言"时删除）。
	Message MessageRef
	// At 是服务端接收事件的时间；判定是否超时以它为准，而不是 goroutine 调度顺序。
	At time.Time
}

// EffectKind 是终态产生的 Telegram 副作用。
type EffectKind string

const (
	EffectRestorePermissions EffectKind = "restore_permissions"
	EffectBan                EffectKind = "ban"
	EffectUnban              EffectKind = "unban"
	EffectApprove            EffectKind = "approve_join_request"
	EffectDecline            EffectKind = "decline_join_request"
	EffectWelcome            EffectKind = "welcome"
	EffectDeleteMessage      EffectKind = "delete_message"
	EffectSetTag             EffectKind = "set_member_tag"
	EffectNotifyAdmin        EffectKind = "notify_admin"
)

// EffectPayload 是副作用的参数。用 map 而不是强类型，便于持久化与向后兼容。
type EffectPayload map[string]string

// Effect 是待执行的 Telegram 副作用（outbox 模式）。
type Effect struct {
	ID        string
	SessionID string
	Kind      EffectKind
	Payload   EffectPayload
	DueAt     time.Time
	Attempts  int
	LastError string
}

// ---------------------------------------------------------------- 工具

// NewSessionID 生成 128 bit 随机 session id（base64url 无填充，22 字符）。
func NewSessionID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成 session id 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// NewOptionToken 生成 4 字节随机选项 token（base64url 无填充，6 字符）。
// 64 字节的 callback_data 预算里，session(22) + token(6) + 前缀足够。
func NewOptionToken() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成选项 token 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// NewEffectID 生成副作用 ID。
func NewEffectID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成 effect id 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
