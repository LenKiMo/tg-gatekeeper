// Package config 负责读取、校验配置。
//
// 设计要点：
//   - 未知字段一律报错（yaml KnownFields），避免拼错后静默使用危险默认值；
//   - 只支持 ${ENV_NAME} 整标量展开，不执行任意 shell 表达式；
//   - 配置项只描述机制（选项个数、超时、数据集路径），实体名称来自数据集与文案模板。
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
)

// Config 是完整配置。
type Config struct {
	SchemaVersion  int            `yaml:"schema_version"`
	Bot            Bot            `yaml:"bot"`
	Runtime        Runtime        `yaml:"runtime"`
	GroupDefaults  GroupDefaults  `yaml:"group_defaults"`
	Commands       Commands       `yaml:"commands"`
	AdFilter       AdFilter       `yaml:"ad_filter"`
	Gatekeeper     Gatekeeper     `yaml:"gatekeeper"`
	Provider       Provider       `yaml:"provider"`
	Images         Images         `yaml:"images"`
	Storage        Storage        `yaml:"storage"`
	Session        Session        `yaml:"session"`
	DeletionWorker DeletionWorker `yaml:"deletion_worker"`
	Dispatcher     Dispatcher     `yaml:"dispatcher"`
	Audit          Audit          `yaml:"audit"`
}

// Bot 是 Telegram 侧配置。
type Bot struct {
	Token string `yaml:"token"`
	// OwnerID 是可执行敏感操作（/reload）的账号；0 表示禁用所有者命令。
	OwnerID               int64  `yaml:"owner_id"`
	APIEndpoint           string `yaml:"api_endpoint"`
	Debug                 bool   `yaml:"debug"`
	PollingTimeoutSeconds int    `yaml:"polling_timeout_seconds"`
	UpdateBuffer          int    `yaml:"update_buffer"`
	IgnoreChannelCommands bool   `yaml:"ignore_channel_commands"`
	HTTPProxy             string `yaml:"http_proxy"`
	RequestTimeoutSeconds int    `yaml:"request_timeout_seconds"`
}

// Runtime 是进程级配置。
type Runtime struct {
	DataDir                string `yaml:"data_dir"`
	Timezone               string `yaml:"timezone"`
	ShutdownTimeoutSeconds int    `yaml:"shutdown_timeout_seconds"`
	ShutdownGraceSeconds   int    `yaml:"shutdown_grace_seconds"`
	HealthcheckOnStart     bool   `yaml:"healthcheck_on_start"`
	LogLevel               string `yaml:"log_level"`
}

// GroupDefaults 是新群首次出现时的默认配置。
type GroupDefaults struct {
	Enabled        bool     `yaml:"enabled"`
	Mode           string   `yaml:"mode"`
	Welcome        string   `yaml:"welcome"`
	RulesMessageID int64    `yaml:"rules_message_id"`
	AdWords        []string `yaml:"ad_words"`
}

// Commands 是命令相关配置。
type Commands struct {
	RequestMode                bool `yaml:"request_mode"`
	Welcome                    bool `yaml:"welcome"`
	Reg                        bool `yaml:"reg"`
	Tag                        bool `yaml:"tag"`
	Ping                       bool `yaml:"ping"`
	Help                       bool `yaml:"help"`
	ResponseDeleteAfterSeconds int  `yaml:"response_delete_after_seconds"`
	WelcomeDeleteAfterSeconds  int  `yaml:"welcome_delete_after_seconds"`
	TagMaxRunes                int  `yaml:"tag_max_runes"`
}

// AdFilter 是广告词过滤配置。
type AdFilter struct {
	Enabled              bool     `yaml:"enabled"`
	GlobalWords          []string `yaml:"global_words"`
	IncludeGroupWords    bool     `yaml:"include_group_words"`
	NormalizeNFKC        bool     `yaml:"normalize_nfkc"`
	IgnoreCase           bool     `yaml:"ignore_case"`
	RemoveWhitespace     bool     `yaml:"remove_whitespace"`
	RemoveZeroWidth      bool     `yaml:"remove_zero_width"`
	RemovableSeparators  []string `yaml:"removable_separators"`
	BioLookupErrorPolicy string   `yaml:"bio_lookup_error_policy"` // audit_continue | deny
	MatchMode            string   `yaml:"match_mode"`              // contains
}

// Difficulty 是难度策略。
type Difficulty struct {
	Strategy       string `yaml:"strategy"` // uniform | same_group | difficulty_band
	DifficultyBand int    `yaml:"difficulty_band"`
}

// Gatekeeper 是验证流程配置。
type Gatekeeper struct {
	OptionCount           int    `yaml:"option_count"`
	TimeoutSeconds        int    `yaml:"timeout_seconds"`
	KickUnbanAfterSeconds int    `yaml:"kick_unban_after_seconds"`
	FailurePolicy         string `yaml:"failure_policy"`  // deny | allow
	ShortagePolicy        string `yaml:"shortage_policy"` // deny | reduce
	MinOptionCount        int    `yaml:"min_option_count"`
	MaxImageAttempts      int    `yaml:"max_image_attempts"`
	InvitedMemberPolicy   string `yaml:"invited_member_policy"` // verify | allow
	BotMemberPolicy       string `yaml:"bot_member_policy"`     // allow | verify
	AdminMemberPolicy     string `yaml:"admin_member_policy"`   // allow | verify
	RevokeMessagesOnBan   bool   `yaml:"revoke_messages_on_ban"`
	// BlockPendingMessages 为 true 时，待验证用户一旦在群里发言立即按违规处置
	// （删消息 + 踢出）。这是原实现里有钩子却从未注册的反绕过能力。
	BlockPendingMessages bool       `yaml:"block_pending_messages"`
	AnswerWrongAlert     bool       `yaml:"answer_wrong_alert"`
	StaleCallbackAlert   bool       `yaml:"stale_callback_alert"`
	ButtonColumns        int        `yaml:"button_columns"`
	CaptionJoin          string     `yaml:"caption_join"`
	CaptionRequest       string     `yaml:"caption_request"`
	AdminCardText        string     `yaml:"admin_card_text"`
	Difficulty           Difficulty `yaml:"difficulty"`
}

// Provider 是数据集配置。
type Provider struct {
	Active                 string                 `yaml:"active"`
	RefreshIntervalSeconds int                    `yaml:"refresh_interval_seconds"`
	Definitions            map[string]ProviderDef `yaml:"definitions"`
}

// ProviderDef 是一个具名数据源。
type ProviderDef struct {
	Type      string        `yaml:"type"`
	JSONFile  *JSONFileDef  `yaml:"jsonfile"`
	Directory *DirectoryDef `yaml:"directory"`
	SQL       *SQLDef       `yaml:"sql"`
	HTTP      *HTTPDef      `yaml:"http"`
}

// JSONFileDef 是 JSON/JSONL 数据源配置。
type JSONFileDef struct {
	Path       string `yaml:"path"`
	Format     string `yaml:"format"` // json | jsonl | auto
	BaseDir    string `yaml:"base_dir"`
	Pool       string `yaml:"pool"`
	Watch      bool   `yaml:"watch"`
	LabelsFile string `yaml:"labels_file"`
}

// DirectoryDef 是图片目录数据源配置。
type DirectoryDef struct {
	Root            string   `yaml:"root"`
	LabelsFile      string   `yaml:"labels_file"`
	LabelsFormat    string   `yaml:"labels_format"` // lines | json
	FilenameIsLabel bool     `yaml:"filename_is_label"`
	Pool            string   `yaml:"pool"`
	Recursive       bool     `yaml:"recursive"`
	Extensions      []string `yaml:"extensions"`
}

// SQLDef 是 SQL 数据源配置。
type SQLDef struct {
	Driver              string `yaml:"driver"`
	DSN                 string `yaml:"dsn"`
	Query               string `yaml:"query"`
	MaxOpenConnections  int    `yaml:"max_open_connections"`
	QueryTimeoutSeconds int    `yaml:"query_timeout_seconds"`
}

// HTTPDef 是 HTTP 数据源配置。
type HTTPDef struct {
	URL                 string            `yaml:"url"`
	Format              string            `yaml:"format"`
	Method              string            `yaml:"method"`
	Headers             map[string]string `yaml:"headers"`
	BearerToken         string            `yaml:"bearer_token"`
	ResponseItemsPath   string            `yaml:"response_items_path"`
	TimeoutSeconds      int               `yaml:"timeout_seconds"`
	MaxResponseBytes    int64             `yaml:"max_response_bytes"`
	MaxRedirects        int               `yaml:"max_redirects"`
	AllowedHosts        []string          `yaml:"allowed_hosts"`
	AllowPrivateIPs     bool              `yaml:"allow_private_ips"`
	CacheFile           string            `yaml:"cache_file"`
	StaleIfErrorSeconds int               `yaml:"stale_if_error_seconds"`
}

// Images 是图片管线配置。
type Images struct {
	AllowedSchemes   []string   `yaml:"allowed_schemes"`
	LocalRoots       []string   `yaml:"local_roots"`
	MaxDownloadBytes int64      `yaml:"max_download_bytes"`
	MaxDecodedPixels int64      `yaml:"max_decoded_pixels"`
	MaxWidth         int        `yaml:"max_width"`
	MaxHeight        int        `yaml:"max_height"`
	OutputFormat     string     `yaml:"output_format"` // jpeg | png
	JPEGQuality      int        `yaml:"jpeg_quality"`
	StripMetadata    bool       `yaml:"strip_metadata"`
	HTTP             ImageHTTP  `yaml:"http"`
	Cache            ImageCache `yaml:"cache"`
}

// ImageHTTP 是图片下载的 HTTP 配置。
type ImageHTTP struct {
	TimeoutSeconds  int      `yaml:"timeout_seconds"`
	MaxRedirects    int      `yaml:"max_redirects"`
	AllowedHosts    []string `yaml:"allowed_hosts"`
	AllowPrivateIPs bool     `yaml:"allow_private_ips"`
	AllowAnyHost    bool     `yaml:"allow_any_host"`
	UserAgent       string   `yaml:"user_agent"`
}

// ImageCache 是图片缓存配置。
type ImageCache struct {
	Enabled    bool   `yaml:"enabled"`
	Dir        string `yaml:"dir"`
	MaxBytes   int64  `yaml:"max_bytes"`
	TTLSeconds int    `yaml:"ttl_seconds"`
}

// Storage 是持久化配置。
type Storage struct {
	GroupStore      string      `yaml:"group_store"`      // sqlite | mysql | yaml
	SessionRegistry string      `yaml:"session_registry"` // sqlite | mysql | memory
	DeletionQueue   string      `yaml:"deletion_queue"`   // sqlite | mysql | memory
	AutoMigrate     bool        `yaml:"auto_migrate"`
	SQLite          SQLiteStore `yaml:"sqlite"`
	MySQL           MySQLStore  `yaml:"mysql"`
	YAML            YAMLStore   `yaml:"yaml"`
}

// SQLiteStore 是 SQLite 参数。
type SQLiteStore struct {
	Path               string `yaml:"path"`
	WAL                bool   `yaml:"wal"`
	BusyTimeoutMS      int    `yaml:"busy_timeout_ms"`
	MaxOpenConnections int    `yaml:"max_open_connections"`
}

// MySQLStore 是 MySQL 参数。
type MySQLStore struct {
	DSN                          string `yaml:"dsn"`
	MaxOpenConnections           int    `yaml:"max_open_connections"`
	MaxIdleConnections           int    `yaml:"max_idle_connections"`
	ConnectionMaxLifetimeSeconds int    `yaml:"connection_max_lifetime_seconds"`
}

// YAMLStore 是 YAML 群配置存储参数。
type YAMLStore struct {
	Path          string `yaml:"path"`
	RuntimeWrites bool   `yaml:"runtime_writes"`
	FileMode      string `yaml:"file_mode"`
}

// Session 是会话/副作用配置。
type Session struct {
	TerminalRetentionSeconds    int `yaml:"terminal_retention_seconds"`
	RecoveryScanIntervalSeconds int `yaml:"recovery_scan_interval_seconds"`
	RecoveryBatchSize           int `yaml:"recovery_batch_size"`
	EffectRetryInitialSeconds   int `yaml:"effect_retry_initial_seconds"`
	EffectRetryMaxSeconds       int `yaml:"effect_retry_max_seconds"`
	EffectMaxAttempts           int `yaml:"effect_max_attempts"`
}

// DeletionWorker 是删除 worker 配置。
type DeletionWorker struct {
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`
	BatchSize           int `yaml:"batch_size"`
	LeaseSeconds        int `yaml:"lease_seconds"`
	RetryInitialSeconds int `yaml:"retry_initial_seconds"`
	RetryMaxSeconds     int `yaml:"retry_max_seconds"`
	AbandonAfterSeconds int `yaml:"abandon_after_seconds"`
}

// Dispatcher 是分派器配置。
type Dispatcher struct {
	CriticalWorkers              int `yaml:"critical_workers"`
	CriticalCapacity             int `yaml:"critical_capacity"`
	CriticalWarnAt               int `yaml:"critical_warn_at"`
	CriticalSubmitTimeoutSeconds int `yaml:"critical_submit_timeout_seconds"`
	CommandWorkers               int `yaml:"command_workers"`
	CommandPerKeyCapacity        int `yaml:"command_per_key_capacity"`
	CommandTotalCapacity         int `yaml:"command_total_capacity"`
	NoticeWorkers                int `yaml:"notice_workers"`
	NoticeTotalCapacity          int `yaml:"notice_total_capacity"`
}

// Audit 是审计配置。
type Audit struct {
	Enabled            bool   `yaml:"enabled"`
	Format             string `yaml:"format"`
	Stdout             bool   `yaml:"stdout"`
	File               string `yaml:"file"`
	FileMode           string `yaml:"file_mode"`
	RotateMaxMegabytes int    `yaml:"rotate_max_megabytes"`
	RotateKeepFiles    int    `yaml:"rotate_keep_files"`
	RotateKeepDays     int    `yaml:"rotate_keep_days"`
	IncludeDisplayName bool   `yaml:"include_display_name"`
	HashEntityID       bool   `yaml:"hash_entity_id"`
	RedactBio          bool   `yaml:"redact_bio"`
	RedactAnswerLabel  bool   `yaml:"redact_answer_label"`
}

// Default 返回带默认值的配置。
func Default() *Config {
	return &Config{
		SchemaVersion: 1,
		Bot: Bot{
			APIEndpoint:           "https://api.telegram.org/bot%s/%s",
			PollingTimeoutSeconds: 60,
			UpdateBuffer:          100,
			IgnoreChannelCommands: true,
			RequestTimeoutSeconds: 20,
		},
		Runtime: Runtime{
			DataDir:                "./data",
			Timezone:               "Asia/Shanghai",
			ShutdownTimeoutSeconds: 30,
			ShutdownGraceSeconds:   10,
			HealthcheckOnStart:     true,
			LogLevel:               "info",
		},
		GroupDefaults: GroupDefaults{Enabled: true, Mode: string(domain.VerifyModeJoin)},
		Commands: Commands{
			RequestMode: true, Welcome: true, Reg: true, Tag: true, Ping: true, Help: true,
			ResponseDeleteAfterSeconds: 30,
			WelcomeDeleteAfterSeconds:  3600,
			TagMaxRunes:                16,
		},
		AdFilter: AdFilter{
			Enabled:              true,
			IncludeGroupWords:    true,
			NormalizeNFKC:        true,
			IgnoreCase:           true,
			RemoveWhitespace:     true,
			RemoveZeroWidth:      true,
			RemovableSeparators:  []string{"-", "_", ".", "·"},
			BioLookupErrorPolicy: "audit_continue",
			MatchMode:            "contains",
		},
		Gatekeeper: Gatekeeper{
			OptionCount:           12,
			TimeoutSeconds:        60,
			KickUnbanAfterSeconds: 60,
			FailurePolicy:         "deny",
			ShortagePolicy:        "deny",
			MinOptionCount:        6,
			MaxImageAttempts:      4,
			InvitedMemberPolicy:   "verify",
			BotMemberPolicy:       "allow",
			AdminMemberPolicy:     "allow",
			RevokeMessagesOnBan:   true,
			BlockPendingMessages:  true,
			AnswerWrongAlert:      true,
			ButtonColumns:         2,
			CaptionJoin:           "欢迎 {mention}，请在 {timeout} 秒内选择与上图对应的名称。",
			CaptionRequest:        "请在 {timeout} 秒内选择与上图对应的名称。",
			AdminCardText:         "用户 {mention} 正在进行入群申请验证。",
			Difficulty:            Difficulty{Strategy: "uniform", DifficultyBand: 1},
		},
		Provider: Provider{
			RefreshIntervalSeconds: 3600,
			Definitions:            map[string]ProviderDef{},
		},
		Images: Images{
			AllowedSchemes:   []string{"file", "https", "http"},
			LocalRoots:       []string{"./data"},
			MaxDownloadBytes: 8 << 20,
			MaxDecodedPixels: 24_000_000,
			MaxWidth:         4096,
			MaxHeight:        4096,
			OutputFormat:     "jpeg",
			JPEGQuality:      88,
			StripMetadata:    true,
			HTTP: ImageHTTP{
				TimeoutSeconds: 15,
				MaxRedirects:   2,
				UserAgent:      "tg-gatekeeper/1",
			},
			Cache: ImageCache{
				Enabled:    true,
				Dir:        "./data/cache/images",
				MaxBytes:   512 << 20,
				TTLSeconds: 7 * 24 * 3600,
			},
		},
		Storage: Storage{
			GroupStore:      "sqlite",
			SessionRegistry: "sqlite",
			DeletionQueue:   "sqlite",
			AutoMigrate:     true,
			SQLite: SQLiteStore{
				Path:               "./data/gatekeeper.db",
				WAL:                true,
				BusyTimeoutMS:      5000,
				MaxOpenConnections: 1,
			},
			MySQL: MySQLStore{
				MaxOpenConnections:           10,
				MaxIdleConnections:           5,
				ConnectionMaxLifetimeSeconds: 1800,
			},
			YAML: YAMLStore{Path: "./data/groups.yaml", RuntimeWrites: true, FileMode: "0600"},
		},
		Session: Session{
			TerminalRetentionSeconds:    86400,
			RecoveryScanIntervalSeconds: 5,
			RecoveryBatchSize:           100,
			EffectRetryInitialSeconds:   2,
			EffectRetryMaxSeconds:       300,
			EffectMaxAttempts:           20,
		},
		DeletionWorker: DeletionWorker{
			PollIntervalSeconds: 2,
			BatchSize:           50,
			LeaseSeconds:        60,
			RetryInitialSeconds: 5,
			RetryMaxSeconds:     300,
			AbandonAfterSeconds: 86400,
		},
		Dispatcher: Dispatcher{
			CriticalWorkers:              8,
			CriticalCapacity:             512,
			CriticalWarnAt:               256,
			CriticalSubmitTimeoutSeconds: 0,
			CommandWorkers:               4,
			CommandPerKeyCapacity:        32,
			CommandTotalCapacity:         256,
			NoticeWorkers:                1,
			NoticeTotalCapacity:          32,
		},
		Audit: Audit{
			Enabled:            true,
			Format:             "jsonl",
			Stdout:             true,
			File:               "./data/logs/audit.jsonl",
			FileMode:           "0640",
			RotateMaxMegabytes: 100,
			RotateKeepFiles:    10,
			RotateKeepDays:     30,
			IncludeDisplayName: true,
			HashEntityID:       true,
			RedactBio:          true,
			RedactAnswerLabel:  true,
		},
	}
}

var envPattern = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// Load 读取配置、展开环境变量、做跨字段校验。
func Load(path string) (*Config, error) {
	cfg := Default()
	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(expandEnv(string(raw))))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("解析配置文件 %s 失败（未知字段会在此报错）: %w", path, err)
		}
	}
	expandEnvStruct(reflect.ValueOf(cfg))
	applyAirbag(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// expandEnv 把整标量形式的 ${NAME} 展开为环境变量值；找不到则保持原样。
func expandEnv(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := envPattern.FindStringSubmatch(m)[1]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

// expandEnvStruct 递归展开结构体里的字符串字段（用于 YAML 反序列化后的值）。
func expandEnvStruct(v reflect.Value) {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !v.IsNil() {
			expandEnvStruct(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				expandEnvStruct(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			expandEnvStruct(v.Index(i))
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			val := iter.Value()
			if val.Kind() == reflect.String && v.CanSet() {
				v.SetMapIndex(iter.Key(), reflect.ValueOf(expandEnv(val.String())))
			}
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(expandEnv(v.String()))
		}
	}
}

func applyAirbag(cfg *Config) {
	if cfg.Runtime.DataDir == "" {
		cfg.Runtime.DataDir = "./data"
	}
	if cfg.Bot.APIEndpoint == "" {
		cfg.Bot.APIEndpoint = "https://api.telegram.org/bot%s/%s"
	}
	// 环境变量兜底：容器部署不想把 token/DSN 写进文件时使用。
	if cfg.Bot.Token == "" {
		cfg.Bot.Token = os.Getenv("TG_BOT_TOKEN")
	}
	if cfg.Storage.MySQL.DSN == "" {
		cfg.Storage.MySQL.DSN = os.Getenv("MYSQL_DSN")
	}
}

// Validate 做跨字段校验。任何"本版本不支持"的后端都会在此明确报错，
// 而不是放一个静默退化的 stub。
func (c *Config) Validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("schema_version 只支持 1，当前 %d", c.SchemaVersion)
	}
	if strings.TrimSpace(c.Bot.Token) == "" {
		return errors.New("bot.token 不能为空（也可用 ${TG_BOT_TOKEN} 或环境变量 TG_BOT_TOKEN）")
	}
	if c.Bot.PollingTimeoutSeconds <= 0 {
		return errors.New("bot.polling_timeout_seconds 必须为正数")
	}
	if c.Bot.UpdateBuffer <= 0 {
		c.Bot.UpdateBuffer = 100
	}
	if c.Bot.RequestTimeoutSeconds <= 0 {
		c.Bot.RequestTimeoutSeconds = 20
	}
	if _, err := time.LoadLocation(c.Runtime.Timezone); err != nil {
		return fmt.Errorf("runtime.timezone %q 无效: %w", c.Runtime.Timezone, err)
	}

	switch c.Gatekeeper.FailurePolicy {
	case "deny", "allow":
	default:
		return fmt.Errorf("gatekeeper.failure_policy 只能是 deny 或 allow，当前 %q", c.Gatekeeper.FailurePolicy)
	}
	switch c.Gatekeeper.ShortagePolicy {
	case "deny", "reduce":
	default:
		return fmt.Errorf("gatekeeper.shortage_policy 只能是 deny 或 reduce，当前 %q", c.Gatekeeper.ShortagePolicy)
	}
	for name, v := range map[string]string{
		"gatekeeper.invited_member_policy": c.Gatekeeper.InvitedMemberPolicy,
		"gatekeeper.bot_member_policy":     c.Gatekeeper.BotMemberPolicy,
		"gatekeeper.admin_member_policy":   c.Gatekeeper.AdminMemberPolicy,
	} {
		if v != "verify" && v != "allow" {
			return fmt.Errorf("%s 只能是 verify 或 allow，当前 %q", name, v)
		}
	}
	switch c.Gatekeeper.Difficulty.Strategy {
	case "uniform", "same_group", "difficulty_band":
	default:
		return errors.New("gatekeeper.difficulty.strategy 只能是 uniform/same_group/difficulty_band")
	}
	if c.Gatekeeper.OptionCount < 2 || c.Gatekeeper.OptionCount > 20 {
		return fmt.Errorf("gatekeeper.option_count 必须在 2..20，当前 %d", c.Gatekeeper.OptionCount)
	}
	if c.Gatekeeper.MinOptionCount < 2 {
		c.Gatekeeper.MinOptionCount = 2
	}
	if c.Gatekeeper.MinOptionCount > c.Gatekeeper.OptionCount {
		c.Gatekeeper.MinOptionCount = c.Gatekeeper.OptionCount
	}
	if c.Gatekeeper.TimeoutSeconds <= 0 {
		return errors.New("gatekeeper.timeout_seconds 必须为正数")
	}
	if c.Gatekeeper.MaxImageAttempts <= 0 {
		c.Gatekeeper.MaxImageAttempts = 1
	}
	if c.Gatekeeper.ButtonColumns <= 0 {
		c.Gatekeeper.ButtonColumns = 2
	}
	if m := c.GroupDefaults.Mode; m != "" && !domain.VerifyMode(m).Valid() {
		return fmt.Errorf("group_defaults.mode 只能是 join 或 request，当前 %q", m)
	}

	if c.AdFilter.MatchMode != "contains" {
		return errors.New("ad_filter.match_mode 首版只支持 contains")
	}
	switch c.AdFilter.BioLookupErrorPolicy {
	case "audit_continue", "deny":
	default:
		return errors.New("ad_filter.bio_lookup_error_policy 只能是 audit_continue 或 deny")
	}

	if c.Provider.Active == "" {
		return errors.New("provider.active 不能为空")
	}
	def, ok := c.Provider.Definitions[c.Provider.Active]
	if !ok {
		return fmt.Errorf("provider.active %q 在 provider.definitions 中不存在", c.Provider.Active)
	}
	if err := validateProviderDef(c.Provider.Active, def); err != nil {
		return err
	}
	if c.Provider.RefreshIntervalSeconds <= 0 {
		c.Provider.RefreshIntervalSeconds = 3600
	}

	if err := c.validateImages(); err != nil {
		return err
	}

	groupBackend := strings.ToLower(c.Storage.GroupStore)
	switch groupBackend {
	case "sqlite", "mysql", "yaml":
	case "redis", "file":
		return fmt.Errorf("storage.group_store=%q 本版本未实现，请使用 sqlite/mysql/yaml", c.Storage.GroupStore)
	default:
		return fmt.Errorf("storage.group_store 只能是 sqlite/mysql/yaml，当前 %q", c.Storage.GroupStore)
	}
	for name, backend := range map[string]string{
		"storage.session_registry": c.Storage.SessionRegistry,
		"storage.deletion_queue":   c.Storage.DeletionQueue,
	} {
		switch strings.ToLower(backend) {
		case "sqlite", "mysql", "memory":
		case "redis", "file":
			return fmt.Errorf("%s=%q 本版本未实现，请使用 sqlite/mysql（memory 仅用于测试）", name, backend)
		default:
			return fmt.Errorf("%s 只能是 sqlite/mysql/memory，当前 %q", name, backend)
		}
	}
	if groupBackend == "mysql" && strings.TrimSpace(c.Storage.MySQL.DSN) == "" {
		return errors.New("storage.mysql.dsn 不能为空（也可用 ${MYSQL_DSN}）")
	}
	if groupBackend == "yaml" && strings.TrimSpace(c.Storage.YAML.Path) == "" {
		return errors.New("storage.yaml.path 不能为空")
	}
	if needsSQLite(groupBackend, c.Storage.SessionRegistry, c.Storage.DeletionQueue) {
		if strings.TrimSpace(c.Storage.SQLite.Path) == "" {
			return errors.New("storage.sqlite.path 不能为空")
		}
	}
	if groupBackend == "yaml" && (strings.EqualFold(c.Storage.SessionRegistry, "sqlite") || strings.EqualFold(c.Storage.DeletionQueue, "sqlite")) {
		// YAML 群配置 + SQLite 状态是受支持的组合（文档里说明：状态始终落在本地 SQLite）。
	}

	if c.Dispatcher.CriticalWorkers <= 0 {
		c.Dispatcher.CriticalWorkers = 8
	}
	if c.Dispatcher.CriticalCapacity <= 0 {
		c.Dispatcher.CriticalCapacity = 512
	}
	if c.Dispatcher.CommandWorkers <= 0 {
		c.Dispatcher.CommandWorkers = 4
	}
	if c.Dispatcher.CommandTotalCapacity <= 0 {
		c.Dispatcher.CommandTotalCapacity = 256
	}
	if c.Dispatcher.CommandPerKeyCapacity <= 0 {
		c.Dispatcher.CommandPerKeyCapacity = 32
	}
	if c.Dispatcher.NoticeWorkers <= 0 {
		c.Dispatcher.NoticeWorkers = 1
	}
	if c.Dispatcher.NoticeTotalCapacity <= 0 {
		c.Dispatcher.NoticeTotalCapacity = 32
	}
	if c.Session.RecoveryScanIntervalSeconds <= 0 {
		c.Session.RecoveryScanIntervalSeconds = 5
	}
	if c.Session.RecoveryBatchSize <= 0 {
		c.Session.RecoveryBatchSize = 100
	}
	if c.Session.EffectRetryInitialSeconds <= 0 {
		c.Session.EffectRetryInitialSeconds = 2
	}
	if c.Session.EffectRetryMaxSeconds <= 0 {
		c.Session.EffectRetryMaxSeconds = 300
	}
	if c.Session.EffectMaxAttempts <= 0 {
		c.Session.EffectMaxAttempts = 20
	}
	if c.DeletionWorker.PollIntervalSeconds <= 0 {
		c.DeletionWorker.PollIntervalSeconds = 2
	}
	if c.DeletionWorker.BatchSize <= 0 {
		c.DeletionWorker.BatchSize = 50
	}
	if c.DeletionWorker.LeaseSeconds <= 0 {
		c.DeletionWorker.LeaseSeconds = 60
	}
	if c.DeletionWorker.AbandonAfterSeconds <= 0 {
		c.DeletionWorker.AbandonAfterSeconds = 86400
	}
	if c.Runtime.ShutdownTimeoutSeconds <= 0 {
		c.Runtime.ShutdownTimeoutSeconds = 30
	}
	if c.Runtime.ShutdownGraceSeconds <= 0 {
		c.Runtime.ShutdownGraceSeconds = 10
	}
	return nil
}

func needsSQLite(groupBackend, registry, deletion string) bool {
	if strings.EqualFold(groupBackend, "sqlite") {
		return true
	}
	for _, b := range []string{registry, deletion} {
		if strings.EqualFold(b, "sqlite") {
			return true
		}
	}
	return false
}

func validateProviderDef(name string, def ProviderDef) error {
	switch strings.ToLower(def.Type) {
	case "jsonfile":
		if def.JSONFile == nil || strings.TrimSpace(def.JSONFile.Path) == "" {
			return fmt.Errorf("provider.definitions.%s.jsonfile.path 不能为空", name)
		}
		switch def.JSONFile.Format {
		case "", "json", "jsonl", "auto":
		default:
			return fmt.Errorf("provider.definitions.%s.jsonfile.format 只能是 json/jsonl/auto", name)
		}
	case "directory":
		if def.Directory == nil || strings.TrimSpace(def.Directory.Root) == "" {
			return fmt.Errorf("provider.definitions.%s.directory.root 不能为空", name)
		}
		if f := def.Directory.LabelsFormat; f != "" && f != "lines" && f != "json" {
			return fmt.Errorf("provider.definitions.%s.directory.labels_format 只能是 lines/json", name)
		}
	case "sql":
		if def.SQL == nil || strings.TrimSpace(def.SQL.Query) == "" {
			return fmt.Errorf("provider.definitions.%s.sql.query 不能为空", name)
		}
		switch strings.ToLower(def.SQL.Driver) {
		case "sqlite", "sqlite3", "mysql", "mariadb":
		default:
			return fmt.Errorf("provider.definitions.%s.sql.driver 只能是 sqlite/mysql", name)
		}
	case "http":
		if def.HTTP == nil || strings.TrimSpace(def.HTTP.URL) == "" {
			return fmt.Errorf("provider.definitions.%s.http.url 不能为空", name)
		}
		if !def.HTTP.AllowPrivateIPs && len(def.HTTP.AllowedHosts) == 0 {
			return fmt.Errorf("provider.definitions.%s.http.allowed_hosts 不能为空（若确实要允许任意主机，显式设置 allow_private_ips: true 并自行承担 SSRF 风险）", name)
		}
	default:
		return fmt.Errorf("provider.definitions.%s.type=%q 不支持（可选 jsonfile/directory/sql/http）", name, def.Type)
	}
	return nil
}

func (c *Config) validateImages() error {
	switch c.Images.OutputFormat {
	case "jpeg", "png":
	default:
		return fmt.Errorf("images.output_format 只能是 jpeg 或 png，当前 %q", c.Images.OutputFormat)
	}
	if c.Images.JPEGQuality <= 0 || c.Images.JPEGQuality > 100 {
		c.Images.JPEGQuality = 88
	}
	if c.Images.MaxDownloadBytes <= 0 {
		c.Images.MaxDownloadBytes = 8 << 20
	}
	if c.Images.MaxDecodedPixels <= 0 {
		c.Images.MaxDecodedPixels = 24_000_000
	}
	if c.Images.MaxWidth <= 0 {
		c.Images.MaxWidth = 4096
	}
	if c.Images.MaxHeight <= 0 {
		c.Images.MaxHeight = 4096
	}
	if c.Images.HTTP.TimeoutSeconds <= 0 {
		c.Images.HTTP.TimeoutSeconds = 15
	}
	if len(c.Images.AllowedSchemes) == 0 {
		c.Images.AllowedSchemes = []string{"file"}
	}
	for _, s := range c.Images.AllowedSchemes {
		switch s {
		case "file", "http", "https":
		default:
			return fmt.Errorf("images.allowed_schemes 含不支持的协议 %q", s)
		}
	}
	return nil
}

// ActiveProvider 返回当前激活的数据源定义。
func (c *Config) ActiveProvider() ProviderDef { return c.Provider.Definitions[c.Provider.Active] }

// VerifyTimeout 返回生效的答题超时。
func (c *Config) VerifyTimeout() time.Duration {
	return time.Duration(c.Gatekeeper.TimeoutSeconds) * time.Second
}

// Band 返回难度带（供 difficulty_band 策略使用）。
func (d Difficulty) Band() int { return d.DifficultyBand }

// KickUnbanAfter 返回临时踢出后的解封延迟。
func (c *Config) KickUnbanAfter() time.Duration {
	return time.Duration(c.Gatekeeper.KickUnbanAfterSeconds) * time.Second
}

// WarnUnsafe 返回需要在启动时醒目打印的安全告警。
func (c *Config) WarnUnsafe() []string {
	var out []string
	if c.Gatekeeper.FailurePolicy == "allow" {
		out = append(out, "gatekeeper.failure_policy=allow：数据源/图片故障时会放行，门禁可被基础设施故障绕过")
	}
	if c.Gatekeeper.ShortagePolicy == "reduce" {
		out = append(out, fmt.Sprintf("gatekeeper.shortage_policy=reduce：数据不足时选项可降到 %d 个", c.Gatekeeper.MinOptionCount))
	}
	if c.Storage.SessionRegistry == "memory" || c.Storage.DeletionQueue == "memory" {
		out = append(out, "storage 使用 memory：进程重启会丢失待验证会话与待删除任务，仅限测试")
	}
	if c.Images.HTTP.AllowAnyHost {
		out = append(out, "images.http.allow_any_host=true：图片 URL 可指向任意主机（含内网），存在 SSRF 风险")
	}
	return out
}
