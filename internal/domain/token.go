package domain

import (
	"crypto/sha256"
	"crypto/subtle"
)

// HashToken 计算选项 token 的 sha256。注册表只保存这个哈希，
// 因此数据库/日志泄漏也读不出答案。
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// MatchesHash 以常数时间比较 token 与哈希。
func MatchesHash(token string, hash []byte) bool {
	if len(hash) == 0 {
		return false
	}
	got := HashToken(token)
	return subtle.ConstantTimeCompare(got, hash) == 1
}
