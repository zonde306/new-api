package system_setting

import (
	"fmt"
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

type DiscordRoleSetProvider func(guildID string) (map[string]struct{}, error)

type DiscordGuildRule struct {
	RequiredGuilds  []*DiscordGuildClause
	OptionalGuilds  []*DiscordGuildClause
	ForbiddenGuilds []*DiscordGuildClause
}

type DiscordGuildClause struct {
	GuildID         string
	RequiredRoleIDs []string
	OptionalRoleIDs []string
	ForbiddenRoleIDs []string
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

	var rawRule map[string][]string
	if err := common.UnmarshalJsonStr(trimmed, &rawRule); err != nil {
		return nil, err
	}

	rule := &DiscordGuildRule{}
	requiredGuildSet := make(map[string]struct{})
	optionalGuildSet := make(map[string]struct{})
	forbiddenGuildSet := make(map[string]struct{})

	for rawGuildID, rawRoles := range rawRule {
		if strings.EqualFold(strings.TrimSpace(rawGuildID), "and") || strings.EqualFold(strings.TrimSpace(rawGuildID), "or") {
			// 丢弃不支持的 legacy 语法，等价于空规则项
			continue
		}

		guildPrefix, guildID, err := parseDiscordRuleID(rawGuildID)
		if err != nil {
			return nil, err
		}

		clause, err := buildDiscordGuildClause(guildID, rawRoles)
		if err != nil {
			return nil, err
		}

		switch guildPrefix {
		case '+':
			if _, exists := forbiddenGuildSet[guildID]; exists {
				return nil, fmt.Errorf("discord guild rule contains conflicted required and forbidden guild: %s", guildID)
			}
			if _, exists := optionalGuildSet[guildID]; exists {
				return nil, fmt.Errorf("discord guild rule contains conflicted optional and required guild: %s", guildID)
			}
			if _, exists := requiredGuildSet[guildID]; exists {
				continue
			}
			requiredGuildSet[guildID] = struct{}{}
			rule.RequiredGuilds = append(rule.RequiredGuilds, clause)
		case '-':
			if _, exists := requiredGuildSet[guildID]; exists {
				return nil, fmt.Errorf("discord guild rule contains conflicted required and forbidden guild: %s", guildID)
			}
			if _, exists := optionalGuildSet[guildID]; exists {
				return nil, fmt.Errorf("discord guild rule contains conflicted optional and forbidden guild: %s", guildID)
			}
			if _, exists := forbiddenGuildSet[guildID]; exists {
				continue
			}
			forbiddenGuildSet[guildID] = struct{}{}
			rule.ForbiddenGuilds = append(rule.ForbiddenGuilds, clause)
		default:
			if _, exists := requiredGuildSet[guildID]; exists {
				return nil, fmt.Errorf("discord guild rule contains conflicted optional and required guild: %s", guildID)
			}
			if _, exists := forbiddenGuildSet[guildID]; exists {
				return nil, fmt.Errorf("discord guild rule contains conflicted optional and forbidden guild: %s", guildID)
			}
			if _, exists := optionalGuildSet[guildID]; exists {
				continue
			}
			optionalGuildSet[guildID] = struct{}{}
			rule.OptionalGuilds = append(rule.OptionalGuilds, clause)
		}
	}

	return rule, nil
}

func (r *DiscordGuildRule) IsEmpty() bool {
	return r == nil || (len(r.RequiredGuilds) == 0 && len(r.OptionalGuilds) == 0 && len(r.ForbiddenGuilds) == 0)
}

func (r *DiscordGuildRule) Evaluate(guildSet map[string]struct{}, roleProvider DiscordRoleSetProvider) (bool, error) {
	if r == nil || r.IsEmpty() {
		return true, nil
	}
	if guildSet == nil {
		guildSet = make(map[string]struct{})
	}

	for _, clause := range r.RequiredGuilds {
		ok, err := clause.MatchGuildAndRoles(guildSet, roleProvider)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}

	for _, clause := range r.ForbiddenGuilds {
		ok, err := clause.MatchGuildAndRoles(guildSet, roleProvider)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}

	if len(r.OptionalGuilds) > 0 {
		matched := false
		for _, clause := range r.OptionalGuilds {
			ok, err := clause.MatchGuildAndRoles(guildSet, roleProvider)
			if err != nil {
				return false, err
			}
			if ok {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}

	return true, nil
}

func (c *DiscordGuildClause) MatchGuildAndRoles(guildSet map[string]struct{}, roleProvider DiscordRoleSetProvider) (bool, error) {
	if c == nil || c.GuildID == "" {
		return false, nil
	}
	if _, ok := guildSet[c.GuildID]; !ok {
		return false, nil
	}
	if len(c.RequiredRoleIDs) == 0 && len(c.OptionalRoleIDs) == 0 && len(c.ForbiddenRoleIDs) == 0 {
		return true, nil
	}
	if roleProvider == nil {
		return false, fmt.Errorf("discord role provider is nil")
	}

	roleSet, err := roleProvider(c.GuildID)
	if err != nil {
		return false, err
	}
	if roleSet == nil {
		roleSet = make(map[string]struct{})
	}

	for _, roleID := range c.RequiredRoleIDs {
		if _, ok := roleSet[roleID]; !ok {
			return false, nil
		}
	}
	for _, roleID := range c.ForbiddenRoleIDs {
		if _, ok := roleSet[roleID]; ok {
			return false, nil
		}
	}
	if len(c.OptionalRoleIDs) > 0 {
		matched := false
		for _, roleID := range c.OptionalRoleIDs {
			if _, ok := roleSet[roleID]; ok {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}

	return true, nil
}

func buildDiscordGuildClause(guildID string, rawRoles []string) (*DiscordGuildClause, error) {
	requiredRoles, optionalRoles, forbiddenRoles, err := parseDiscordRuleIDs(rawRoles)
	if err != nil {
		return nil, err
	}
	return &DiscordGuildClause{
		GuildID:          guildID,
		RequiredRoleIDs:  requiredRoles,
		OptionalRoleIDs:  optionalRoles,
		ForbiddenRoleIDs: forbiddenRoles,
	}, nil
}

func parseDiscordRuleIDs(input []string) ([]string, []string, []string, error) {
	if len(input) == 0 {
		return []string{}, []string{}, []string{}, nil
	}

	required := make([]string, 0, len(input))
	optional := make([]string, 0, len(input))
	forbidden := make([]string, 0, len(input))
	requiredSet := make(map[string]struct{}, len(input))
	optionalSet := make(map[string]struct{}, len(input))
	forbiddenSet := make(map[string]struct{}, len(input))

	for _, rawID := range input {
		prefix, id, err := parseDiscordRuleID(rawID)
		if err != nil {
			return nil, nil, nil, err
		}

		switch prefix {
		case '+':
			if _, exists := forbiddenSet[id]; exists {
				return nil, nil, nil, fmt.Errorf("discord guild rule contains conflicted required and forbidden id: %s", id)
			}
			if _, exists := requiredSet[id]; exists {
				continue
			}
			requiredSet[id] = struct{}{}
			required = append(required, id)
		case '-':
			if _, exists := requiredSet[id]; exists {
				return nil, nil, nil, fmt.Errorf("discord guild rule contains conflicted required and forbidden id: %s", id)
			}
			if _, exists := forbiddenSet[id]; exists {
				continue
			}
			forbiddenSet[id] = struct{}{}
			forbidden = append(forbidden, id)
		default:
			if _, exists := requiredSet[id]; exists {
				continue
			}
			if _, exists := forbiddenSet[id]; exists {
				continue
			}
			if _, exists := optionalSet[id]; exists {
				continue
			}
			optionalSet[id] = struct{}{}
			optional = append(optional, id)
		}
	}

	return required, optional, forbidden, nil
}

func parseDiscordRuleID(raw string) (byte, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, "", fmt.Errorf("discord guild rule contains empty id")
	}

	prefix := byte(0)
	id := trimmed
	if strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-") {
		prefix = trimmed[0]
		id = strings.TrimSpace(trimmed[1:])
		if id == "" {
			return 0, "", fmt.Errorf("discord guild rule contains invalid prefixed id: %s", raw)
		}
	}
	return prefix, id, nil
}
