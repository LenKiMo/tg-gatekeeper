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

// RenderCaption 把模板里的占位符替换为已转义文本。
//
// 支持 {mention}、{name}、{timeout}、{id}、{rules}；未知占位原样保留（便于运营发现拼错）。
//
// {rules} 渲染成一条 MarkdownV2 链接（链接文字"点击阅读"）。**不要**在模板里手写
// `[文字](url)`：运营在客户端里打字时，客户端会把它变成 text_link 实体，机器人收到的
// Message.Text 里只剩可见文字、链接只存在于 Entities 里——所以链接必须由机器人拼。
func RenderCaption(template string, userID int64, displayName string, timeoutSeconds int, rulesLink string) string {
	if template == "" {
		return ""
	}
	name := strings.TrimSpace(displayName)
	rules := ""
	if rulesLink != "" {
		rules = "[" + EscapeMarkdownV2("点击阅读") + "](" + escapeLinkURL(rulesLink) + ")"
	}
	replacer := strings.NewReplacer(
		"{mention}", Mention(userID, name),
		"{name}", EscapeMarkdownV2(name),
		"{timeout}", strconv.Itoa(timeoutSeconds),
		"{id}", strconv.FormatInt(userID, 10),
		"{rules}", rules,
	)
	return replacer.Replace(template)
}

// escapeLinkURL 只转义 MarkdownV2 链接目标里必须转义的字符（')' 与 '\\'）。
// 其余字符（. / : - _ 等）在链接目标内是合法的，多转义反而会让 URL 变样。
func escapeLinkURL(u string) string {
	u = strings.ReplaceAll(u, "\\", "\\\\")
	return strings.ReplaceAll(u, ")", "\\)")
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

// AdminRow 管理员处置键（一行两个）：放行在左、封禁在右下角。
// 危险动作放右下角，避免误触。
func AdminRow(sessionID string) []ports.Button {
	return []ports.Button{
		{Text: "✅ 放行", Data: EncodeAdmin(sessionID, false)},
		{Text: "🚫 封禁", Data: EncodeAdmin(sessionID, true)},
	}
}

// AdminKeyboard 单独一张管理员卡片的键盘。（申请模式下题目发在私聊，
// 管理员看不到，因此群里仍需这张卡片。）
func AdminKeyboard(sessionID string) ports.ButtonGrid {
	return ports.ButtonGrid{AdminRow(sessionID)}
}

// ChallengeKeyboard 入群模式的题目键盘：选项 + 末行管理员处置键。
//
// 管理员按钮和选项在同一张卡片上，因此不需要再发一条"管理员卡片"——
// 多发一条消息在千人大群里纯属冗余。
func ChallengeKeyboard(options []domain.Option, columns int, sessionID string) ports.ButtonGrid {
	return append(OptionKeyboard(options, columns, sessionID), AdminRow(sessionID))
}
