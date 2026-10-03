// Package ports 定义核心与外部世界之间的全部接口。
//
// domain 与 gatekeeper 只依赖这些接口，不导入 telegram-bot-api、SQL 驱动或任何
// provider 实现。这样抽样、状态机与副作用顺序都能在纯单元测试里验证。
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// ---------------------------------------------------------------- 数据源

// Source 提供数据集快照。
type Source interface {
	// ID 返回数据来源标识，用于日志与审计。
	ID() string
	// Snapshot 返回一份不可变快照；实现内部热更新时必须构造新 slice 后原子替换。
	Snapshot(ctx context.Context) (domain.Snapshot, error)
	// Close 释放连接等资源。
	Close() error
}

// ResolvedImage 是解析并处理完成的图片。
type ResolvedImage struct {
	Bytes []byte
	// Filename 固定为 challenge.<ext>，绝不沿用源文件名（否则会泄漏答案）。
	Filename  string
	MediaType string
	CacheKey  string
}

// ImageResolver 负责取图、限流、限尺寸、重编码与缓存。
type ImageResolver interface {
	// Resolve 解析一个图片引用（file:/http(s):），返回可直接上传的字节。
	Resolve(ctx context.Context, imageRef string) (ResolvedImage, error)
	Close() error
}

// ---------------------------------------------------------------- 群配置存储

var (
	// ErrConflict 表示乐观并发冲突（revision 不匹配）。
	ErrConflict = errors.New("revision conflict")
	// ErrNotFound 表示记录不存在。
	ErrNotFound = errors.New("not found")
)

// Store 是群配置存储。实现必须保证 SaveGroup 的 CAS 原子性。
type Store interface {
	EnsureGroup(ctx context.Context, chatID int64, defaults domain.GroupConfig) (domain.GroupConfig, error)
	GetGroup(ctx context.Context, chatID int64) (domain.GroupConfig, error)
	SaveGroup(ctx context.Context, cfg domain.GroupConfig, expectedRevision uint64) (domain.GroupConfig, error)
	// Probe 验证存储可读写，且**不留下任何数据**（自检用）。
	// 不要用 EnsureGroup(某个哨兵 chat_id) 代替：那会在生产库里永久留下一条假群配置。
	Probe(ctx context.Context) error
	Close() error
}

// ---------------------------------------------------------------- 会话注册表

// BeginResult 是开始一次会话的结果。
type BeginResult struct {
	Session   domain.Session
	Duplicate bool            // 同 update 的重复投递
	Replaced  *domain.Session // 被替换掉的旧世代会话（若有）
}

// ClaimResult 是抢占终态的结果。
type ClaimResult struct {
	Won     bool
	Reason  string // Won=false 时的原因：unknown/expired/replaced/wrong_actor/bad_token/already_done
	Before  domain.Session
	After   domain.Session
	Effects []domain.Effect // 与终态在同一事务写入 outbox
}

// 常见失败原因。
const (
	ClaimUnknown    = "unknown"
	ClaimExpired    = "expired"
	ClaimNotDue     = "not_due"
	ClaimReplaced   = "replaced"
	ClaimWrongActor = "wrong_actor"
	ClaimBadToken   = "bad_token"
	ClaimDone       = "already_done"
)

// EffectPlanner 由业务层提供：注册表赢得终态后，在同一事务里按它生成副作用。
// 这样"状态机判定"留在注册表（可测、可恢复），而"发什么欢迎语/带哪些消息 ID"
// 留在业务层（拿得到群配置）。两者之间没有先 Get 再 Delete 的竞态窗口。
type EffectPlanner func(before, after domain.Session, event domain.Event) []domain.Effect

// SessionRegistry 是会话状态的唯一事实来源。
//
// Claim 是并发正确性的中心：必须在同一把锁/同一事务里核对
// (ChatID, UserID, SessionID, State, ExpiresAt)、校验答题者与 token、
// 决定目标状态并写入副作用 outbox。业务层绝不先 Get 再 Delete。
type SessionRegistry interface {
	Begin(ctx context.Context, s domain.Session) (BeginResult, error)
	AttachMessages(ctx context.Context, key domain.SessionKey, sessionID string,
		challenge, admin domain.MessageRef, expiresAt time.Time, expectedVersion uint64) (domain.Session, error)
	Get(ctx context.Context, key domain.SessionKey) (domain.Session, error)
	// GetByID 按 session id 读取任意状态的会话（申请模式的私聊按钮、超时扫描用）。
	GetByID(ctx context.Context, sessionID string) (domain.Session, error)
	Claim(ctx context.Context, key domain.SessionKey, event domain.Event, plan EffectPlanner) (ClaimResult, error)
	// ListActive 返回未终结的会话；expiredOnly 为 true 时只返回已过期的。
	ListActive(ctx context.Context, now time.Time, expiredOnly bool, limit int) ([]domain.Session, error)
	ListDueEffects(ctx context.Context, now time.Time, limit int) ([]domain.Effect, error)
	AckEffect(ctx context.Context, effectID string) error
	RetryEffect(ctx context.Context, effectID string, next time.Time, cause string) error
	// CancelEffectsOf 作废某会话尚未执行的副作用（例如管理员永久封禁后撤销临时解封）。
	CancelEffectsOf(ctx context.Context, sessionID string, kinds ...domain.EffectKind) (int, error)
	Close() error
}

// ---------------------------------------------------------------- 延时删除队列

// DeleteJob 是一条待删除消息。
type DeleteJob struct {
	ID         string
	Message    domain.MessageRef
	DueAt      time.Time
	LeaseUntil time.Time
	Attempts   int
	LastError  string
}

// DeletionQueue 是持久化的延时删除队列。
type DeletionQueue interface {
	Enqueue(ctx context.Context, msg domain.MessageRef, dueAt time.Time) error
	LeaseDue(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]DeleteJob, error)
	Ack(ctx context.Context, jobID string) error
	Nack(ctx context.Context, jobID string, retryAt time.Time, cause string) error
	Close() error
}

// ---------------------------------------------------------------- 分派器

// Lane 是队列通道。
type Lane string

const (
	// LaneCritical 承载入群、申请、回调、待验证发言、离群；不丢，满载时背压。
	LaneCritical Lane = "critical"
	// LaneCommand 承载命令；满载可拒绝并回繁忙提示。
	LaneCommand Lane = "command"
	// LaneNotice 承载提示类消息，容量最小，避免提示本身放大拥塞。
	LaneNotice Lane = "notice"
)

// DispatchKey 是串行化的粒度：同一个 key 的任务严格 FIFO。
type DispatchKey struct {
	ChatID int64
	UserID int64
}

// Task 是一个待执行任务。
type Task func(ctx context.Context) error

// Dispatcher 按 key 串行、按 lane 限并发。
type Dispatcher interface {
	Submit(ctx context.Context, lane Lane, key DispatchKey, task Task) error
	StopAdmission()
	Drain(ctx context.Context) error
	Stats() DispatcherStats
}

// DispatcherStats 是分派器的运行指标。
type DispatcherStats struct {
	CriticalQueued  int
	CommandQueued   int
	NoticeQueued    int
	CriticalMaxWait time.Duration
}

// ---------------------------------------------------------------- 审计

// AuditEvent 是一条结构化审计记录。禁止写入 bio 原文、正确标签、图片 URL、token。
type AuditEvent struct {
	Event           string
	ChatID          int64
	UserID          int64
	ActorID         int64
	SessionID       string
	Mode            string
	OldState        string
	NewState        string
	DatasetRevision string
	Effect          string
	Attempt         int
	Result          string
	ErrorClass      string
	DisplayName     string
	Extra           map[string]string
}

// Audit 是审计出口。
type Audit interface {
	Log(ctx context.Context, ev AuditEvent)
	Close() error
}

// ---------------------------------------------------------------- Telegram

// Button 是一个内联按钮。
type Button struct {
	Text string
	Data string
}

// ButtonGrid 是按钮布局（行 × 列）。
type ButtonGrid [][]Button

// Empty 判断按钮表是否为空。
func (g ButtonGrid) Empty() bool { return len(g) == 0 }

// Telegram 是核心依赖的最小 Telegram 能力集；测试用它做 mock。
type Telegram interface {
	RestrictNoMessages(ctx context.Context, chatID, userID int64) error
	RestorePermissions(ctx context.Context, chatID, userID int64) error
	Ban(ctx context.Context, chatID, userID int64, revokeMessages bool) error
	Unban(ctx context.Context, chatID, userID int64, onlyIfBanned bool) error
	ApproveJoinRequest(ctx context.Context, chatID, userID int64) error
	DeclineJoinRequest(ctx context.Context, chatID, userID int64) error
	// SendChallenge 发送题目图片，返回消息引用。
	SendChallenge(ctx context.Context, targetChatID int64, image ResolvedImage,
		caption string, buttons ButtonGrid) (domain.MessageRef, error)
	// SendText 发送文本；markdownV2 为 true 时按 MarkdownV2 解析。
	SendText(ctx context.Context, chatID int64, text string, markdownV2 bool,
		buttons ButtonGrid) (domain.MessageRef, error)
	DeleteMessage(ctx context.Context, ref domain.MessageRef) error
	AnswerCallback(ctx context.Context, callbackID, text string, alert bool) error
	// GetBio 取用户简介（普通入群模式下用于广告词检查）。
	GetBio(ctx context.Context, userID int64) (string, error)
	IsAdminWithPermissions(ctx context.Context, chatID, userID int64, permission uint16) (bool, error)
	// GetMemberStatus 返回 creator/administrator/member/restricted/left/kicked/unknown。
	GetMemberStatus(ctx context.Context, chatID, userID int64) (string, error)
	SetMemberTag(ctx context.Context, chatID, userID int64, tag string) error
	// Self 返回机器人自身 ID/用户名。
	Self() (id int64, username string)
	Close() error
}

// AdminCanRestrictMembers 与库中的 AdminCanRestrictMembers 位保持一致。
const AdminCanRestrictMembers uint16 = 1 << 4

// ---------------------------------------------------------------- 辅助

// Clock 抽象时间，便于测试 deadline 边界。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产时钟。
type SystemClock struct{}

// Now 返回当前时间。
func (SystemClock) Now() time.Time { return time.Now() }
