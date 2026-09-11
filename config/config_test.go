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
		"no matcher":             {Rule{Agent: JournalAgent}, "needs a prefix, a regex or utterances"},
		"both matchers":          {Rule{Prefix: "a", Regex: "b", Agent: JournalAgent}, "more than one matcher"},
		"prefix and utterances":  {Rule{Prefix: "a", Utterances: []string{"x"}, Agent: JournalAgent}, "more than one matcher"},
		"strip a semantic match": {Rule{Utterances: []string{"x"}, Strip: true, Agent: JournalAgent}, "strip"},
		"threshold out of range": {Rule{Utterances: []string{"x"}, Threshold: 1.5, Agent: JournalAgent}, "between 0 and 1"},
		"empty utterance":        {Rule{Utterances: []string{"x", "  "}, Agent: JournalAgent}, "is empty"},
		"bad regex":              {Rule{Regex: "([", Agent: JournalAgent}, "regex"},
		"strip a regex":          {Rule{Regex: "^a", Strip: true, Agent: JournalAgent}, "strip"},
		"unknown agent":          {Rule{Prefix: "a", Agent: "ghost"}, "not a configured agent"},
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
	// The key has a required format, so a placeholder will not do.
	t.Setenv("RIVERBED_SECRET_KEY", strings.Repeat("ab", 32))
	if _, err := Load("../riverbed.example.toml"); err != nil {
		t.Errorf("the shipped example must load: %v", err)
	}
}

func TestSemanticRulesRequireAnEmbedder(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.Router.Rules = []Rule{{Utterances: []string{"turn on the lights"}, Agent: JournalAgent}}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "embedding model is required") {
		t.Errorf("want an embedder error, got %v", err)
	}

	cfg.Embedding.Model = "potion-base-8M"
	if err := cfg.Validate(); err != nil {
		t.Errorf("with a model configured it should be valid: %v", err)
	}
}

func TestRouterThreshold(t *testing.T) {
	r := Router{}
	if got := r.Threshold(Rule{}); got != DefaultSemanticThreshold {
		t.Errorf("threshold = %v, want the default", got)
	}

	r.SemanticThreshold = 0.6
	if got := r.Threshold(Rule{}); got != 0.6 {
		t.Errorf("threshold = %v, want the router value", got)
	}
	if got := r.Threshold(Rule{Threshold: 0.8}); got != 0.8 {
		t.Errorf("threshold = %v, want the rule to win", got)
	}
}

func TestRouterSemantic(t *testing.T) {
	if (Router{Rules: []Rule{{Prefix: "note"}}}).Semantic() {
		t.Error("a prefix rule is not semantic")
	}
	if !(Router{Rules: []Rule{{Prefix: "note"}, {Utterances: []string{"x"}}}}).Semantic() {
		t.Error("one semantic rule makes the router semantic")
	}
}

func TestSemanticThresholdRange(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.Router.SemanticThreshold = 2
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "between 0 and 1") {
		t.Errorf("want a range error, got %v", err)
	}
}

func TestSemanticRuleLoadsFromTOML(t *testing.T) {
	cfg, err := Load(write(t, `
[webhook]
token = "t"
[store]
path = "test.db"
[embedding]
kind = "potion"
model = "potion-base-8M"
[router]
default = "journal"
semantic_threshold = 0.42
[[router.rule]]
utterances = ["turn on the kitchen lights", "dim the lamp"]
threshold = 0.55
agent = "journal"
tags = ["home"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Router.SemanticThreshold != 0.42 {
		t.Errorf("router threshold = %v", cfg.Router.SemanticThreshold)
	}
	rule := cfg.Router.Rules[0]
	if !rule.Semantic() || len(rule.Utterances) != 2 {
		t.Errorf("rule = %+v", rule)
	}
	if got := cfg.Router.Threshold(rule); got != 0.55 {
		t.Errorf("effective threshold = %v, want the rule value", got)
	}
}

func TestOAuthRequiresASecretKey(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.Server.BaseURL = "https://riverbed.example"
	cfg.MCP = []MCP{{
		Name: "s", URL: "https://example.invalid/mcp", Transport: "streamable", Auth: "oauth",
	}}

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "store.secret_key is required") {
		t.Fatalf("want a secret key error, got %v", err)
	}

	cfg.Store.SecretKey = strings.Repeat("ab", 32)
	if err := cfg.Validate(); err != nil {
		t.Errorf("with a key it should be valid: %v", err)
	}
}

func TestBearerAuthNeedsNoSecretKey(t *testing.T) {
	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.MCP = []MCP{{
		Name: "s", URL: "https://example.invalid/sse", Transport: "sse",
		Auth: "bearer", Token: "t",
	}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a bearer server stores no credentials: %v", err)
	}
}

func TestSecretKeyFormat(t *testing.T) {
	for name, key := range map[string]string{
		"too short": strings.Repeat("ab", 8),
		"not hex":   strings.Repeat("zz", 32),
		"too long":  strings.Repeat("ab", 64),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Webhook.Token = "x"
			cfg.Store.SecretKey = key
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "secret_key") {
				t.Errorf("want a format error, got %v", err)
			}
		})
	}

	cfg := Default()
	cfg.Webhook.Token = "x"
	cfg.Store.SecretKey = strings.ToUpper(strings.Repeat("ab", 32))
	if err := cfg.Validate(); err != nil {
		t.Errorf("uppercase hex should be accepted: %v", err)
	}
}

func TestSecretKeyEnvOverride(t *testing.T) {
	key := strings.Repeat("cd", 32)
	t.Setenv("RIVERBED_SECRET_KEY", key)
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.SecretKey != key {
		t.Errorf("secret key = %q", cfg.Store.SecretKey)
	}
}

func TestScaffoldGeneratesASecretKey(t *testing.T) {
	s := Scaffold{}
	secrets, err := s.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	key, ok := secrets[SecretKeyEnv]
	if !ok {
		t.Fatal("a generated configuration should come with a key")
	}
	if !isHexKey(key) {
		t.Errorf("generated key = %q, which is not usable", key)
	}

	rendered, err := s.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "secret_key = \"${"+SecretKeyEnv+"}\"") {
		t.Errorf("the configuration should refer to the key:\n%s", rendered)
	}
	if strings.Contains(rendered, key) {
		t.Error("the key itself must not be written into the configuration")
	}
}
