package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadRendered renders a scaffold, writes it, and loads it back. A generated
// configuration that does not load is the failure this guards against.
func loadRendered(t *testing.T, s Scaffold) (string, Config) {
	t.Helper()
	rendered, err := s.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	secrets, err := s.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range secrets {
		t.Setenv(name, value)
	}
	// Keys the template may refer to.
	t.Setenv("ANTHROPIC_API_KEY", "placeholder")
	t.Setenv("HOMEASSISTANT_TOKEN", "placeholder")

	path := filepath.Join(t.TempDir(), "riverbed.toml")
	if err := os.WriteFile(path, []byte(rendered), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the generated configuration must load:\n%v\n\n%s", err, rendered)
	}
	return rendered, cfg
}

func TestScaffoldMinimal(t *testing.T) {
	rendered, cfg := loadRendered(t, Scaffold{})

	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
	if cfg.Router.Default != JournalAgent {
		t.Errorf("default route = %q", cfg.Router.Default)
	}
	if cfg.Embedding.Enabled() {
		t.Error("no model was requested, so embedding should be off")
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("agents = %v", cfg.Agents)
	}
	// One rule is always written, so the journal prefix works out of the box.
	if len(cfg.Router.Rules) != 1 || cfg.Router.Rules[0].Prefix != "note" {
		t.Errorf("rules = %+v", cfg.Router.Rules)
	}
	if strings.Contains(rendered, "base_url") {
		t.Error("no base URL was requested, so the key should be absent")
	}
}

func TestScaffoldHoldsNoSecrets(t *testing.T) {
	s := Scaffold{MCPServe: true}
	rendered, _ := loadRendered(t, s)

	secrets, err := s.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range secrets {
		if strings.Contains(rendered, value) {
			t.Errorf("the token for %s was written into the configuration", name)
		}
		if !strings.Contains(rendered, "${"+name+"}") {
			t.Errorf("the configuration should refer to ${%s}", name)
		}
	}
}

func TestScaffoldSecretsAreDistinctAndRandom(t *testing.T) {
	s := Scaffold{MCPServe: true}
	first, err := s.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	if first[WebhookTokenEnv] == first[MCPTokenEnv] {
		t.Error("the webhook and mcp tokens must differ")
	}
	if len(first[WebhookTokenEnv]) != 64 {
		t.Errorf("token length = %d, want 64 hex characters", len(first[WebhookTokenEnv]))
	}
	second, err := s.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	if first[WebhookTokenEnv] == second[WebhookTokenEnv] {
		t.Error("tokens must not repeat between calls")
	}
}

func TestScaffoldWithoutMCPServeGeneratesNoMCPToken(t *testing.T) {
	s := Scaffold{}
	secrets, err := s.Secrets()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := secrets[MCPTokenEnv]; ok {
		t.Error("an unused token should not be generated")
	}
}

func TestScaffoldWithEmbedding(t *testing.T) {
	_, cfg := loadRendered(t, Scaffold{EmbedKind: "potion", EmbedModel: "potion-base-8M"})
	if !cfg.Embedding.Enabled() {
		t.Fatal("embedding should be enabled")
	}
	if cfg.Embedding.Model != "potion-base-8M" {
		t.Errorf("model = %q", cfg.Embedding.Model)
	}
}

func TestScaffoldWithAgent(t *testing.T) {
	_, cfg := loadRendered(t, Scaffold{
		MCPServe: true,
		Agent: &ScaffoldAgent{
			Name: "shelley", Kind: "claude", Model: "claude-sonnet-5",
			APIKeyEnv: "ANTHROPIC_API_KEY",
		},
	})

	if len(cfg.Agents) != 1 {
		t.Fatalf("agents = %+v", cfg.Agents)
	}
	agent := cfg.Agents[0]
	if agent.Name != "shelley" || agent.Kind != "claude" || agent.Model != "claude-sonnet-5" {
		t.Errorf("agent = %+v", agent)
	}
	if agent.APIKey != "placeholder" {
		t.Errorf("the key should come from the environment, got %q", agent.APIKey)
	}

	// A prefix rule routing to the agent must be present and listed first.
	if len(cfg.Router.Rules) != 2 {
		t.Fatalf("rules = %+v", cfg.Router.Rules)
	}
	if cfg.Router.Rules[0].Prefix != "hey shelley" || cfg.Router.Rules[0].Agent != "shelley" {
		t.Errorf("first rule = %+v", cfg.Router.Rules[0])
	}
	if !cfg.Router.Rules[0].Strip {
		t.Error("the wake prefix should be stripped")
	}
	if !cfg.MCPServe.Enabled {
		t.Error("mcp_serve should be enabled")
	}
}

func TestScaffoldAgentKinds(t *testing.T) {
	for name, agent := range map[string]ScaffoldAgent{
		"claude": {Kind: "claude", Model: "claude-sonnet-5", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"gemini": {Kind: "gemini", Model: "gemini-2.5-flash", APIKeyEnv: "ANTHROPIC_API_KEY"},
		"openai": {Kind: "openai", Model: "deepseek-chat", BaseURL: "https://api.example.invalid/v1"},
		"http":   {Kind: "http", BaseURL: "http://localhost:9000/prompt"},
	} {
		t.Run(name, func(t *testing.T) {
			copied := agent
			_, cfg := loadRendered(t, Scaffold{Agent: &copied})
			if len(cfg.Agents) != 1 || cfg.Agents[0].Kind != agent.Kind {
				t.Errorf("agents = %+v", cfg.Agents)
			}
		})
	}
}

func TestScaffoldDefaultsAgentNameAndPrefix(t *testing.T) {
	s := Scaffold{Agent: &ScaffoldAgent{Kind: "claude", Model: "m", APIKeyEnv: "ANTHROPIC_API_KEY"}}
	s.Defaults()
	if s.Agent.Name != "claude" {
		t.Errorf("name = %q, want the kind", s.Agent.Name)
	}
	if s.Agent.Prefix != "hey claude" {
		t.Errorf("prefix = %q", s.Agent.Prefix)
	}
}

func TestScaffoldWithHomeAssistant(t *testing.T) {
	_, cfg := loadRendered(t, Scaffold{
		Agent: &ScaffoldAgent{
			Name: "claude", Kind: "claude", Model: "claude-sonnet-5",
			APIKeyEnv: "ANTHROPIC_API_KEY",
		},
		HomeAssistantURL: "http://homeassistant.example.lan:8123/mcp_server/sse",
	})

	if len(cfg.MCP) != 1 {
		t.Fatalf("mcp servers = %+v", cfg.MCP)
	}
	server := cfg.MCP[0]
	if server.Name != "homeassistant" || server.Transport != "sse" || server.Auth != "bearer" {
		t.Errorf("server = %+v", server)
	}
	if len(server.Agents) != 1 || server.Agents[0] != "claude" {
		t.Errorf("the server should be offered to the agent, got %v", server.Agents)
	}
}

func TestScaffoldRejectsInconsistentInput(t *testing.T) {
	for name, s := range map[string]Scaffold{
		"unknown embed kind":      {EmbedKind: "telepathy"},
		"unknown agent kind":      {Agent: &ScaffoldAgent{Kind: "telepathy"}},
		"claude without a model":  {Agent: &ScaffoldAgent{Kind: "claude"}},
		"http without a base URL": {Agent: &ScaffoldAgent{Kind: "http"}},
		"reserved agent name":     {Agent: &ScaffoldAgent{Name: JournalAgent, Kind: "claude", Model: "m"}},
		"agent name with a space": {Agent: &ScaffoldAgent{Name: "my agent", Kind: "claude", Model: "m"}},
		"home assistant alone":    {HomeAssistantURL: "http://example.invalid/sse"},
	} {
		t.Run(name, func(t *testing.T) {
			copied := s
			if _, err := copied.Render(); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestScaffoldWithBaseURL(t *testing.T) {
	rendered, cfg := loadRendered(t, Scaffold{BaseURL: "https://riverbed.example.com"})
	if cfg.Server.BaseURL != "https://riverbed.example.com" {
		t.Errorf("base URL = %q", cfg.Server.BaseURL)
	}
	if !strings.Contains(rendered, "base_url") {
		t.Error("the key should be present")
	}
}

func TestScaffoldRetainAudio(t *testing.T) {
	_, cfg := loadRendered(t, Scaffold{RetainAudio: true})
	if !cfg.Audio.Retain {
		t.Error("audio retention should be on")
	}
	_, cfg = loadRendered(t, Scaffold{RetainAudio: false})
	if cfg.Audio.Retain {
		t.Error("audio retention should be off")
	}
}

func TestEnvFile(t *testing.T) {
	rendered := EnvFile(map[string]string{
		MCPTokenEnv:     "bbb",
		WebhookTokenEnv: "aaa",
	})
	if !strings.Contains(rendered, WebhookTokenEnv+"=aaa") {
		t.Errorf("missing the webhook token:\n%s", rendered)
	}
	// Sorted, so regenerating produces no spurious differences.
	if strings.Index(rendered, MCPTokenEnv) > strings.Index(rendered, WebhookTokenEnv) {
		t.Errorf("entries should be sorted:\n%s", rendered)
	}
	if !strings.Contains(rendered, "out of version control") {
		t.Error("the file should warn that it holds secrets")
	}
}
