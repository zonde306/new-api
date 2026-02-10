package system_setting

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

type DiscordSettings struct {
	Enabled      bool   `json:"enabled"`
	ClientId     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Guilds       string `json:"guilds"`
}

type DiscordGuildRule struct {
	Or  []string `json:"or"`
	And []string `json:"and"`
}

// 默认配置
var defaultDiscordSettings = DiscordSettings{}

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("discord", &defaultDiscordSettings)
}

func GetDiscordSettings() *DiscordSettings {
	return &defaultDiscordSettings
}

func ParseDiscordGuildRule(raw string) (*DiscordGuildRule, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return &DiscordGuildRule{}, nil
	}

	var rule DiscordGuildRule
	if err := common.UnmarshalJsonStr(trimmed, &rule); err != nil {
		return nil, err
	}

	rule.Or = normalizeDiscordGuildIDs(rule.Or)
	rule.And = normalizeDiscordGuildIDs(rule.And)
	return &rule, nil
}

func (r *DiscordGuildRule) IsEmpty() bool {
	return r == nil || (len(r.Or) == 0 && len(r.And) == 0)
}

func normalizeDiscordGuildIDs(input []string) []string {
	if len(input) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(input))
	result := make([]string, 0, len(input))
	for _, item := range input {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
