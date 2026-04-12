package auth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	log "github.com/sirupsen/logrus"
)

const banRecordDirName = ".system/ban-records"

var banRecordWriteMu sync.Mutex

// BanRecord captures one auto-deleted auth credential event.
type BanRecord struct {
	Name      string     `json:"name,omitempty"`
	Account   string     `json:"account,omitempty"`
	Provider  string     `json:"provider,omitempty"`
	Source    string     `json:"source,omitempty"`
	Reason    string     `json:"reason"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	BannedAt  time.Time  `json:"banned_at"`
}

func appendBanRecord(auth *Auth, reason string, source string, now time.Time) error {
	recordPath, ok := banRecordPathForAuth(auth, now)
	if !ok {
		return nil
	}
	if source == "" {
		source = "request"
	}
	record := buildBanRecord(auth, reason, source, now)
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		return err
	}

	banRecordWriteMu.Lock()
	defer banRecordWriteMu.Unlock()

	file, err := os.OpenFile(recordPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()

	_, err = file.Write(raw)
	return err
}

// LoadBanRecordsForDay loads all ban records for the given local day from auth-dir.
func LoadBanRecordsForDay(authDir string, day time.Time) ([]BanRecord, error) {
	recordPath := banRecordPath(authDir, day)
	file, err := os.Open(recordPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []BanRecord{}, nil
		}
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	records := make([]BanRecord, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record BanRecord
		if err := json.Unmarshal(line, &record); err != nil {
			log.WithError(err).Warnf("failed to decode ban record line from %s", recordPath)
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].BannedAt.After(records[j].BannedAt)
	})
	return records, nil
}

func buildBanRecord(auth *Auth, reason string, source string, now time.Time) BanRecord {
	record := BanRecord{
		Name:     banRecordName(auth),
		Account:  banRecordAccount(auth),
		Provider: strings.TrimSpace(auth.Provider),
		Source:   strings.TrimSpace(source),
		Reason:   strings.TrimSpace(reason),
		BannedAt: now,
	}
	if createdAt := banRecordCreatedAt(auth); createdAt != nil && !createdAt.IsZero() {
		ts := createdAt.UTC()
		record.CreatedAt = &ts
	}
	return record
}

func banRecordPathForAuth(auth *Auth, now time.Time) (string, bool) {
	authDir := banRecordAuthDir(auth)
	if authDir == "" {
		return "", false
	}
	return banRecordPath(authDir, now), true
}

func banRecordPath(authDir string, now time.Time) string {
	day := now
	if day.IsZero() {
		day = time.Now()
	}
	return filepath.Join(authDir, banRecordDirName, "banned-auth-records-"+day.Format("2006-01-02")+".jsonl")
}

func banRecordAuthDir(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			return filepath.Dir(path)
		}
	}
	for _, candidate := range []string{strings.TrimSpace(auth.FileName), strings.TrimSpace(auth.ID)} {
		if candidate == "" {
			continue
		}
		if filepath.IsAbs(candidate) {
			return filepath.Dir(candidate)
		}
	}
	return ""
}

func banRecordName(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return filepath.Base(name)
	}
	return strings.TrimSpace(auth.ID)
}

func banRecordAccount(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if _, account := auth.AccountInfo(); strings.TrimSpace(account) != "" {
		return strings.TrimSpace(account)
	}
	if claims := banRecordCodexClaims(auth); claims != nil {
		if email := strings.TrimSpace(claims.Email); email != "" {
			return email
		}
	}
	if auth.Metadata != nil {
		for _, key := range []string{"account", "username", "email"} {
			if value, ok := auth.Metadata[key].(string); ok {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	if auth.Attributes != nil {
		for _, key := range []string{"account", "username", "account_email", "email"} {
			if trimmed := strings.TrimSpace(auth.Attributes[key]); trimmed != "" {
				return trimmed
			}
		}
	}
	return banRecordName(auth)
}

func banRecordCreatedAt(auth *Auth) *time.Time {
	if claims := banRecordCodexClaims(auth); claims != nil {
		if ts, ok := parseTimeValue(claims.CodexAuthInfo.ChatgptSubscriptionActiveStart); ok && !ts.IsZero() {
			createdAt := ts.UTC()
			return &createdAt
		}
	}
	if auth != nil && !auth.CreatedAt.IsZero() {
		createdAt := auth.CreatedAt.UTC()
		return &createdAt
	}
	return nil
}

func banRecordCodexClaims(auth *Auth) *codex.JWTClaims {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		return nil
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil {
		return nil
	}
	return claims
}
