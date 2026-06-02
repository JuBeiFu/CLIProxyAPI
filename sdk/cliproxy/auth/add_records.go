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

	log "github.com/sirupsen/logrus"
)

const addRecordDirName = ".system/add-records"

var addRecordWriteMu sync.Mutex

// AddRecord captures one "account added to the pool" event (a NEW auth file
// uploaded via the management API). It is the reliable per-day source for the
// "上号" (daily new accounts) chart — file modtime/created_at are not.
type AddRecord struct {
	Name      string     `json:"name,omitempty"`
	Account   string     `json:"account,omitempty"`
	Provider  string     `json:"provider,omitempty"`
	Plan      string     `json:"plan,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	AddedAt   time.Time  `json:"added_at"`
}

// AppendAddRecord logs a new-account event to
// .system/add-records/added-auth-records-YYYY-MM-DD.jsonl. Best-effort — callers
// MUST only invoke it for genuinely new auth files (not re-uploads), so a day's
// line count equals that day's new accounts.
func AppendAddRecord(authDir string, auth *Auth, now time.Time) error {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" || auth == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	record := AddRecord{
		Name:     banRecordName(auth),
		Account:  banRecordAccount(auth),
		Provider: strings.TrimSpace(auth.Provider),
		Plan:     authPlanType(auth),
		AddedAt:  now,
	}
	if createdAt := banRecordCreatedAt(auth); createdAt != nil && !createdAt.IsZero() {
		ts := createdAt.UTC()
		record.CreatedAt = &ts
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	recordPath := addRecordPath(authDir, now)
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o700); err != nil {
		return err
	}

	addRecordWriteMu.Lock()
	defer addRecordWriteMu.Unlock()

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

// LoadAddRecordsForDay loads all add records for the given local day.
func LoadAddRecordsForDay(authDir string, day time.Time) ([]AddRecord, error) {
	recordPath := addRecordPath(authDir, day)
	file, err := os.Open(recordPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []AddRecord{}, nil
		}
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()

	records := make([]AddRecord, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record AddRecord
		if err := json.Unmarshal(line, &record); err != nil {
			log.WithError(err).Warnf("failed to decode add record line from %s", recordPath)
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].AddedAt.After(records[j].AddedAt)
	})
	return records, nil
}

func addRecordPath(authDir string, day time.Time) string {
	if day.IsZero() {
		day = time.Now()
	}
	return filepath.Join(authDir, addRecordDirName, "added-auth-records-"+day.Format("2006-01-02")+".jsonl")
}
