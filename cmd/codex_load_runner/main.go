package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
)

type headerFlags []string

func (h *headerFlags) String() string {
	return strings.Join(*h, ",")
}

func (h *headerFlags) Set(value string) error {
	*h = append(*h, value)
	return nil
}

func main() {
	var headers headerFlags
	var opts helps.CodexLoadRunnerOptions

	flag.StringVar(&opts.TargetURL, "target", "", "Target URL, for example http://127.0.0.1:8317/v1/responses")
	flag.StringVar(&opts.APIKey, "api-key", "", "Bearer API key")
	flag.StringVar(&opts.Model, "model", "gpt-5.4", "Model name to send")
	flag.StringVar(&opts.Scenario, "scenario", "fast-complete", "Mock scenario injected via metadata.cpa_mock_scenario")
	flag.BoolVar(&opts.Stream, "stream", true, "Send stream=true in the request body")
	flag.IntVar(&opts.Requests, "requests", 1, "Total request count")
	flag.IntVar(&opts.Concurrency, "concurrency", 1, "Concurrent worker count")
	flag.DurationVar(&opts.Timeout, "timeout", 2*time.Minute, "Per-request timeout")
	flag.DurationVar(&opts.CancelAfter, "cancel-after", 0, "Cancel each request after this delay")
	flag.StringVar(&opts.RequestInput, "input", "hello", "Request input text")
	flag.Var(&headers, "header", "Extra request header in Key=Value form (repeatable)")
	flag.Parse()

	opts.Headers = make(map[string]string, len(headers))
	for _, header := range headers {
		key, value, ok := strings.Cut(header, "=")
		if !ok || strings.TrimSpace(key) == "" {
			_, _ = fmt.Fprintf(os.Stderr, "invalid header %q, expected Key=Value\n", header)
			os.Exit(2)
		}
		opts.Headers[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	summary, err := helps.RunCodexLoad(context.Background(), opts)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "codex load runner error: %v\n", err)
		os.Exit(1)
	}
	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to render summary: %v\n", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintln(os.Stdout, string(body))
}
