package weblogin

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// totpAt computes the 8-digit RFC-6238 TOTP for a base32 secret at unix time t.
func totpAt(secret string, unix int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", "")))
	if err != nil {
		return "", fmt.Errorf("totp: bad base32 secret: %w", err)
	}
	counter := uint64(unix / 30)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off]&0x7f) << 24) | (uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) | uint32(sum[off+3])
	return fmt.Sprintf("%08d", bin%100000000), nil
}

// TOTPNow returns the current 6-digit TOTP (OpenAI uses 6 digits).
func TOTPNow(secret string) (string, error) {
	code8, err := totpAt(secret, time.Now().Unix())
	if err != nil {
		return "", err
	}
	return code8[len(code8)-6:], nil
}
