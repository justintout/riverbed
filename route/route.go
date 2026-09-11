// Package route decides what should happen to a transcription.
//
// Deciding is needed because the device's webhook does not say which button
// combination produced a recording: the payload carries the transcription, a
// timestamp and the client name, nothing more. So the text itself has to say
// whether it is a thought to file or a request to act on.
//
// Two mechanisms are offered, and both are optional. Ordered rules match a
// spoken prefix or a regular expression, which is deterministic and costs
// nothing. A classifier agent, typically a small local model, resolves whatever
// the rules do not match. When neither decides, the default route applies.
package route

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"unicode"

	"github.com/justintout/riverbed/agent"
	"github.com/justintout/riverbed/config"
)

// Decision is the outcome of routing.
type Decision struct {
	// Agent is the agent to run, or config.JournalAgent to store only.
	Agent string
	// Prompt is the text the agent should answer, with any matched prefix
	// removed.
	Prompt string
	// Reason says what decided the route, for the record and for debugging.
	Reason string
	// Tags are attached to the recording.
	Tags []string
}

// Journaled reports whether the decision is to store without calling an agent.
func (d Decision) Journaled() bool { return d.Agent == config.JournalAgent }

// Router applies rules and, when configured, a classifier.
type Router struct {
	rules      []compiledRule
	defaultTo  string
	classifier agent.Agent
	// targets are the agent names the classifier may choose from.
	targets []string
	log     *slog.Logger
}

type compiledRule struct {
	cfg   config.Rule
	index int
	// prefix is the normalized spoken prefix.
	prefix string
	regex  *regexp.Regexp
}

// Options configures a Router.
type Options struct {
	Config config.Router
	// Classifier is the agent named by Config.Classifier, or nil.
	Classifier agent.Agent
	// Targets are the routable agent names offered to the classifier.
	Targets []string
	Logger  *slog.Logger
}

// New compiles the router.
func New(opts Options) (*Router, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	r := &Router{
		defaultTo:  opts.Config.Default,
		classifier: opts.Classifier,
		targets:    opts.Targets,
		log:        opts.Logger,
	}
	if r.defaultTo == "" {
		r.defaultTo = config.JournalAgent
	}

	for i, rule := range opts.Config.Rules {
		compiled := compiledRule{cfg: rule, index: i}
		switch {
		case rule.Prefix != "":
			compiled.prefix = Normalize(rule.Prefix)
			if compiled.prefix == "" {
				return nil, fmt.Errorf("route: rule %d has a prefix of only punctuation", i)
			}
		case rule.Regex != "":
			re, err := regexp.Compile(rule.Regex)
			if err != nil {
				return nil, fmt.Errorf("route: rule %d regex: %w", i, err)
			}
			compiled.regex = re
		default:
			return nil, fmt.Errorf("route: rule %d has neither a prefix nor a regex", i)
		}
		r.rules = append(r.rules, compiled)
	}
	return r, nil
}

// Route decides what to do with a transcription.
func (r *Router) Route(ctx context.Context, transcription string) Decision {
	text := strings.TrimSpace(transcription)
	normalized := Normalize(text)

	for _, rule := range r.rules {
		if rule.prefix != "" {
			if !matchPrefix(normalized, rule.prefix) {
				continue
			}
			prompt := text
			if rule.cfg.Strip {
				prompt = stripPrefix(text, rule.prefix)
				// A bare wake word with nothing after it carries no request,
				// so keep the original text rather than an empty prompt.
				if strings.TrimSpace(prompt) == "" {
					prompt = text
				}
			}
			return Decision{
				Agent:  rule.cfg.Agent,
				Prompt: prompt,
				Reason: fmt.Sprintf("rule %d prefix %q", rule.index, rule.cfg.Prefix),
				Tags:   rule.cfg.Tags,
			}
		}
		if rule.regex != nil && rule.regex.MatchString(normalized) {
			return Decision{
				Agent:  rule.cfg.Agent,
				Prompt: text,
				Reason: fmt.Sprintf("rule %d regex %q", rule.index, rule.cfg.Regex),
				Tags:   rule.cfg.Tags,
			}
		}
	}

	if r.classifier != nil && len(r.targets) > 0 {
		if chosen, ok := r.classify(ctx, text); ok {
			return Decision{
				Agent:  chosen,
				Prompt: text,
				Reason: "classifier " + r.classifier.Name(),
			}
		}
	}

	return Decision{Agent: r.defaultTo, Prompt: text, Reason: "default"}
}

// classify asks the classifier agent to pick a route. A failure is logged and
// ignored: routing then falls through to the default, which is always safe
// because the default stores rather than acts.
func (r *Router) classify(ctx context.Context, text string) (string, bool) {
	res, err := r.classifier.Run(ctx, agent.Request{Prompt: r.classifyPrompt(text)})
	if err != nil {
		r.log.Warn("classifier failed", "classifier", r.classifier.Name(), "error", err)
		return "", false
	}

	chosen, ok := r.parseChoice(res.Text)
	if !ok {
		r.log.Warn("classifier gave an unusable answer",
			"classifier", r.classifier.Name(), "answer", res.Text)
		return "", false
	}
	return chosen, true
}

// classifyPrompt asks for one name and nothing else. Small local models follow
// a closed list far better than an open instruction.
func (r *Router) classifyPrompt(text string) string {
	var b strings.Builder
	b.WriteString("Choose which handler should receive a voice note.\n\nHandlers:\n")
	for _, name := range r.targets {
		if name == config.JournalAgent {
			b.WriteString("- " + name + ": the note is a thought, reminder or observation to file away\n")
			continue
		}
		b.WriteString("- " + name + ": the note asks for an action or an answer\n")
	}
	b.WriteString("\nNote:\n")
	b.WriteString(text)
	b.WriteString("\n\nAnswer with one handler name and nothing else.")
	return b.String()
}

// parseChoice reads a handler name out of the classifier's answer, tolerating
// surrounding prose, quotes, or a JSON wrapper.
func (r *Router) parseChoice(answer string) (string, bool) {
	candidate := strings.TrimSpace(answer)

	// A model told to answer with a name sometimes answers with JSON anyway.
	if strings.HasPrefix(candidate, "{") {
		var fields map[string]string
		if err := json.Unmarshal([]byte(candidate), &fields); err == nil {
			for _, key := range []string{"handler", "agent", "route", "choice", "answer"} {
				if v, ok := fields[key]; ok {
					candidate = v
					break
				}
			}
		}
	}

	cleaned := Normalize(candidate)
	for _, name := range r.targets {
		if cleaned == Normalize(name) {
			return name, true
		}
	}
	// Fall back to a contained name, which covers "I would choose journal."
	for _, name := range r.targets {
		if containsWord(cleaned, Normalize(name)) {
			return name, true
		}
	}
	return "", false
}

// Normalize lowercases text and reduces it to words separated by single spaces,
// so a spoken prefix matches however the transcription punctuated it.
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		case r == '\'':
			// Keep apostrophes inside words: "what's" stays one word.
			if !lastSpace {
				b.WriteRune(r)
			}
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// matchPrefix reports whether normalized text begins with the prefix on a word
// boundary, so "note" does not match "nothing".
func matchPrefix(normalized, prefix string) bool {
	return normalized == prefix || strings.HasPrefix(normalized, prefix+" ")
}

// stripPrefix removes a normalized prefix from the original text, keeping the
// original's casing and punctuation in what remains.
func stripPrefix(text, prefix string) string {
	words := len(strings.Fields(prefix))
	// Walk the original text, dropping the same number of words the prefix has.
	remaining := text
	for i := 0; i < words; i++ {
		remaining = strings.TrimLeftFunc(remaining, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		cut := strings.IndexFunc(remaining, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
		})
		if cut < 0 {
			return ""
		}
		remaining = remaining[cut:]
	}
	return strings.TrimLeftFunc(remaining, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ':' || r == '.' || r == '-'
	})
}

// containsWord reports whether haystack contains needle as whole words.
func containsWord(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for _, field := range strings.Fields(haystack) {
		if field == needle {
			return true
		}
	}
	return strings.Contains(haystack, " "+needle+" ") ||
		strings.HasPrefix(haystack, needle+" ") ||
		strings.HasSuffix(haystack, " "+needle)
}
