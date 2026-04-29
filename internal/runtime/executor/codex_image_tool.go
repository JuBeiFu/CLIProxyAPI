package executor

import (
	"fmt"
	"os"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexImageToolQualityEnv        = "CODEX_IMAGE_TOOL_QUALITY"
	codexImageToolQualityDefault    = "high"
	codexImageToolBackgroundEnv     = "CODEX_IMAGE_TOOL_BACKGROUND"
	codexImageToolBackgroundDefault = "auto"

	// CODEX_AUTO_IMAGE_TOOL=0 disables auto-attaching image_generation to
	// codex models. Any other value, including unset, leaves it enabled.
	codexAutoImageToolEnv = "CODEX_AUTO_IMAGE_TOOL"
)

func codexImageToolQuality() string {
	if v := strings.TrimSpace(os.Getenv(codexImageToolQualityEnv)); v != "" {
		return v
	}
	return codexImageToolQualityDefault
}

func codexImageToolBackground() string {
	if v := strings.TrimSpace(os.Getenv(codexImageToolBackgroundEnv)); v != "" {
		return v
	}
	return codexImageToolBackgroundDefault
}

// codexAutoImageToolEnabled reports whether auto-injection of the
// image_generation tool on codex models is enabled.
func codexAutoImageToolEnabled() bool {
	return strings.TrimSpace(os.Getenv(codexAutoImageToolEnv)) != "0"
}

func isCodexFamilyModel(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	if strings.HasSuffix(name, "-codex-spark") {
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
// not already provide one. It omits size so user aspect ratio instructions can
// guide generation.
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
		`{"type":"image_generation","quality":"%s","background":"%s"}`,
		codexImageToolQuality(), codexImageToolBackground(),
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
