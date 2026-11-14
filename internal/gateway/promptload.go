package gateway

import (
	"fmt"
	"strings"

	"allin/internal/config"
	"allin/internal/prompt"
)

// loadPromptText 按配置读取要注入的系统提示词。
func loadPromptText(cfg *config.Config) (string, error) {
	mode := strings.TrimSpace(cfg.Prompt.Mode)
	if mode != prompt.ModeInject && mode != prompt.ModeReplace {
		return "", nil
	}
	if cfg.Prompt.File != "" {
		text, err := prompt.Config{Mode: mode, File: cfg.Prompt.File}.Load()
		if err != nil {
			return "", err
		}
		return text, nil
	}
	if strings.TrimSpace(cfg.Prompt.Inline) != "" {
		return strings.TrimSpace(cfg.Prompt.Inline), nil
	}
	return "", fmt.Errorf("prompt.mode=%s 但 file 与 inline 都为空", mode)
}
