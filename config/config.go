// Package config loads and validates Riverbed's configuration.
//
// Configuration is a TOML file. Every string value may reference environment
// variables as ${VAR}, which keeps secrets out of the file. A handful of
// environment variables also override the file directly so that a container can
// be configured without one.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is a complete Riverbed configuration.
type Config struct {
	Server    Server    `toml:"server"`
	Webhook   Webhook   `toml:"webhook"`
	Store     Store     `toml:"store"`
	Audio     Audio     `toml:"audio"`
	Embedding Embedding `toml:"embedding"`
	MCPServe  MCPServe  `toml:"mcp_serve"`
	UI        UI        `toml:"ui"`
	Router    Router    `toml:"router"`
	Agents    []Agent   `toml:"agent"`
	MCP       []MCP     `toml:"mcp"`
}

// Server holds HTTP listener settings.
type Server struct {
	Addr string `toml:"addr"`
	// BaseURL is Riverbed's externally reachable URL. It is required only to
	// build OAuth redirect URLs.
	BaseURL string `toml:"base_url"`
}

// Webhook holds settings for the recording receiver.
type Webhook struct {
	Path string `toml:"path"`
	// Token is compared against the bearer token in the Authorization header.
	Token string `toml:"token"`
	// Workers is the number of recordings processed concurrently.
	Workers int `toml:"workers"`
}

// Store holds database settings.
type Store struct {
	Path     string `toml:"path"`
	PoolSize int    `toml:"pool_size"`
	// SecretKey encrypts the OAuth tokens and client secrets kept in the
	// database. It is 64 hexadecimal characters, which is 32 bytes. It is
	// required when an MCP server uses OAuth.
	SecretKey string `toml:"secret_key"`
}

// Audio controls retention of the recorded audio.
type Audio struct {
	Retain   bool  `toml:"retain"`
	MaxBytes int64 `toml:"max_bytes"`
}

// Embedding configures the embedder. Vector storage and hybrid retrieval are
// enabled only when Model is set.
type Embedding struct {
	// Kind is "potion", "goformer" or "remote".
	Kind string `toml:"kind"`
	// Model names a potion model ("potion-base-8M"), a goformer model
	// directory, or a model id for a remote endpoint.
	Model string `toml:"model"`
	// BaseURL and APIKey apply to kind "remote" only.
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	// Dim truncates the embedding. Zero keeps the model's native dimension.
	Dim int `toml:"dim"`
}

// Enabled reports whether embeddings, and therefore vector retrieval, are on.
func (e Embedding) Enabled() bool { return e.Model != "" }

// MCPServe configures the MCP server Riverbed exposes for its own journal.
type MCPServe struct {
	Enabled bool   `toml:"enabled"`
	Path    string `toml:"path"`
	Token   string `toml:"token"`
}

// UI configures the web interface.
type UI struct {
	Enabled bool `toml:"enabled"`
	// Password guards the interface. Empty leaves it open to anyone who can
	// reach the listener.
	Password string `toml:"password"`
}

// SecretKeyHexLength is the length of store.secret_key, which is 32 bytes as
// hexadecimal.
const SecretKeyHexLength = 64

// isHexKey reports whether s is a secret key of the right length. The key itself
// is parsed by the store; this only rejects an obviously wrong value early, so
// that validation stays free of dependencies.
func isHexKey(s string) bool {
	if len(s) != SecretKeyHexLength {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// DefaultSemanticThreshold is the cosine similarity a semantic rule requires
// when neither the rule nor the router sets one.
//
// The usable range depends on the embedding model, so this is a starting point
// rather than a good value for every model. Measure it with "riverbed route".
const DefaultSemanticThreshold = 0.5

// Router decides which agent, if any, handles a transcription.
type Router struct {
	// Default is the agent used when no rule matches and no classifier is
	// configured, or when the classifier declines. Use JournalAgent to store
	// without calling an agent.
	Default string `toml:"default"`
	// Classifier names an agent used to choose a route when no rule matches.
	// Empty disables classification.
	Classifier string `toml:"classifier"`
	// SemanticThreshold is the similarity semantic rules require when they set
	// none of their own. Zero means DefaultSemanticThreshold.
	SemanticThreshold float64 `toml:"semantic_threshold"`
	Rules             []Rule  `toml:"rule"`
}

// Semantic reports whether any rule matches by meaning, which requires an
// embedder.
func (r Router) Semantic() bool {
	for _, rule := range r.Rules {
		if rule.Semantic() {
			return true
		}
	}
	return false
}

// Threshold returns the similarity the rule requires.
func (r Router) Threshold(rule Rule) float64 {
	switch {
	case rule.Threshold > 0:
		return rule.Threshold
	case r.SemanticThreshold > 0:
		return r.SemanticThreshold
	default:
		return DefaultSemanticThreshold
	}
}

// Rule matches a transcription by spoken prefix, by regular expression, or by
// meaning.
//
// A rule sets exactly one matcher. Utterances are examples of what the route
// handles; a transcription matches when its embedding is at least Threshold
// similar to one of them, which needs no model call.
type Rule struct {
	Prefix     string   `toml:"prefix"`
	Regex      string   `toml:"regex"`
	Utterances []string `toml:"utterances"`
	Threshold  float64  `toml:"threshold"`
	Agent      string   `toml:"agent"`
	Strip      bool     `toml:"strip"`
	Tags       []string `toml:"tags"`
}

// Semantic reports whether the rule matches by meaning.
func (r Rule) Semantic() bool { return len(r.Utterances) > 0 }

// JournalAgent is the reserved agent name meaning "store only, call nothing".
const JournalAgent = "journal"

// Agent is one configured agent.
type Agent struct {
	Name string `toml:"name"`
	// Kind is "claude", "gemini", "openai" or "http".
	Kind    string `toml:"kind"`
	Model   string `toml:"model"`
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	System  string `toml:"system"`
	// MaxTokens caps the reply length. Zero uses the provider default.
	MaxTokens int64 `toml:"max_tokens"`
	// MaxTurns caps tool-use rounds per request.
	MaxTurns int      `toml:"max_turns"`
	Timeout  Duration `toml:"timeout"`
	// Headers are extra HTTP headers, used by kind "http".
	Headers map[string]string `toml:"headers"`
}

// MCP is one MCP server whose tools are offered to agents.
type MCP struct {
	Name string `toml:"name"`
	URL  string `toml:"url"`
	// Transport is "streamable" or "sse".
	Transport string `toml:"transport"`
	// Auth is "none", "bearer" or "oauth".
	Auth  string `toml:"auth"`
	Token string `toml:"token"`
	// Scopes are requested during the OAuth flow.
	Scopes []string `toml:"scopes"`
	// Agents lists the agents that may use this server. Empty means all.
	Agents []string `toml:"agents"`
}

// Duration is a time.Duration that unmarshals from a TOML string such as "30s".
type Duration struct {
	time.Duration
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Or returns d, or def when d is zero.
func (d Duration) Or(def time.Duration) time.Duration {
	if d.Duration == 0 {
		return def
	}
	return d.Duration
}

// Default returns the configuration used before a file is applied.
func Default() Config {
	return Config{
		Server:    Server{Addr: ":8080"},
		Webhook:   Webhook{Path: "/webhook/recording", Workers: 2},
		Store:     Store{Path: "riverbed.db", PoolSize: 8},
		Audio:     Audio{Retain: true, MaxBytes: 32 << 20},
		Embedding: Embedding{Kind: "potion"},
		MCPServe:  MCPServe{Path: "/mcp"},
		Router:    Router{Default: JournalAgent},
	}
}

// Load reads the TOML file at path, expands ${VAR} references, applies
// environment overrides, and validates the result. An empty path loads the
// defaults with environment overrides only.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		expanded, err := expand(string(data))
		if err != nil {
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
		if err := toml.Unmarshal([]byte(expanded), &cfg); err != nil {
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// expand replaces every ${VAR} inside a double-quoted TOML string with the
// environment value.
//
// Only double-quoted strings are touched. A reference in a comment is left
// alone, so commenting out an agent does not keep demanding its secret, and a
// single-quoted literal string is left alone because TOML processes no escapes
// there, so a substituted value needing one would be corrupted.
//
// A reference to an unset variable is an error: silently inserting an empty
// secret would produce a daemon that looks configured and is not.
func expand(s string) (string, error) {
	var (
		out     strings.Builder
		missing []string
	)
	out.Grow(len(s))

	for i := 0; i < len(s); {
		switch {
		case s[i] == '#':
			// A comment runs to the end of the line.
			end := strings.IndexByte(s[i:], '\n')
			if end < 0 {
				out.WriteString(s[i:])
				i = len(s)
				continue
			}
			out.WriteString(s[i : i+end+1])
			i += end + 1

		case strings.HasPrefix(s[i:], literalMulti):
			i = copyVerbatim(&out, s, i, literalMulti)

		case s[i] == '\'':
			i = copyVerbatim(&out, s, i, "'")

		case strings.HasPrefix(s[i:], basicMulti):
			i = expandString(&out, s, i, basicMulti, &missing)

		case s[i] == '"':
			i = expandString(&out, s, i, `"`, &missing)

		default:
			out.WriteByte(s[i])
			i++
		}
	}

	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out.String(), nil
}

// TOML's multi-line string delimiters.
const (
	basicMulti   = `"""`
	literalMulti = `'''`
)

// copyVerbatim copies a delimited run unchanged, both delimiters included, and
// returns the index just past it.
func copyVerbatim(out *strings.Builder, s string, i int, delim string) int {
	out.WriteString(delim)
	i += len(delim)
	for i < len(s) {
		if strings.HasPrefix(s[i:], delim) {
			out.WriteString(delim)
			return i + len(delim)
		}
		out.WriteByte(s[i])
		i++
	}
	return i
}

// expandString copies a double-quoted string, substituting ${VAR} as it goes,
// and returns the index just past the closing delimiter.
func expandString(out *strings.Builder, s string, i int, delim string, missing *[]string) int {
	out.WriteString(delim)
	i += len(delim)
	for i < len(s) {
		// A backslash escape is copied whole so that \" does not read as the
		// end of the string.
		if s[i] == '\\' && i+1 < len(s) {
			out.WriteString(s[i : i+2])
			i += 2
			continue
		}
		if strings.HasPrefix(s[i:], delim) {
			out.WriteString(delim)
			return i + len(delim)
		}
		if name, width, ok := envReference(s[i:]); ok {
			value, found := os.LookupEnv(name)
			if !found {
				*missing = append(*missing, name)
			}
			out.WriteString(tomlEscape(value))
			i += width
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return i
}

// envReference reads a ${NAME} reference at the start of s, returning the name
// and how many bytes it occupied.
func envReference(s string) (name string, width int, ok bool) {
	if !strings.HasPrefix(s, "${") {
		return "", 0, false
	}
	end := strings.IndexByte(s, '}')
	if end < 0 {
		return "", 0, false
	}
	name = s[2:end]
	if name == "" {
		return "", 0, false
	}
	for i, r := range name {
		switch {
		case r == '_',
			r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			i > 0 && r >= '0' && r <= '9':
		default:
			return "", 0, false
		}
	}
	return name, end + 1, true
}

// tomlEscape escapes characters that would otherwise break out of the TOML
// string the value is substituted into.
func tomlEscape(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *Config) applyEnv() {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	str("RIVERBED_ADDR", &c.Server.Addr)
	str("RIVERBED_BASE_URL", &c.Server.BaseURL)
	str("RIVERBED_DB", &c.Store.Path)
	str("RIVERBED_SECRET_KEY", &c.Store.SecretKey)
	str("RIVERBED_WEBHOOK_TOKEN", &c.Webhook.Token)
	str("RIVERBED_EMBED_KIND", &c.Embedding.Kind)
	str("RIVERBED_EMBED_MODEL", &c.Embedding.Model)
	str("RIVERBED_EMBED_BASE_URL", &c.Embedding.BaseURL)
	str("RIVERBED_EMBED_API_KEY", &c.Embedding.APIKey)
	str("RIVERBED_MCP_TOKEN", &c.MCPServe.Token)
	str("RIVERBED_UI_PASSWORD", &c.UI.Password)
}

// Validate reports whether the configuration describes a runnable system.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Server.Addr == "" {
		add("server.addr is required")
	}
	if !strings.HasPrefix(c.Webhook.Path, "/") {
		add("webhook.path must begin with /: %q", c.Webhook.Path)
	}
	if c.Webhook.Token == "" {
		add("webhook.token is required; the receiver is otherwise open to anyone")
	}
	if c.Webhook.Workers < 1 {
		add("webhook.workers must be at least 1")
	}
	if c.Store.Path == "" {
		add("store.path is required")
	}
	if c.Store.PoolSize < 2 {
		add("store.pool_size must be at least 2")
	}
	if c.Store.SecretKey != "" && !isHexKey(c.Store.SecretKey) {
		add("store.secret_key must be %d hexadecimal characters; generate one with \"riverbed key\"",
			SecretKeyHexLength)
	}
	if c.Audio.Retain && c.Audio.MaxBytes < 1 {
		add("audio.max_bytes must be positive when audio.retain is set")
	}

	if c.Embedding.Enabled() {
		switch c.Embedding.Kind {
		case "potion", "goformer":
		case "remote":
			if c.Embedding.BaseURL == "" {
				add("embedding.base_url is required for kind %q", c.Embedding.Kind)
			}
		default:
			add("embedding.kind must be potion, goformer or remote, not %q", c.Embedding.Kind)
		}
		if c.Embedding.Dim < 0 {
			add("embedding.dim cannot be negative")
		}
	}

	if c.MCPServe.Enabled {
		if !strings.HasPrefix(c.MCPServe.Path, "/") {
			add("mcp_serve.path must begin with /: %q", c.MCPServe.Path)
		}
		if c.MCPServe.Token == "" {
			add("mcp_serve.token is required while mcp_serve.enabled is set")
		}
		if c.MCPServe.Path == c.Webhook.Path {
			add("mcp_serve.path and webhook.path must differ")
		}
	}

	agents := make(map[string]bool, len(c.Agents))
	for i, a := range c.Agents {
		switch {
		case a.Name == "":
			add("agent[%d].name is required", i)
		case a.Name == JournalAgent:
			add("agent[%d].name %q is reserved for store-only routing", i, JournalAgent)
		case agents[a.Name]:
			add("duplicate agent name %q", a.Name)
		}
		agents[a.Name] = true

		switch a.Kind {
		case "claude", "gemini", "openai":
			if a.Model == "" {
				add("agent %q: model is required for kind %q", a.Name, a.Kind)
			}
		case "http":
			if a.BaseURL == "" {
				add("agent %q: base_url is required for kind \"http\"", a.Name)
			}
		case "":
			add("agent %q: kind is required", a.Name)
		default:
			add("agent %q: unknown kind %q", a.Name, a.Kind)
		}
		if a.MaxTurns < 0 {
			add("agent %q: max_turns cannot be negative", a.Name)
		}
	}

	routable := func(name string) bool { return name == JournalAgent || agents[name] }
	if c.Router.Default == "" {
		add("router.default is required")
	} else if !routable(c.Router.Default) {
		add("router.default %q is not a configured agent", c.Router.Default)
	}
	if c.Router.Classifier != "" && !agents[c.Router.Classifier] {
		add("router.classifier %q is not a configured agent", c.Router.Classifier)
	}
	if c.Router.SemanticThreshold < 0 || c.Router.SemanticThreshold > 1 {
		add("router.semantic_threshold must be between 0 and 1, not %v", c.Router.SemanticThreshold)
	}
	if c.Router.Semantic() && !c.Embedding.Enabled() {
		add("router rules match by meaning, so an embedding model is required")
	}
	for i, r := range c.Router.Rules {
		matchers := 0
		for _, set := range []bool{r.Prefix != "", r.Regex != "", r.Semantic()} {
			if set {
				matchers++
			}
		}
		switch matchers {
		case 1:
		case 0:
			add("router.rule[%d] needs a prefix, a regex or utterances", i)
		default:
			add("router.rule[%d] sets more than one matcher; use one per rule", i)
		}
		if r.Regex != "" {
			if _, err := regexp.Compile(r.Regex); err != nil {
				add("router.rule[%d] regex: %v", i, err)
			}
			if r.Strip {
				add("router.rule[%d] cannot strip a regex match; use a prefix", i)
			}
		}
		if r.Semantic() {
			if r.Strip {
				add("router.rule[%d] cannot strip a semantic match; use a prefix", i)
			}
			if r.Threshold < 0 || r.Threshold > 1 {
				add("router.rule[%d].threshold must be between 0 and 1, not %v", i, r.Threshold)
			}
			for j, utterance := range r.Utterances {
				if strings.TrimSpace(utterance) == "" {
					add("router.rule[%d].utterances[%d] is empty", i, j)
				}
			}
		}
		if r.Agent == "" {
			add("router.rule[%d].agent is required", i)
		} else if !routable(r.Agent) {
			add("router.rule[%d].agent %q is not a configured agent", i, r.Agent)
		}
	}

	servers := make(map[string]bool, len(c.MCP))
	for i, m := range c.MCP {
		switch {
		case m.Name == "":
			add("mcp[%d].name is required", i)
		case servers[m.Name]:
			add("duplicate mcp name %q", m.Name)
		}
		servers[m.Name] = true

		if m.URL == "" {
			add("mcp %q: url is required", m.Name)
		}
		switch m.Transport {
		case "streamable", "sse":
		case "":
			add("mcp %q: transport is required", m.Name)
		default:
			add("mcp %q: transport must be streamable or sse, not %q", m.Name, m.Transport)
		}
		switch m.Auth {
		case "none", "":
		case "bearer":
			if m.Token == "" {
				add("mcp %q: token is required for bearer auth", m.Name)
			}
		case "oauth":
			if m.Transport != "streamable" {
				add("mcp %q: oauth requires the streamable transport", m.Name)
			}
			if c.Server.BaseURL == "" {
				add("mcp %q: server.base_url is required to build the oauth redirect", m.Name)
			}
			if c.Store.SecretKey == "" {
				add("mcp %q: store.secret_key is required to encrypt the tokens oauth produces", m.Name)
			}
		default:
			add("mcp %q: unknown auth %q", m.Name, m.Auth)
		}
		for _, name := range m.Agents {
			if !agents[name] {
				add("mcp %q: agent %q is not configured", m.Name, name)
			}
		}
	}

	return errors.Join(errs...)
}

// Agent returns the named agent.
func (c *Config) Agent(name string) (Agent, bool) {
	for _, a := range c.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

// MCPFor returns the MCP servers whose tools the named agent may use.
func (c *Config) MCPFor(agent string) []MCP {
	var out []MCP
	for _, m := range c.MCP {
		if len(m.Agents) == 0 {
			out = append(out, m)
			continue
		}
		for _, name := range m.Agents {
			if name == agent {
				out = append(out, m)
				break
			}
		}
	}
	return out
}
