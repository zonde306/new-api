package system_setting

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDiscordGuildRule_Empty(t *testing.T) {
	rule, err := ParseDiscordGuildRule("   ")
	require.NoError(t, err)
	require.NotNil(t, rule)
	require.True(t, rule.IsEmpty())
}

func TestParseDiscordGuildRule_LegacyAndOrIgnored(t *testing.T) {
	rule, err := ParseDiscordGuildRule(`{"and":["guild_1"],"or":["guild_2"]}`)
	require.NoError(t, err)
	require.NotNil(t, rule)
	require.True(t, rule.IsEmpty())
}

func TestParseDiscordGuildRule_ConflictGuildRejected(t *testing.T) {
	_, err := ParseDiscordGuildRule(`{"+guild_1":[],"-guild_1":[]}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "conflicted required and forbidden guild")
}

func TestParseDiscordGuildRule_ConflictOptionalAndForbiddenGuildRejected(t *testing.T) {
	_, err := ParseDiscordGuildRule(`{"guild_1":[],"-guild_1":[]}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "conflicted optional and forbidden guild")
}

func TestParseDiscordGuildRule_ConflictOptionalAndRequiredGuildRejected(t *testing.T) {
	_, err := ParseDiscordGuildRule(`{"guild_1":[],"+guild_1":[]}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "conflicted optional and required guild")
}

func TestParseDiscordGuildRule_ConflictRoleRejected(t *testing.T) {
	_, err := ParseDiscordGuildRule(`{"guild_1":["+role_1","-role_1"]}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "conflicted required and forbidden id")
}

func TestParseDiscordGuildRule_ParsePrefixSemantics(t *testing.T) {
	rule, err := ParseDiscordGuildRule(`{"guild_1":["-role_1","role_2","role_3","+role_4"],"-guild_2":[]}`)
	require.NoError(t, err)
	require.NotNil(t, rule)

	require.Len(t, rule.RequiredGuilds, 0)
	require.Len(t, rule.OptionalGuilds, 1)
	require.Len(t, rule.ForbiddenGuilds, 1)

	guild1 := findGuildClauseByID(t, rule.OptionalGuilds, "guild_1")
	require.Equal(t, []string{"role_4"}, guild1.RequiredRoleIDs)
	require.Equal(t, []string{"role_2", "role_3"}, guild1.OptionalRoleIDs)
	require.Equal(t, []string{"role_1"}, guild1.ForbiddenRoleIDs)

	guild2 := findGuildClauseByID(t, rule.ForbiddenGuilds, "guild_2")
	require.Empty(t, guild2.RequiredRoleIDs)
	require.Empty(t, guild2.OptionalRoleIDs)
	require.Empty(t, guild2.ForbiddenRoleIDs)
}

func TestDiscordGuildRule_EvaluateSamples(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		guilds      []string
		rolesByGuild map[string][]string
		wantMatch   bool
	}{
		{
			name:      "服务器1 AND 身份组1或身份组2",
			raw:       `{"server_1":["role_1","role_2"]}`,
			guilds:    []string{"server_1"},
			rolesByGuild: map[string][]string{"server_1": []string{"role_2"}},
			wantMatch: true,
		},
		{
			name:      "服务器1 AND 身份组1 AND 身份组2",
			raw:       `{"server_1":["+role_1","+role_2"]}`,
			guilds:    []string{"server_1"},
			rolesByGuild: map[string][]string{"server_1": []string{"role_1", "role_2"}},
			wantMatch: true,
		},
		{
			name:      "服务器1 AND NOT 身份组1",
			raw:       `{"server_1":["-role_1"]}`,
			guilds:    []string{"server_1"},
			rolesByGuild: map[string][]string{"server_1": []string{"role_2"}},
			wantMatch: true,
		},
		{
			name:      "服务器1 AND NOT 身份组1 AND 身份组2或身份组3 AND 身份组4",
			raw:       `{"server_1":["-role_1","role_2","role_3","+role_4"]}`,
			guilds:    []string{"server_1"},
			rolesByGuild: map[string][]string{"server_1": []string{"role_3", "role_4"}},
			wantMatch: true,
		},
		{
			name:      "NOT 服务器2",
			raw:       `{"-server_2":[]}`,
			guilds:    []string{"server_1"},
			rolesByGuild: map[string][]string{},
			wantMatch: true,
		},
		{
			name:      "NOT 服务器2且身份组1 子句命中时拒绝",
			raw:       `{"-server_2":["role_1"]}`,
			guilds:    []string{"server_2"},
			rolesByGuild: map[string][]string{"server_2": []string{"role_1"}},
			wantMatch: false,
		},
		{
			name:      "OR 服务器子句：满足其中一个",
			raw:       `{"server_1":["role_1"],"server_2":["role_2"]}`,
			guilds:    []string{"server_2"},
			rolesByGuild: map[string][]string{"server_2": []string{"role_2"}},
			wantMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, err := ParseDiscordGuildRule(tt.raw)
			require.NoError(t, err)

			guildSet := toSet(tt.guilds...)
			roles := make(map[string]map[string]struct{}, len(tt.rolesByGuild))
			for guildID, roleIDs := range tt.rolesByGuild {
				roles[guildID] = toSet(roleIDs...)
			}

			provider := func(guildID string) (map[string]struct{}, error) {
				if roleSet, ok := roles[guildID]; ok {
					return roleSet, nil
				}
				return map[string]struct{}{}, nil
			}

			matched, evalErr := rule.Evaluate(guildSet, provider)
			require.NoError(t, evalErr)
			require.Equal(t, tt.wantMatch, matched)
		})
	}
}

func TestDiscordGuildRule_EvaluateRoleProviderNilWhenRoleRuleExists(t *testing.T) {
	rule, err := ParseDiscordGuildRule(`{"server_1":["+role_1"]}`)
	require.NoError(t, err)

	matched, evalErr := rule.Evaluate(toSet("server_1"), nil)
	require.Error(t, evalErr)
	require.False(t, matched)
	require.Contains(t, evalErr.Error(), "role provider is nil")
}

func TestDiscordGuildRule_EvaluateRoleProviderNotRequiredWhenOnlyGuildRule(t *testing.T) {
	rule, err := ParseDiscordGuildRule(`{"server_1":[]}`)
	require.NoError(t, err)

	matched, evalErr := rule.Evaluate(toSet("server_1"), nil)
	require.NoError(t, evalErr)
	require.True(t, matched)
}

func TestDiscordGuildRule_EvaluateRoleProviderError(t *testing.T) {
	rule, err := ParseDiscordGuildRule(`{"server_1":["role_1"]}`)
	require.NoError(t, err)

	matched, evalErr := rule.Evaluate(toSet("server_1"), func(guildID string) (map[string]struct{}, error) {
		return nil, fmt.Errorf("mock provider error")
	})
	require.Error(t, evalErr)
	require.False(t, matched)
	require.Contains(t, evalErr.Error(), "mock provider error")
}

func toSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

func findGuildClauseByID(t *testing.T, clauses []*DiscordGuildClause, guildID string) *DiscordGuildClause {
	t.Helper()
	for _, c := range clauses {
		if c != nil && c.GuildID == guildID {
			return c
		}
	}
	require.FailNow(t, "guild clause not found", "guildID=%s", guildID)
	return nil
}
