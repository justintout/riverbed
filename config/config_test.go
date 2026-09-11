package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "riverbed.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
[webhook]
token = "shared-secret"
[store]
path = "test.db"
`

func TestLoadMinimal(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %q, want the default :8080", cfg.Server.Addr)
	}
	if cfg.Router.Default != JournalAgent {
		t.Errorf("router.default = %q, want %q", cfg.Router.Default, JournalAgent)
	}
	if cfg.Embedding.Enabled() {
		t.Error("embedding should be off when no model is named")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("RIVERBED_TEST_TOKEN", `quote"and\slash`)
	cfg, err := Load(write(t, `
[webhook]
token = "${RIVERBED_TEST_TOKEN}"
[store]
path = "test.db"
`))
	if err != nil {
		t.Fatal(err)
	}
	if want := `quote"and\slash`; cfg.Webhook.Token != want {
		t.Errorf("token = %q, want %q", cfg.Webhook.Token, want)
	}
}

func TestExpandMissingEnvIsAnError(t *testing.T) {
	_, err := Load(write(t, `
[webhook]
token = "${RIVERBED_DEFINITELY_UNSET_VAR}"
`))
	if err == nil {
		t.Fatal("want an error for an unset variable, got nil")
	}
	if !strings.Contains(err.Error(), "RIVERBED_DEFINITELY_UNSET_VAR") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("RIVERBED_ADDR", ":9999")
	t.Setenv("RIVERBED_EMBED_MODEL", "potion-base-8M")
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("addr = %q, want the override :9999", cfg.Server.Addr)
	}
	if !cfg.Embedding.Enabled() {
		t.Error("naming a model in the environment should enable embedding")
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = ""
	cfg.Router.Default = "nobody"
	cfg.Agents = []Agent{{Name: "a", Kind: "mystery"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("want errors, got nil")
	}
	for _, want := range []string{"webhook.token", "router.default", "unknown kind"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%v", want, err)
		}
	}
}

func TestReservedAgentName(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.Agents = []Agent{{Name: JournalAgent, Kind: "http", BaseURL: "http://example.invalid"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("want a reserved-name error, got %v", err)
	}
}

func TestRoutingToJournalIsValid(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.Router.Rules = []Rule{{Prefix: "note", Agent: JournalAgent, Strip: true}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("routing to the journal should be valid: %v", err)
	}
}

func TestRuleValidation(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Webhook.Token = "x"
		return c
	}
	for name, tc := range map[string]struct {
		rule Rule
		want string
	}{
		"no matcher":    {Rule{Agent: JournalAgent}, "prefix or a regex"},
		"both matchers": {Rule{Prefix: "a", Regex: "b", Agent: JournalAgent}, "both"},
		"bad regex":     {Rule{Regex: "([", Agent: JournalAgent}, "regex"},
		"strip a regex": {Rule{Regex: "^a", Strip: true, Agent: JournalAgent}, "strip"},
		"unknown agent": {Rule{Prefix: "a", Agent: "ghost"}, "not a configured agent"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			cfg.Router.Rules = []Rule{tc.rule}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestOAuthNeedsBaseURL(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.MCP = []MCP{{Name: "s", URL: "https://example.invalid/mcp", Transport: "streamable", Auth: "oauth"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("want a base_url error, got %v", err)
	}
}

func TestMCPFor(t *testing.T) {
	cfg := Config{MCP: []MCP{
		{Name: "shared"},
		{Name: "claude-only", Agents: []string{"claude"}},
	}}
	got := cfg.MCPFor("claude")
	if len(got) != 2 {
		t.Fatalf("claude should see both servers, got %d", len(got))
	}
	got = cfg.MCPFor("other")
	if len(got) != 1 || got[0].Name != "shared" {
		t.Errorf("other should see only the shared server, got %v", got)
	}
}

func TestExpandIgnoresComments(t *testing.T) {
	// A commented-out agent must not keep demanding its secret.
	cfg, err := Load(write(t, `
[webhook]
token = "shared-secret"
[store]
path = "test.db"
# [[agent]]
# name = "claude"
# api_key = "${SOME_KEY_THAT_IS_NOT_SET}"
`))
	if err != nil {
		t.Fatalf("a reference inside a comment should be ignored: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("agents = %v", cfg.Agents)
	}
}

func TestExpandIgnoresTrailingComments(t *testing.T) {
	_, err := Load(write(t, `
[webhook]
token = "shared-secret" # see ${ALSO_NOT_SET} for the value
[store]
path = "test.db"
`))
	if err != nil {
		t.Errorf("a trailing comment should be ignored: %v", err)
	}
}

func TestExpandIgnoresLiteralStrings(t *testing.T) {
	// A single-quoted string is raw in TOML, so it is left alone.
	cfg, err := Load(write(t, `
[webhook]
token = 'literal ${NOT_SET} value'
[store]
path = "test.db"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Webhook.Token != "literal ${NOT_SET} value" {
		t.Errorf("token = %q, want it verbatim", cfg.Webhook.Token)
	}
}

func TestExpandInMultilineString(t *testing.T) {
	t.Setenv("RIVERBED_TEST_SYSTEM", "be brief")
	cfg, err := Load(write(t, `
[webhook]
token = "t"
[store]
path = "test.db"
[[agent]]
name = "a"
kind = "http"
base_url = "http://localhost:9000"
system = """
${RIVERBED_TEST_SYSTEM}
"""
`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Agents[0].System, "be brief") {
		t.Errorf("system = %q", cfg.Agents[0].System)
	}
}

func TestExpandLeavesNonReferencesAlone(t *testing.T) {
	cfg, err := Load(write(t, `
[webhook]
token = "price is $5 {literally} and ${} too"
[store]
path = "test.db"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Webhook.Token != "price is $5 {literally} and ${} too" {
		t.Errorf("token = %q", cfg.Webhook.Token)
	}
}

func TestExpandHandlesEscapedQuote(t *testing.T) {
	t.Setenv("RIVERBED_TEST_TOKEN2", "secret")
	cfg, err := Load(write(t, `
[webhook]
token = "a \" quote then ${RIVERBED_TEST_TOKEN2}"
[store]
path = "test.db"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Webhook.Token != `a " quote then secret` {
		t.Errorf("token = %q", cfg.Webhook.Token)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	for _, key := range []string{
		"RIVERBED_WEBHOOK_TOKEN", "RIVERBED_MCP_TOKEN",
		"ANTHROPIC_API_KEY", "HOMEASSISTANT_TOKEN",
	} {
		t.Setenv(key, "placeholder")
	}
	if _, err := Load("../riverbed.example.toml"); err != nil {
		t.Errorf("the shipped example must load: %v", err)
	}
}
