package gatekeeper

// 入群资料的采集、脱敏与保留期清理。
//
// 边界（与 audit.jsonl 刻意不对称）：
//   - 这张表里**没有 user_id**。ref = HMAC-SHA256(key, user_id) 的前 16 字节十六进制，
//     同一用户在库里可关联（能算"这个人反复来过"），但库被拿走也反查不到人。
//   - 真正的处置记录（封禁/解封/申诉/管理员操作）留在 audit.jsonl，那条线保留明文
//     user_id——脱敏会让"这个人为什么被封"失去可追溯性，等于把运维能力也一起删掉。
//   - 聊天正文本来就不落库（消息级拦截只写审计里的命中词与按钮链接），因此没有
//     "聊天记录保留期"这件事需要处理；浏览器/客户端侧的聊天过期与本服务无关。
//
// 采集的是"入群那一刻读到的公开资料"（昵称、简介、有无 username/头像），用途只有
// 两个：离线提炼谐音/词条，以及事后判断某条规则是不是误伤。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/LenKiMo/tg-gatekeeper/internal/domain"
	"github.com/LenKiMo/tg-gatekeeper/internal/ports"
)

// 落库的判定类型（decision 字段）。
const (
	profileDecisionVerify      = "verify"          // 正常出题
	profileDecisionAdName      = "ad_display_name" // 昵称命中
	profileDecisionAdBio       = "ad_bio"          // 简介命中
	profileDecisionEnglishName = "english_name_bot"
	profileDecisionSkip        = "skip"
)

// profileRef 把 user_id 单向映射成分析用的稳定标识。
func profileRef(key []byte, userID int64) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.FormatInt(userID, 10)))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

var (
	emojiRe = regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{FE0F}\x{2B00}-\x{2BFF}]`)
	cjkRe   = regexp.MustCompile(`[\x{4E00}-\x{9FFF}]`)
)

// nameShape 给出昵称的粗形态，供离线统计（"英文名占多少、emoji 装饰占多少"）。
func nameShape(name string) string {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "empty"
	case cjkRe.MatchString(n):
		if emojiRe.MatchString(n) {
			return "cjk_emoji"
		}
		return "cjk"
	case emojiRe.MatchString(n):
		if strings.TrimSpace(emojiRe.ReplaceAllString(n, "")) == "" {
			return "emoji_only"
		}
		return "cjk_emoji"
	}
	onlyASCII := true
	for _, r := range n {
		if r > unicode.MaxASCII {
			onlyASCII = false
			break
		}
	}
	if onlyASCII {
		return "ascii_name"
	}
	return "other"
}

// collectProfiles 判断是否要写脱敏快照。
func (s *Service) collectProfiles() bool { return s.profiles != nil && s.cfg.Privacy.CollectProfiles }

// noteProfile 写一条脱敏快照。失败只告警，绝不影响验证流程。
func (s *Service) noteProfile(ctx context.Context, group domain.GroupConfig, userID int64,
	name string, p ports.UserProfile, decision, rule, word string) {
	if !s.collectProfiles() {
		return
	}
	days := s.cfg.Privacy.RetentionDays
	var expires int64
	if days > 0 {
		expires = time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	}
	snap := ports.ProfileSnapshot{
		Ref:         profileRef(s.privacyKey, userID),
		ChatID:      group.ChatID,
		DisplayName: name,
		Bio:         p.Bio,
		HasUsername: p.Username != "",
		HasPhoto:    p.HasPhoto,
		NameShape:   nameShape(name),
		Decision:    decision,
		Rule:        rule,
		Word:        word,
		CreatedAt:   time.Now().Unix(),
		ExpiresAt:   expires,
	}
	if err := s.profiles.SaveProfile(ctx, snap); err != nil {
		s.log.Warn("写入脱敏资料快照失败", "err", err)
	}
}

// profileRetentionLoop 每小时清理一次过期快照。
//
// 保留期只在"有数据要留"时才有意义：privacy.retention_days=0 表示不自动清理，
// 循环也就不必跑。
func (s *Service) profileRetentionLoop() {
	defer s.wg.Done()
	days := s.cfg.Privacy.RetentionDays
	if s.profiles == nil || days <= 0 {
		<-s.stopCh
		return
	}
	purge := func() {
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if n, err := s.profiles.PurgeProfilesBefore(ctx, cutoff); err != nil {
			s.log.Warn("清理过期资料快照失败", "err", err)
		} else if n > 0 {
			s.log.Info("已清理过期资料快照", "rows", n, "retention_days", days)
		}
	}
	purge()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			purge()
		}
	}
}
