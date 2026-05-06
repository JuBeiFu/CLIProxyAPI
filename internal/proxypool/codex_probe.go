package proxypool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexroute"
)

type ProbeTarget struct {
	AuthID string
	Route  RouteDescriptor
}

type ProbeOutcome struct {
	CheckedAt       time.Time
	FirstByte       time.Duration
	FirstPayload    time.Duration
	Successful      bool
	RouteConsistent bool
	TerminalReason  string
}

type CodexProbeConfig struct {
	ProbeURL string
	Client   *http.Client
}

type CodexProbeRunner struct {
	cfg CodexProbeConfig
}

const (
	codexProbeAuthIDHeader     = "X-CLIProxy-Probe-Auth-ID"
	codexProbeProxyPoolHeader  = "X-CLIProxy-Probe-Proxy-Pool"
	codexProbeProxyEntryHeader = "X-CLIProxy-Probe-Proxy-Entry"
	codexProbeDirectHeader     = "X-CLIProxy-Probe-Direct"
)

func NewCodexProbeRunner(cfg CodexProbeConfig) *CodexProbeRunner {
	return &CodexProbeRunner{cfg: cfg}
}

func (r *CodexProbeRunner) Probe(ctx context.Context, target ProbeTarget) (ProbeOutcome, error) {
	if r == nil {
		return ProbeOutcome{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	payload := map[string]any{
		"model": "gpt-5.4",
		"input": "ping",
		"metadata": map[string]any{
			"codex_probe_auth_id":     strings.TrimSpace(target.AuthID),
			"codex_probe_proxy_pool":  strings.TrimSpace(target.Route.Pool),
			"codex_probe_proxy_entry": strings.TrimSpace(target.Route.Entry),
			"codex_probe_direct":      target.Route.Direct,
		},
	}
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return ProbeOutcome{}, errMarshal
	}

	reqCtx := codexroute.WithRequestRoute(ctx, target.Route.RequestRoute())
	req, errReq := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimSpace(r.cfg.ProbeURL), bytes.NewReader(body))
	if errReq != nil {
		return ProbeOutcome{}, errReq
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(codexProbeAuthIDHeader, strings.TrimSpace(target.AuthID))
	req.Header.Set(codexProbeProxyPoolHeader, strings.TrimSpace(target.Route.Pool))
	req.Header.Set(codexProbeProxyEntryHeader, strings.TrimSpace(target.Route.Entry))
	req.Header.Set(codexProbeDirectHeader, strconv.FormatBool(target.Route.Direct))

	client := r.cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	startedAt := time.Now()
	resp, errDo := client.Do(req)
	if errDo != nil {
		return ProbeOutcome{}, errDo
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	outcome := ProbeOutcome{
		CheckedAt:       time.Now(),
		FirstByte:       time.Since(startedAt),
		RouteConsistent: resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices,
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		text := strings.ToLower(strings.TrimSpace(string(bodyBytes)))
		if strings.Contains(text, "plan_mismatch") || strings.Contains(text, "free route") {
			outcome.RouteConsistent = false
			outcome.TerminalReason = "plan_mismatch"
			return outcome, nil
		}
		outcome.TerminalReason = strings.TrimSpace(resp.Status)
		return outcome, nil
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		if outcome.FirstPayload <= 0 {
			outcome.FirstPayload = time.Since(startedAt)
		}
		eventType := eventTypeFromJSON(raw)
		switch eventType {
		case "response.output_text.delta", "response.completed":
			outcome.Successful = true
		}
		if eventType == "response.completed" {
			outcome.TerminalReason = "response.completed"
			return outcome, nil
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return ProbeOutcome{}, errScan
	}
	if outcome.Successful {
		outcome.TerminalReason = "stream_ended"
	}
	return outcome, nil
}

func eventTypeFromJSON(raw string) string {
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Type)
}
