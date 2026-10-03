package telegram

import (
	"strings"

	tgbotapi "github.com/ijnkawakaze/telegram-bot-api"
)

// TextWithLinks 返回消息文本，并把 text_link 实体还原成 MarkdownV2 链接。
//
// 运营在客户端里输入 `[点击阅读](https://…)` 时，客户端会把它变成 text_link 实体：
// 机器人收到的 Message.Text 只剩可见文字（"点击阅读"），URL 只存在于 Entities 里。
// 直接存 Text 会把链接悄悄丢掉（真实踩过：欢迎语里的群规链接变成光秃秃的"点击阅读"），
// 所以这里按实体把链接拼回去。
func TextWithLinks(m *tgbotapi.Message) string {
	if m == nil {
		return ""
	}
	text := m.Text
	if text == "" || len(m.Entities) == 0 {
		return text
	}
	runes := []rune(text)
	index := utf16ToRuneIndex(text)
	var b strings.Builder
	prev := 0
	for _, e := range m.Entities {
		if e.Type != "text_link" || e.URL == "" {
			continue
		}
		start, okStart := index[e.Offset]
		end, okEnd := index[e.Offset+e.Length]
		if !okStart || !okEnd || start < prev || end < start || end > len(runes) {
			continue
		}
		b.WriteString(string(runes[prev:start]))
		b.WriteString("[" + string(runes[start:end]) + "](" + e.URL + ")")
		prev = end
	}
	b.WriteString(string(runes[prev:]))
	return b.String()
}

// utf16ToRuneIndex 建立 UTF-16 码元偏移 → rune 下标的映射。
//
// Telegram 的实体偏移以 UTF-16 码元计（emoji 占 2 个），不能直接当 rune 下标用。
func utf16ToRuneIndex(s string) map[int]int {
	runes := []rune(s)
	index := make(map[int]int, len(runes)+1)
	units := 0
	for i, r := range runes {
		index[units] = i
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	index[units] = len(runes)
	return index
}
