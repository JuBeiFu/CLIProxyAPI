//go:build manualbench

package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

func TestManualServerCodexMockLoadCPA(t *testing.T) {
	const (
		totalAuths   = 16
		requestCount = 1024
		concurrency  = 64
	)

	testCases := []struct {
		name     string
		scenario string
		stream   bool
	}{
		{
			name:     "mock-fast-complete",
			scenario: "fast-complete",
			stream:   false,
		},
		{
			name:     "mock-reasoning-burst-before-output",
			scenario: "reasoning-burst-before-output",
			stream:   true,
		},
		{
			name:     "mock-fragmented-output-deltas",
			scenario: "fragmented-output-deltas",
			stream:   true,
		},
	}

	loggerModes := []struct {
		name  string
		setup func(*testing.T) func()
	}{
		{
			name: "quiet-logger",
			setup: func(_ *testing.T) func() {
				return muteManualBenchLogger()
			},
		},
		{
			name:  "info-logger-to-file",
			setup: manualBenchLoggerToFile,
		},
	}

	for _, mode := range loggerModes {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			restoreLogger := mode.setup(t)
			defer restoreLogger()

			mock := helps.NewCodexUpstreamMock()
			upstream := httptest.NewServer(mock.Handler())
			defer upstream.Close()

			cfg := &config.Config{}
			server, authIDs := manualCodexMockServerSetup(t, cfg, upstream.URL, totalAuths)
			defer cleanupManualCodexMockAuths(authIDs)

			httpServer := httptest.NewServer(server.engine)
			defer httpServer.Close()

			for _, tc := range testCases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					summary, err := helps.RunCodexLoad(context.Background(), helps.CodexLoadRunnerOptions{
						TargetURL:   httpServer.URL + "/v1/responses",
						Model:       "gpt-5.4",
						Scenario:    tc.scenario,
						Stream:      tc.stream,
						Requests:    requestCount,
						Concurrency: concurrency,
						Timeout:     30 * time.Second,
						Input:       "hello",
					})
					if err != nil {
						t.Fatalf("RunCodexLoad error: %v", err)
					}
					if summary.Failures != 0 {
						t.Fatalf("unexpected failures=%d summary=%+v", summary.Failures, summary)
					}
					t.Log(manualSummaryLine(mode.name+"-"+tc.name, summary))
				})
			}
		})
	}
}

func TestManualServerCodexMockLoadCPAFromAuthFiles(t *testing.T) {
	const (
		requestCount = 1024
		concurrency  = 64
	)

	restoreLogger := muteManualBenchLogger()
	defer restoreLogger()

	authDir := filepath.Join(
		"C:\\Users\\Administrator\\source\\repos\\CodexHelper",
		"output",
		"local-cpa-vs-sub2api-20260507",
		"cpa-authfile-16",
		"auth",
	)

	mock := helps.NewCodexUpstreamMock()
	upstream := httptest.NewServer(mock.Handler())
	defer upstream.Close()

	if err := rewriteManualCodexAuthBaseURLs(authDir, upstream.URL); err != nil {
		t.Fatalf("rewrite auth base URLs error: %v", err)
	}

	cfg := &config.Config{}
	server, authIDs := manualCodexMockServerFromAuthFiles(t, cfg, authDir)
	defer cleanupManualCodexMockAuths(authIDs)

	httpServer := httptest.NewServer(server.engine)
	defer httpServer.Close()

	testCases := []struct {
		name     string
		scenario string
		stream   bool
	}{
		{
			name:     "file-auth-fast-complete",
			scenario: "fast-complete",
			stream:   false,
		},
		{
			name:     "file-auth-reasoning-burst-before-output",
			scenario: "reasoning-burst-before-output",
			stream:   true,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			summary, err := helps.RunCodexLoad(context.Background(), helps.CodexLoadRunnerOptions{
				TargetURL:   httpServer.URL + "/v1/responses",
				Model:       "gpt-5.4",
				Scenario:    tc.scenario,
				Stream:      tc.stream,
				Requests:    requestCount,
				Concurrency: concurrency,
				Timeout:     30 * time.Second,
				Input:       "hello",
			})
			if err != nil {
				t.Fatalf("RunCodexLoad error: %v", err)
			}
			if summary.Failures != 0 {
				t.Fatalf("unexpected failures=%d summary=%+v", summary.Failures, summary)
			}
			t.Log(manualSummaryLine(tc.name, summary))
		})
	}
}

func manualCodexMockServerSetup(t *testing.T, cfg *config.Config, upstreamURL string, total int) (*Server, []string) {
	t.Helper()

	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))

	modelRegistry := registry.GetGlobalRegistry()
	authIDs := make([]string, 0, total)
	for index := 0; index < total; index++ {
		authID := "manual-codex-mock-" + strconv.Itoa(index)
		auth := &coreauth.Auth{
			ID:       authID,
			Provider: "codex",
			Attributes: map[string]string{
				"base_url": upstreamURL,
				"api_key":  "bench-key",
			},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", authID, err)
		}
		modelRegistry.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
		authIDs = append(authIDs, authID)
	}

	return NewServer(cfg, manager, sdkaccess.NewManager(), "manual-codex-mock-config.yaml"), authIDs
}

func manualCodexMockServerFromAuthFiles(t *testing.T, cfg *config.Config, authDir string) (*Server, []string) {
	t.Helper()

	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(authDir)

	manager := coreauth.NewManager(store, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
	manager.SetConfig(cfg)

	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("Load auths error: %v", err)
	}

	modelRegistry := registry.GetGlobalRegistry()
	auths := manager.List()
	authIDs := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		modelRegistry.RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}})
		authIDs = append(authIDs, auth.ID)
	}

	return NewServer(cfg, manager, sdkaccess.NewManager(), "manual-codex-file-config.yaml"), authIDs
}

func rewriteManualCodexAuthBaseURLs(authDir string, upstreamURL string) error {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(authDir, entry.Name())
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		updated, errSet := sjson.SetBytes(raw, "base_url", upstreamURL)
		if errSet != nil {
			return errSet
		}
		if errWrite := os.WriteFile(path, updated, 0o600); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func cleanupManualCodexMockAuths(authIDs []string) {
	modelRegistry := registry.GetGlobalRegistry()
	for _, authID := range authIDs {
		modelRegistry.UnregisterClient(authID)
	}
}

func manualBenchLoggerToFile(t *testing.T) func() {
	t.Helper()

	logger := log.StandardLogger()
	prevOut := logger.Out
	prevLevel := logger.GetLevel()
	prevFormatter := logger.Formatter
	prevReportCaller := logger.ReportCaller

	file, err := os.CreateTemp(t.TempDir(), "manual-bench-*.log")
	if err != nil {
		t.Fatalf("CreateTemp error: %v", err)
	}

	logger.SetOutput(file)
	logger.SetLevel(log.InfoLevel)
	logger.SetFormatter(&internallogging.LogFormatter{})
	logger.SetReportCaller(true)

	return func() {
		logger.SetOutput(prevOut)
		logger.SetLevel(prevLevel)
		logger.SetFormatter(prevFormatter)
		logger.SetReportCaller(prevReportCaller)
		_ = file.Close()
	}
}
