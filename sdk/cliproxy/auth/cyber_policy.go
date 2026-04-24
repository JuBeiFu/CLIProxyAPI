package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/tidwall/gjson"
)

const (
	cyberPolicyTriggerCountKey        = "cyber_policy_trigger_count"
	cyberPolicyLastTriggeredAtKey     = "cyber_policy_last_triggered_at"
	cyberPolicyLastModelKey           = "cyber_policy_last_model"
	cyberPolicyLastRequestPreviewKey  = "cyber_policy_last_request_preview"
	cyberPolicyLastRequestSHA256Key   = "cyber_policy_last_request_sha256"
	cyberPolicyAuditFilename          = "cyber_policy_audit.jsonl"
	cyberPolicyRequestPreviewMaxBytes = 4096
)

type cyberPolicyAuditRecord struct {
	Timestamp      string `json:"timestamp"`
	AuthID         string `json:"auth_id"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Count          int    `json:"count"`
	Status         int    `json:"status,omitempty"`
	ErrorMessage   string `json:"error_message,omitempty"`
	RequestSHA256  string `json:"request_sha256,omitempty"`
	RequestPreview string `json:"request_preview,omitempty"`
	RequestPayload string `json:"request_payload,omitempty"`
}

func authCyberPolicyTriggerCount(auth *Auth) int {
	if auth == nil || auth.Metadata == nil {
		return 0
	}
	return metadataInt(auth.Metadata[cyberPolicyTriggerCountKey])
}

func metadataInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case int32:
		return int(v)
	case float64:
		return int(v)
	case float32:
		return int(v)
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return int(n)
		}
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil {
			return n
		}
	}
	return 0
}

func isCyberPolicyResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isCyberPolicyErrorMessage(err.Message) || strings.EqualFold(strings.TrimSpace(err.Code), "cyber_policy")
}

func isCyberPolicyErrorMessage(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		return false
	}
	if strings.Contains(strings.ToLower(message), "cyber_policy") {
		return true
	}
	if !gjson.Valid(message) {
		return false
	}
	paths := []string{
		"error.code",
		"code",
		"response.error.code",
		"response.metadata.openai_verification_recommendation",
		"metadata.openai_verification_recommendation",
	}
	for _, path := range paths {
		if strings.EqualFold(strings.TrimSpace(gjson.Get(message, path).String()), "cyber_policy") ||
			strings.EqualFold(strings.TrimSpace(gjson.Get(message, path).String()), "trusted_access_for_cyber") {
			return true
		}
	}
	return false
}

func recordCyberPolicyTriggerLocked(auth *Auth, result Result, now time.Time) cyberPolicyAuditRecord {
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	count := authCyberPolicyTriggerCount(auth) + 1
	hash, preview := cyberPolicyRequestSnapshot(result.RequestPayload)
	auth.Metadata[cyberPolicyTriggerCountKey] = count
	auth.Metadata[cyberPolicyLastTriggeredAtKey] = now.UTC().Format(time.RFC3339Nano)
	auth.Metadata[cyberPolicyLastModelKey] = result.Model
	if hash != "" {
		auth.Metadata[cyberPolicyLastRequestSHA256Key] = hash
	}
	if preview != "" {
		auth.Metadata[cyberPolicyLastRequestPreviewKey] = preview
	}

	record := cyberPolicyAuditRecord{
		Timestamp:      now.UTC().Format(time.RFC3339Nano),
		AuthID:         result.AuthID,
		Provider:       result.Provider,
		Model:          result.Model,
		Count:          count,
		RequestSHA256:  hash,
		RequestPreview: preview,
		RequestPayload: string(result.RequestPayload),
	}
	if result.Error != nil {
		record.Status = result.Error.StatusCode()
		record.ErrorMessage = result.Error.Message
	}
	return record
}

func cyberPolicyRequestSnapshot(payload []byte) (string, string) {
	if len(payload) == 0 {
		return "", ""
	}
	sum := sha256.Sum256(payload)
	preview := payload
	if len(preview) > cyberPolicyRequestPreviewMaxBytes {
		preview = preview[:cyberPolicyRequestPreviewMaxBytes]
	}
	return hex.EncodeToString(sum[:]), string(preview)
}

func cyberPolicyCooldown(count int) time.Duration {
	if count <= 0 {
		count = 1
	}
	cooldown := time.Duration(count) * 5 * time.Minute
	if cooldown > 2*time.Hour {
		return 2 * time.Hour
	}
	return cooldown
}

func appendCyberPolicyAudit(cfg *internalconfig.Config, record cyberPolicyAuditRecord) error {
	logDir := logging.ResolveLogDirectory(cfg)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return appendFile(filepath.Join(logDir, cyberPolicyAuditFilename), line, 0o644)
}

func appendFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
