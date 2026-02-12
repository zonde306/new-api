package common

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"strconv"
)

func Sha256Raw(data []byte) []byte {
	h := sha256.New()
	h.Write(data)
	return h.Sum(nil)
}

func Sha1Raw(data []byte) []byte {
	h := sha1.New()
	h.Write(data)
	return h.Sum(nil)
}

func Sha1(data []byte) string {
	return hex.EncodeToString(Sha1Raw(data))
}

func HmacSha256Raw(message, key []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(message)
	return h.Sum(nil)
}

func HmacSha256(message, key string) string {
	return hex.EncodeToString(HmacSha256Raw([]byte(message), []byte(key)))
}

// HashShard returns a stable shard suffix in range [0, shardCount-1].
// When shardCount <= 1, it returns "0" to keep key deterministic.
func HashShard(input string, shardCount int) string {
	if shardCount <= 1 {
		return "0"
	}
	sum := crc32.ChecksumIEEE([]byte(input))
	return strconv.FormatUint(uint64(sum%uint32(shardCount)), 10)
}
