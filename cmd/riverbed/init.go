package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justintout/riverbed"
	"github.com/justintout/riverbed/config"
)

// initialize writes a configuration file and the secrets it refers to, then
// creates the database.
//
// It is non-interactive so that an agent or a script can run it, and it refuses
// to overwrite an existing file unless -force is given. Running it twice with
// the same flags and -force produces the same configuration, but new tokens.
func initialize(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	var c common
	c.bind(fs)

	out := fs.String("out", "riverbed.toml", "path of the configuration file to write")
	envOut := fs.String("env-out", "", "path of the secrets file to write (default: riverbed.env beside -out)")
	force := fs.Bool("force", false, "overwrite existing files")
	printOnly := fs.Bool("print", false, "print the configuration and the secrets, write nothing")

	addr := fs.String("addr", ":8080", "address the server listens on")
	baseURL := fs.String("base-url", "", "externally reachable URL, required only for MCP servers using OAuth")
	db := fs.String("db", "riverbed.db", "path of the SQLite database")
	retainAudio := fs.Bool("retain-audio", true, "store the audio of each recording")

	embedKind := fs.String("embed-kind", "potion", "potion, goformer or remote")
	embedModel := fs.String("embed-model", "potion-base-8M",
		`embedding model; empty disables embedding and leaves retrieval keyword only`)

	serveMCP := fs.Bool("serve-mcp", true, "serve the stored transcriptions as an MCP server")

	agentKind := fs.String("agent-kind", "", "claude, gemini, openai or http; empty configures no agent")
	agentName := fs.String("agent-name", "", "name of the agent (default: the kind)")
	agentModel := fs.String("agent-model", "", "model the agent uses")
	agentBaseURL := fs.String("agent-base-url", "", "base URL for kind openai or http")
	agentKeyEnv := fs.String("agent-key-env", "", "environment variable holding the agent API key")
	agentPrefix := fs.String("agent-prefix", "", `spoken prefix that routes to the agent (default: "hey <name>")`)

	homeAssistant := fs.String("home-assistant-url", "",
		"URL of the Home Assistant MCP server, for example http://host:8123/mcp_server/sse")

	migrateDB := fs.Bool("migrate", true, "create the database after writing the configuration")

	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: riverbed init [flags]")
		fmt.Fprintln(os.Stderr, "\nWrites a configuration file, a secrets file, and the database.")
		fmt.Fprintln(os.Stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	scaffold := config.Scaffold{
		Addr:        *addr,
		BaseURL:     *baseURL,
		DBPath:      *db,
		EmbedKind:   *embedKind,
		EmbedModel:  *embedModel,
		MCPServe:    *serveMCP,
		RetainAudio: *retainAudio,
	}
	if *agentKind != "" {
		scaffold.Agent = &config.ScaffoldAgent{
			Name:      *agentName,
			Kind:      *agentKind,
			Model:     *agentModel,
			BaseURL:   *agentBaseURL,
			APIKeyEnv: *agentKeyEnv,
			Prefix:    *agentPrefix,
		}
	}
	if *homeAssistant != "" {
		scaffold.HomeAssistantURL = *homeAssistant
	}

	rendered, err := scaffold.Render()
	if err != nil {
		return err
	}
	secrets, err := scaffold.Secrets()
	if err != nil {
		return err
	}

	if *printOnly {
		fmt.Println("# ---- configuration ----")
		fmt.Print(rendered)
		fmt.Println("\n# ---- secrets ----")
		fmt.Print(config.EnvFile(secrets))
		return nil
	}

	envPath := *envOut
	if envPath == "" {
		envPath = filepath.Join(filepath.Dir(*out), "riverbed.env")
	}

	// Both files are checked before either is written, so a refusal leaves
	// nothing half done.
	for _, path := range []string{*out, envPath} {
		if _, err := os.Stat(path); err == nil && !*force {
			return fmt.Errorf("%s already exists; pass -force to overwrite it", path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(envPath, []byte(config.EnvFile(secrets)), 0o600); err != nil {
		return err
	}
	if err := ignoreSecrets(envPath); err != nil {
		return err
	}

	fmt.Printf("Wrote %s\n", *out)
	fmt.Printf("Wrote %s, mode 0600. Keep it out of version control.\n", envPath)

	if *migrateDB {
		// The configuration is loaded with the secrets applied, which also
		// proves that what was written is valid.
		for name, value := range secrets {
			if err := os.Setenv(name, value); err != nil {
				return err
			}
		}
		c.configPath = *out
		cfg, logger, err := c.load()
		if err != nil {
			return fmt.Errorf("the generated configuration did not load: %w", err)
		}

		ctx, stop := signalContext()
		defer stop()

		app, err := riverbed.Open(ctx, riverbed.OpenOptions{
			Config: cfg, Version: version, Logger: logger,
		})
		if err != nil {
			return err
		}
		if err := app.Close(); err != nil {
			return err
		}
		fmt.Printf("Created %s\n", cfg.Store.Path)
	}

	printNextSteps(*out, envPath, scaffold, secrets)
	return nil
}

// ignoreSecrets adds the secrets file to .gitignore when the directory is a
// repository and the file is not already ignored. Writing a secret into a
// tracked file is the mistake this prevents.
func ignoreSecrets(envPath string) error {
	dir := filepath.Dir(envPath)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil
	}

	gitignore := filepath.Join(dir, ".gitignore")
	existing, err := os.ReadFile(gitignore)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	name := filepath.Base(envPath)
	for _, line := range strings.Split(string(existing), "\n") {
		switch strings.TrimSpace(line) {
		case name, "/" + name, "*.env":
			return nil
		}
	}

	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += name + "\n"
	if err := os.WriteFile(gitignore, []byte(body), 0o644); err != nil {
		return err
	}
	fmt.Printf("Added %s to .gitignore\n", name)
	return nil
}

// printNextSteps tells the operator what only a person can do.
func printNextSteps(configPath, envPath string, scaffold config.Scaffold, secrets map[string]string) {
	fmt.Println()
	fmt.Println("Next steps")
	fmt.Println()
	fmt.Println("1. Load the secrets and start the server:")
	fmt.Printf("     set -a; . %s; set +a\n", envPath)
	fmt.Printf("     riverbed serve -config %s\n", configPath)
	fmt.Println()
	fmt.Println("2. On the device, under the Index tab settings, set the webhook:")
	fmt.Println("     URL:     https://<your-host>/webhook/recording")
	fmt.Printf("     Header:  Authorization: Bearer %s\n", secrets[config.WebhookTokenEnv])
	fmt.Println("     Send:    transcription, or both audio and transcription")
	fmt.Println()

	step := 3
	if scaffold.MCPServe {
		fmt.Printf("%d. To let the device search its own notes, add an MCP server:\n", step)
		fmt.Println("     URL:     https://<your-host>/mcp")
		fmt.Println("     Type:    Streamable")
		fmt.Printf("     Auth:    Bearer %s\n", secrets[config.MCPTokenEnv])
		fmt.Println()
		step++
	}
	if scaffold.Agent != nil && scaffold.Agent.APIKeyEnv != "" {
		fmt.Printf("%d. Set the agent API key in the environment:\n", step)
		fmt.Printf("     export %s=...\n", scaffold.Agent.APIKeyEnv)
		fmt.Println()
		step++
	}
	if scaffold.HomeAssistantURL != "" {
		fmt.Printf("%d. Create a long-lived access token in Home Assistant and set:\n", step)
		fmt.Println("     export HOMEASSISTANT_TOKEN=...")
		fmt.Println()
		step++
	}
	fmt.Printf("%d. Check it without the device:\n", step)
	fmt.Printf("     curl -X POST http://localhost%s/webhook/recording \\\n", defaultPort(scaffold.Addr))
	fmt.Printf("       -H \"Authorization: Bearer $%s\" \\\n", config.WebhookTokenEnv)
	fmt.Println("       -F \"transcription=Note, this is a test\" \\")
	fmt.Printf("       -F \"recordedAt=$(date +%%s)000\" -F \"client=ring\"\n")
	fmt.Printf("     riverbed search -config %s test\n", configPath)
}

// defaultPort renders a listen address as something curl can use.
func defaultPort(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return addr
	}
	if _, port, ok := strings.Cut(addr, ":"); ok {
		return ":" + port
	}
	return ":8080"
}
