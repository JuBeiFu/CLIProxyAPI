package executor

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	gptDrawUpstreamModelEnv     = "CODEX_GPTDRAW_UPSTREAM_MODEL"
	gptDrawUpstreamModelDefault = "gpt-5.4-mini"
	gptDrawQualityEnv           = "CODEX_GPTDRAW_QUALITY"
	gptDrawQualityDefault       = "high"
	gptDrawBackgroundEnv        = "CODEX_GPTDRAW_BACKGROUND"
	gptDrawBackgroundDefault    = "auto"

	// CODEX_AUTO_IMAGE_TOOL=0 disables auto-attaching image_generation to real
	// codex models. Any other value (or unset) leaves it enabled.
	codexAutoImageToolEnv          = "CODEX_AUTO_IMAGE_TOOL"
	codexAutoImageToolDefaultSize  = "1024x1024"
)

var gptDrawModelRegex = regexp.MustCompile(`^gpt-draw-(\d+)x(\d+)$`)

// parseGptDrawModel extracts width x height from a gpt-draw-<W>x<H> model name.
// Returns ("1024x1024", true) for "gpt-draw-1024x1024".
func parseGptDrawModel(name string) (string, bool) {
	m := gptDrawModelRegex.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return "", false
	}
	return m[1] + "x" + m[2], true
}

// IsGptDrawModel reports whether the given client-visible model name is a
// gpt-draw-<W>x<H> alias that should be transformed for codex image_generation.
func IsGptDrawModel(name string) bool {
	_, ok := parseGptDrawModel(name)
	return ok
}

// gptDrawUpstreamModel returns the upstream codex model used to service
// gpt-draw-* requests. Configurable via CODEX_GPTDRAW_UPSTREAM_MODEL.
func gptDrawUpstreamModel() string {
	if v := strings.TrimSpace(os.Getenv(gptDrawUpstreamModelEnv)); v != "" {
		return v
	}
	return gptDrawUpstreamModelDefault
}

func gptDrawQuality() string {
	if v := strings.TrimSpace(os.Getenv(gptDrawQualityEnv)); v != "" {
		return v
	}
	return gptDrawQualityDefault
}

func gptDrawBackground() string {
	if v := strings.TrimSpace(os.Getenv(gptDrawBackgroundEnv)); v != "" {
		return v
	}
	return gptDrawBackgroundDefault
}

// codexAutoImageToolEnabled reports whether auto-injection of the image_generation
// tool on real codex models is enabled (default on; disabled when CODEX_AUTO_IMAGE_TOOL=0).
func codexAutoImageToolEnabled() bool {
	return strings.TrimSpace(os.Getenv(codexAutoImageToolEnv)) != "0"
}

// isCodexFamilyModel reports whether a resolved upstream model name is a
// real codex family model (gpt-5*, excluding the synthetic gpt-draw-* names).
func isCodexFamilyModel(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "gpt-draw-") {
		return false
	}
	return strings.HasPrefix(name, "gpt-5")
}

// hasImageGenerationTool scans the tools array in a codex-format request body
// and returns true if any entry has type="image_generation".
func hasImageGenerationTool(body []byte) bool {
	toolsResult := gjson.GetBytes(body, "tools")
	if !toolsResult.Exists() || !toolsResult.IsArray() {
		return false
	}
	for _, t := range toolsResult.Array() {
		if strings.EqualFold(t.Get("type").String(), "image_generation") {
			return true
		}
	}
	return false
}

// maybeAttachImageGenerationTool appends a default image_generation tool to the
// body when the upstream model belongs to the codex family and the caller did
// not already provide one. It never sets tool_choice, so the model remains free
// to ignore the tool for non-image requests.
//
// The caller is responsible for ensuring gpt-draw payloads (which force both
// tools and tool_choice) skip this helper.
func maybeAttachImageGenerationTool(upstreamModel string, body []byte) []byte {
	if !codexAutoImageToolEnabled() {
		return body
	}
	if !isCodexFamilyModel(upstreamModel) {
		return body
	}
	if hasImageGenerationTool(body) {
		return body
	}
	tool := fmt.Sprintf(
		`{"type":"image_generation","size":"%s","quality":"%s","background":"%s"}`,
		codexAutoImageToolDefaultSize, gptDrawQuality(), gptDrawBackground(),
	)
	toolsResult := gjson.GetBytes(body, "tools")
	if toolsResult.Exists() && toolsResult.IsArray() {
		if updated, err := sjson.SetRawBytes(body, "tools.-1", []byte(tool)); err == nil {
			return updated
		}
		return body
	}
	if updated, err := sjson.SetRawBytes(body, "tools", []byte("["+tool+"]")); err == nil {
		return updated
	}
	return body
}

// transformGptDrawPayload rewrites a codex-format responses payload so the
// upstream receives a real codex model plus a forced image_generation tool.
// It returns the upstream model name and the mutated payload.
//
// Size comes from the requested gpt-draw-<W>x<H> model name. The caller is
// expected to have verified the requested model matches gpt-draw-*.
func transformGptDrawPayload(requestedModel string, body []byte) (string, []byte) {
	size, ok := parseGptDrawModel(requestedModel)
	if !ok {
		return "", body
	}

	upstream := gptDrawUpstreamModel()

	tools := fmt.Sprintf(
		`[{"type":"image_generation","size":"%s","quality":"%s","background":"%s"}]`,
		size, gptDrawQuality(), gptDrawBackground(),
	)
	toolChoice := `{"type":"image_generation"}`

	out := body
	if updated, err := sjson.SetBytes(out, "model", upstream); err == nil {
		out = updated
	}
	if updated, err := sjson.SetRawBytes(out, "tools", []byte(tools)); err == nil {
		out = updated
	}
	if updated, err := sjson.SetRawBytes(out, "tool_choice", []byte(toolChoice)); err == nil {
		out = updated
	}
	out, _ = sjson.DeleteBytes(out, "reasoning")
	out, _ = sjson.DeleteBytes(out, "response_format")

	return upstream, out
}
