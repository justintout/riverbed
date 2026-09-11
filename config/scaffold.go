package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// Scaffold describes the configuration to generate.
type Scaffold struct {
	Addr    string
	BaseURL string
	DBPath  string

	// EmbedKind and EmbedModel configure embedding. An empty EmbedModel leaves
	// retrieval keyword only.
	EmbedKind  string
	EmbedModel string

	// MCPServe adds the section that serves the journal over MCP.
	MCPServe bool

	// RetainAudio keeps the audio part of each recording.
	RetainAudio bool

	// Agent, when set, adds one agent and a prefix rule that routes to it.
	Agent *ScaffoldAgent

	// HomeAssistantURL, when set, adds a Home Assistant MCP server. It requires
	// an agent to offer the tools to.
	HomeAssistantURL string
}

// ScaffoldAgent is the single agent a generated configuration starts with.
type ScaffoldAgent struct {
	Name    string
	Kind    string
	Model   string
	BaseURL string
	// APIKeyEnv names the environment variable holding the key. It is left
	// empty for an agent that needs none.
	APIKeyEnv string
	// Prefix is the spoken prefix that routes to this agent. An empty Prefix
	// adds no rule.
	Prefix string
}

// Secret names the environment variables a generated configuration refers to.
const (
	WebhookTokenEnv = "RIVERBED_WEBHOOK_TOKEN"
	MCPTokenEnv     = "RIVERBED_MCP_TOKEN"
)

// Defaults fills in the values a scaffold leaves empty.
func (s *Scaffold) Defaults() {
	if s.Addr == "" {
		s.Addr = ":8080"
	}
	if s.DBPath == "" {
		s.DBPath = "riverbed.db"
	}
	if s.EmbedKind == "" {
		s.EmbedKind = "potion"
	}
	if s.Agent != nil {
		if s.Agent.Name == "" {
			s.Agent.Name = s.Agent.Kind
		}
		if s.Agent.Prefix == "" {
			s.Agent.Prefix = "hey " + s.Agent.Name
		}
	}
}

// Validate reports whether the scaffold describes a configuration that will
// load. It checks the scaffold's own consistency; Config.Validate checks the
// result.
func (s *Scaffold) Validate() error {
	switch s.EmbedKind {
	case "potion", "goformer", "remote":
	default:
		return fmt.Errorf("embedding kind must be potion, goformer or remote, not %q", s.EmbedKind)
	}
	if s.Agent != nil {
		switch s.Agent.Kind {
		case "claude", "gemini", "openai":
			if s.Agent.Model == "" {
				return fmt.Errorf("agent kind %q needs a model", s.Agent.Kind)
			}
		case "http":
			if s.Agent.BaseURL == "" {
				return fmt.Errorf("agent kind \"http\" needs a base URL")
			}
		default:
			return fmt.Errorf("agent kind must be claude, gemini, openai or http, not %q", s.Agent.Kind)
		}
		if strings.Contains(s.Agent.Name, " ") {
			return fmt.Errorf("agent name %q cannot contain a space", s.Agent.Name)
		}
		if s.Agent.Name == JournalAgent {
			return fmt.Errorf("agent name %q is reserved", JournalAgent)
		}
	}
	if s.HomeAssistantURL != "" && s.Agent == nil {
		return fmt.Errorf("a Home Assistant server needs an agent to offer its tools to")
	}
	return nil
}

// Secrets returns freshly generated tokens, keyed by the environment variable
// that a generated configuration reads them from.
//
// Tokens are returned rather than written into the configuration so that the
// file itself holds no secret and can be committed.
func (s *Scaffold) Secrets() (map[string]string, error) {
	secrets := map[string]string{}
	token, err := Token()
	if err != nil {
		return nil, err
	}
	secrets[WebhookTokenEnv] = token
	if s.MCPServe {
		token, err := Token()
		if err != nil {
			return nil, err
		}
		secrets[MCPTokenEnv] = token
	}
	return secrets, nil
}

// Token returns 32 random bytes as hex, for use as a shared secret.
func Token() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// EnvFile renders secrets as a file that a shell can source and systemd can
// read as an EnvironmentFile.
func EnvFile(secrets map[string]string) string {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Riverbed secrets. Keep this file out of version control.\n")
	b.WriteString("# Load it with: set -a; . ./riverbed.env; set +a\n")
	for _, name := range names {
		fmt.Fprintf(&b, "%s=%s\n", name, secrets[name])
	}
	return b.String()
}

// Render returns the TOML for the scaffold. Secrets are referenced as ${VAR}
// and never written into it.
func (s *Scaffold) Render() (string, error) {
	s.Defaults()
	if err := s.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	if err := scaffoldTemplate.Execute(&b, s); err != nil {
		return "", fmt.Errorf("render configuration: %w", err)
	}
	return b.String(), nil
}

// scaffoldTemplate is the generated configuration. It is deliberately sparse:
// riverbed.example.toml documents every option, and a generated file that
// repeats all of it is harder to read than one that states what was chosen.
var scaffoldTemplate = template.Must(template.New("riverbed.toml").Parse(
	`# Riverbed configuration, generated by "riverbed init".
# See riverbed.example.toml for every available option.
#
# Values written as ${VAR} are read from the environment, so this file holds no
# secrets. The generated tokens are in riverbed.env.

[server]
addr = "{{ .Addr }}"
{{- if .BaseURL }}
base_url = "{{ .BaseURL }}"
{{- end }}

[webhook]
path = "/webhook/recording"
token = "${` + WebhookTokenEnv + `}"
workers = 2

[store]
path = "{{ .DBPath }}"

[audio]
retain = {{ .RetainAudio }}

[embedding]
{{- if .EmbedModel }}
kind = "{{ .EmbedKind }}"
model = "{{ .EmbedModel }}"
{{- else }}
# No model is set, so retrieval uses keywords only. Set a model to enable
# search by meaning, for example:
#   kind = "potion"
#   model = "potion-base-8M"
kind = "{{ .EmbedKind }}"
model = ""
{{- end }}
{{- if .MCPServe }}

[mcp_serve]
enabled = true
path = "/mcp"
token = "${` + MCPTokenEnv + `}"
{{- end }}
{{- with .Agent }}

[[agent]]
name = "{{ .Name }}"
kind = "{{ .Kind }}"
{{- if .Model }}
model = "{{ .Model }}"
{{- end }}
{{- if .BaseURL }}
base_url = "{{ .BaseURL }}"
{{- end }}
{{- if .APIKeyEnv }}
api_key = "${{"{"}}{{ .APIKeyEnv }}}"
{{- end }}
timeout = "2m"
{{- end }}
{{- if .HomeAssistantURL }}

[[mcp]]
name = "homeassistant"
url = "{{ .HomeAssistantURL }}"
transport = "sse"
auth = "bearer"
token = "${HOMEASSISTANT_TOKEN}"
agents = ["{{ .Agent.Name }}"]
{{- end }}

[router]
# Recordings that match no rule are stored and embedded, and call no agent.
default = "{{ .Router.Default }}"
{{- with .Agent }}{{ if .Prefix }}

[[router.rule]]
prefix = "{{ .Prefix }}"
agent = "{{ .Name }}"
strip = true
tags = ["{{ .Name }}"]
{{- end }}{{ end }}

[[router.rule]]
prefix = "note"
agent = "journal"
strip = true
tags = ["note"]
`))

// Router reports the routing defaults the template writes.
func (s *Scaffold) Router() struct{ Default string } {
	return struct{ Default string }{Default: JournalAgent}
}
