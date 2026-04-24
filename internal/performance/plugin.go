package performance

import (
	"context"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type Plugin struct {
	tracker *Tracker
}

var defaultTracker = NewTracker(DefaultConfig())

func init() {
	coreusage.RegisterPlugin(NewPlugin(defaultTracker))
}

func NewPlugin(tracker *Tracker) *Plugin {
	return &Plugin{tracker: tracker}
}

func DefaultTracker() *Tracker {
	return defaultTracker
}

func ConfigureDefault(cfg Config) {
	defaultTracker.Configure(cfg)
}

func (p *Plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || p.tracker == nil {
		return
	}
	p.tracker.Record(Sample{
		Provider:         record.Provider,
		AuthID:           record.AuthID,
		AuthIndex:        record.AuthIndex,
		Model:            record.Model,
		RequestedAt:      record.RequestedAt,
		Latency:          record.Latency,
		FirstByteLatency: record.FirstByteLatency,
		OutputTokens:     record.Detail.OutputTokens,
		TotalTokens:      record.Detail.TotalTokens,
		Failed:           record.Failed,
		StatusCode:       record.Detail.StatusCode,
	})
}
