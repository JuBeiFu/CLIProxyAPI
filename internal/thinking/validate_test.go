package thinking

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func TestValidateConfig_DowngradesXHighToHighForSameOpenAIFamily(t *testing.T) {
	modelInfo := registry.LookupModelInfo("gpt-5.1", "openai")
	if modelInfo == nil || modelInfo.Thinking == nil {
		t.Fatalf("expected thinking-capable model info, got %+v", modelInfo)
	}

	config := ThinkingConfig{
		Mode:  ModeLevel,
		Level: LevelXHigh,
	}

	got, err := ValidateConfig(config, modelInfo, "openai-response", "codex", false)
	if err != nil {
		t.Fatalf("ValidateConfig() error = %v", err)
	}
	if got == nil {
		t.Fatal("ValidateConfig() returned nil config")
	}
	if got.Mode != ModeLevel {
		t.Fatalf("Mode = %q, want %q", got.Mode, ModeLevel)
	}
	if got.Level != LevelHigh {
		t.Fatalf("Level = %q, want %q", got.Level, LevelHigh)
	}
}

func TestValidateConfig_StillRejectsOtherUnsupportedLevelsForSameOpenAIFamily(t *testing.T) {
	modelInfo := registry.LookupModelInfo("gpt-5", "openai")
	if modelInfo == nil || modelInfo.Thinking == nil {
		t.Fatalf("expected thinking-capable model info, got %+v", modelInfo)
	}

	config := ThinkingConfig{
		Mode:  ModeLevel,
		Level: LevelMax,
	}

	_, err := ValidateConfig(config, modelInfo, "openai-response", "codex", false)
	if err == nil {
		t.Fatal("ValidateConfig() error = nil, want unsupported level error")
	}

	thinkingErr, ok := err.(*ThinkingError)
	if !ok {
		t.Fatalf("ValidateConfig() error type = %T, want *ThinkingError", err)
	}
	if thinkingErr.Code != ErrLevelNotSupported {
		t.Fatalf("ThinkingError.Code = %q, want %q", thinkingErr.Code, ErrLevelNotSupported)
	}
}
