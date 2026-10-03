package telegram

import (
	"strconv"
	"strings"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// markdownV2Special 是 Telegram MarkdownV2 要求转义的字符集合。
const markdownV2Special = `_*[]()~` + "`" + `>#+-=|{}.!`

// EscapeMarkdownV2 转义动态文本中的全部保留字符。
//
// 任何来自用户/配置/数据集的字符串（昵称、欢迎语、标签、群规）都必须先过这里，
// 否则一个小圆点就足以让 sendMessage 直接 400。
func EscapeMarkdownV2(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for _, r := range s {
		if strings.ContainsRune(markdownV2Special, r) || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Mention 生成一个到用户的 MarkdownV2 提及链接（昵称已转义）。
func Mention(userID int64, displayName string) string {
	name := strings.TrimSpace(displayName)
	if name == "" {
		name = "新成员"
	}
	return "[" + EscapeMarkdownV2(name) + "](tg://user?id=" + strconv.FormatInt(userID, 10) + ")"
}

// RenderCaption 把模板里的 {mention}/{timeout}/用户昵称占位替换为已转义文本。
//
// 支持 {mention}、{name}、{timeout}、{id}；未知占位原样保留（便于运营发现拼错）。
func RenderCaption(template string, userID int64, displayName string, timeoutSeconds int) string {
	if template == "" {
		return ""
	}
	name := strings.TrimSpace(displayName)
	replacer := strings.NewReplacer(
		"{mention}", Mention(userID, name),
		"{name}", EscapeMarkdownV2(name),
		"{timeout}", strconv.Itoa(timeoutSeconds),
		"{id}", strconv.FormatInt(userID, 10),
	)
	return replacer.Replace(template)
}

// OptionKeyboard 把选项按列数切成按钮表。
//
// 按钮文本用选项标签；callback_data 只用 token。
func OptionKeyboard(options []domain.Option, columns int, sessionID string) ports.ButtonGrid {
	if columns <= 0 {
		columns = 2
	}
	grid := make(ports.ButtonGrid, 0, (len(options)+columns-1)/columns)
	for i := 0; i < len(options); i += columns {
		end := i + columns
		if end > len(options) {
			end = len(options)
		}
		row := make([]ports.Button, 0, columns)
		for _, o := range options[i:end] {
			row = append(row, ports.Button{Text: o.Label, Data: EncodeAnswer(sessionID, o.Token)})
		}
		grid = append(grid, row)
	}
	return grid
}

// AdminKeyboard 是申请模式下的群内管理员处置按钮。
func AdminKeyboard(sessionID string) ports.ButtonGrid {
	return ports.ButtonGrid{{
		{Text: "✅ 放行", Data: EncodeAdmin(sessionID, false)},
		{Text: "🚫 封禁", Data: EncodeAdmin(sessionID, true)},
	}}
}
