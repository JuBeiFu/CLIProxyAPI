package helps

import "time"

type codexUpstreamMockProfile struct {
	Name                  string
	FirstEventDelay       time.Duration
	ForceScenario         string
	RateLimitEvery        int64
	InitialRateLimitCount int64
	ForceInjectedEOF      bool
}

var builtInCodexUpstreamMockProfiles = map[string]codexUpstreamMockProfile{
	"fast":                   {Name: "fast"},
	"slow-first-byte":        {Name: "slow-first-byte", FirstEventDelay: 300 * time.Millisecond},
	"intermittent-429":       {Name: "intermittent-429", RateLimitEvery: 3},
	"burst-429-then-recover": {Name: "burst-429-then-recover", InitialRateLimitCount: 2},
	"eof-before-completion":  {Name: "eof-before-completion", ForceInjectedEOF: true},
	"stall-before-output":    {Name: "stall-before-output", ForceScenario: "stall-before-output"},
}

func (m *CodexUpstreamMock) selectProfile(name string) codexUpstreamMockProfile {
	if m == nil {
		return codexUpstreamMockProfile{}
	}
	if profile, ok := builtInCodexUpstreamMockProfiles[name]; ok {
		return profile
	}
	return codexUpstreamMockProfile{}
}

func (m *CodexUpstreamMock) effectiveScenarioName(requestedScenario string, profile codexUpstreamMockProfile) string {
	if forced := profile.ForceScenario; forced != "" {
		return forced
	}
	return requestedScenario
}

func (m *CodexUpstreamMock) applyProfileToResponsesPlan(plan codexUpstreamMockResponsesPlan, profile codexUpstreamMockProfile, ordinal int64) codexUpstreamMockResponsesPlan {
	if shouldCodexUpstreamMockProfileRateLimit(profile, ordinal) {
		return codexUpstreamMockResponsesPlan{
			StatusCode:  429,
			ContentType: "application/json",
			Body:        `{"error":{"message":"mock rate limit","type":"usage_limit_reached","code":"rate_limit_exceeded"}}`,
			Outcome:     codexUpstreamMockOutcomeCompleted,
		}
	}

	cloned := cloneCodexUpstreamMockResponsesPlan(plan)
	if profile.FirstEventDelay > 0 && len(cloned.Events) > 0 {
		cloned.Events[0].Delay += profile.FirstEventDelay
	}
	if profile.ForceInjectedEOF {
		cloned.Events = stripCodexUpstreamMockCompletedEvents(cloned.Events)
		cloned.Outcome = codexUpstreamMockOutcomeInjectedEOF
	}
	return cloned
}

func (m *CodexUpstreamMock) applyProfileToCompactPlan(plan *codexUpstreamMockCompactPlan, profile codexUpstreamMockProfile, ordinal int64) *codexUpstreamMockCompactPlan {
	if shouldCodexUpstreamMockProfileRateLimit(profile, ordinal) {
		return &codexUpstreamMockCompactPlan{
			StatusCode:  429,
			ContentType: "application/json",
			Body:        `{"error":{"message":"mock rate limit","type":"usage_limit_reached","code":"rate_limit_exceeded"}}`,
		}
	}

	if plan == nil {
		plan = &codexUpstreamMockCompactPlan{}
	}
	cloned := *plan
	if profile.FirstEventDelay > 0 {
		cloned.Delay += profile.FirstEventDelay
	}
	if profile.ForceInjectedEOF {
		cloned.StatusCode = 502
		cloned.ContentType = "application/json"
		cloned.Body = `{"error":{"message":"mock upstream stream ended before completion","type":"server_error","code":"upstream_eof_before_completion"}}`
	}
	return &cloned
}

func cloneCodexUpstreamMockResponsesPlan(plan codexUpstreamMockResponsesPlan) codexUpstreamMockResponsesPlan {
	cloned := plan
	if len(plan.Events) > 0 {
		cloned.Events = append([]codexUpstreamMockResponsesEvent(nil), plan.Events...)
	}
	return cloned
}

func stripCodexUpstreamMockCompletedEvents(events []codexUpstreamMockResponsesEvent) []codexUpstreamMockResponsesEvent {
	if len(events) == 0 {
		return nil
	}
	filtered := make([]codexUpstreamMockResponsesEvent, 0, len(events))
	for _, event := range events {
		if event.Comment == "" && containsCodexUpstreamMockCompletedMarker(event.Data) {
			continue
		}
		filtered = append(filtered, event)
	}
	return filtered
}

func shouldCodexUpstreamMockProfileRateLimit(profile codexUpstreamMockProfile, ordinal int64) bool {
	if ordinal <= 0 {
		return false
	}
	if profile.InitialRateLimitCount > 0 && ordinal <= profile.InitialRateLimitCount {
		return true
	}
	return profile.RateLimitEvery > 0 && ordinal%profile.RateLimitEvery == 0
}
