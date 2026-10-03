package telegram

import (
	"errors"
	"fmt"
	"strings"
)

// CallbackVersion 是 callback_data 的版本前缀；将来改格式时递增，旧按钮会被判为过期。
const CallbackVersion = "g1"

// CallbackKind 是按钮语义。
type CallbackKind string

const (
	// CallbackAnswer 是用户选择某个选项。
	CallbackAnswer CallbackKind = "answer"
	// CallbackAdminPass 是管理员放行。
	CallbackAdminPass CallbackKind = "admin_pass"
	// CallbackAdminBan 是管理员封禁。
	CallbackAdminBan CallbackKind = "admin_ban"
)

// Callback 是解析后的按钮数据。
type Callback struct {
	Kind      CallbackKind
	SessionID string
	Token     string
}

// ErrBadCallback 表示数据格式不合法（不是本 bot 的按钮 / 被截断 / 版本不符）。
var ErrBadCallback = errors.New("非法的 callback data")

// MaxCallbackBytes 是 Telegram 的硬限制。
const MaxCallbackBytes = 64

// sessionIDLen/optionTokenLen 是 domain 层生成的固定长度（16/4 字节 base64url 无填充）。
// 严格校验长度可以让"别的 bot 的按钮""被截断的旧数据"一律被拒绝，而不是靠运气。
const (
	sessionIDLen   = 22
	optionTokenLen = 6
)

// EncodeAnswer 生成选项按钮的数据：g1.<session>.<token>。
// 只含随机 token，不含标签、用户 ID、图片 URL，因此按钮数据不泄漏答案。
func EncodeAnswer(sessionID, token string) string {
	return strings.Join([]string{CallbackVersion, sessionID, token}, ".")
}

// EncodeAdmin 生成管理员按钮的数据：g1.<session>.p / .b。
func EncodeAdmin(sessionID string, ban bool) string {
	suffix := "p"
	if ban {
		suffix = "b"
	}
	return strings.Join([]string{CallbackVersion, sessionID, suffix}, ".")
}

// Decode 解析按钮数据。
func Decode(data string) (Callback, error) {
	if len(data) == 0 || len(data) > MaxCallbackBytes {
		return Callback{}, fmt.Errorf("%w: 长度 %d", ErrBadCallback, len(data))
	}
	parts := strings.Split(data, ".")
	if len(parts) != 3 {
		return Callback{}, fmt.Errorf("%w: 字段数 %d", ErrBadCallback, len(parts))
	}
	if parts[0] != CallbackVersion {
		return Callback{}, fmt.Errorf("%w: 版本 %q", ErrBadCallback, parts[0])
	}
	sessionID, tail := parts[1], parts[2]
	if !validToken(sessionID, sessionIDLen) {
		return Callback{}, fmt.Errorf("%w: session 非法", ErrBadCallback)
	}
	switch tail {
	case "p":
		return Callback{Kind: CallbackAdminPass, SessionID: sessionID}, nil
	case "b":
		return Callback{Kind: CallbackAdminBan, SessionID: sessionID}, nil
	}
	if !validToken(tail, optionTokenLen) {
		return Callback{}, fmt.Errorf("%w: token 非法", ErrBadCallback)
	}
	return Callback{Kind: CallbackAnswer, SessionID: sessionID, Token: tail}, nil
}

// validToken 只接受指定长度的 base64url 无填充字符串。
func validToken(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
