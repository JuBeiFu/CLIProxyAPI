package proxypool

import "strings"

func passiveNormalizedError(errText string) string {
	return strings.ToLower(strings.TrimSpace(errText))
}

func passiveErrorIsClientAbort(errText string) bool {
	text := passiveNormalizedError(errText)
	if text == "" {
		return false
	}
	return strings.Contains(text, "context canceled") ||
		strings.Contains(text, "client disconnected") ||
		strings.Contains(text, "client closed")
}

func passiveErrorIsIncompleteStream(errText string) bool {
	text := passiveNormalizedError(errText)
	if text == "" {
		return false
	}
	return strings.Contains(text, "stream disconnected before completion") ||
		strings.Contains(text, "stream closed before response.completed")
}

func passiveErrorIsRouteTransportFailure(errText string) bool {
	text := passiveNormalizedError(errText)
	if text == "" {
		return false
	}
	return strings.Contains(text, "network is unreachable") ||
		strings.Contains(text, "no route to host") ||
		strings.Contains(text, "no such host") ||
		strings.Contains(text, "server misbehaving")
}
