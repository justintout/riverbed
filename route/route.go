// Package route decides what should happen to a transcription.
//
// Deciding is needed because the webhook carries only the transcription, a
// timestamp and the client name. The text itself therefore has to say whether it
// is a thought to file or a request to act on.
//
// Three mechanisms are offered, and all are optional. They are tried in
// increasing order of cost.
//
// Prefix and regular expression rules are exact and free, so they are tried
// first, in configuration order.
//
// Semantic rules carry example utterances. The transcription is embedded once
// and compared to every example by cosine similarity, and the highest scoring
// rule wins if it reaches its threshold. This needs no model call, so it costs
// well under a millisecond with an in-process embedder.
//
// A classifier agent, typically a small local model, resolves what neither
// matched. When nothing decides, the default route applies.
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
	"github.com/justintout/riverbed/embedding"
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
	// Score is the similarity of a semantic match, and zero otherwise.
	Score float64
	// Utterance is the example a semantic match scored against.
	Utterance string
}

// Journaled reports whether the decision is to store without calling an agent.
func (d Decision) Journaled() bool { return d.Agent == config.JournalAgent }

// Router applies rules and, when configured, a classifier.
type Router struct {
	rules      []compiledRule
	defaultTo  string
	classifier agent.Agent
	// targets are the agent names the classifier may choose from.
	targets  []string
	embedder embedding.Embedder
	log      *slog.Logger
}

type compiledRule struct {
	cfg   config.Rule
	index int
	// prefix is the normalized spoken prefix.
	prefix string
	regex  *regexp.Regexp
	// utterances are the rule's examples with their embeddings.
	utterances []utterance
	threshold  float64
}

// Options configures a Router.
type Options struct {
	Config config.Router
	// Classifier is the agent named by Config.Classifier, or nil.
	Classifier agent.Agent
	// Targets are the routable agent names offered to the classifier.
	Targets []string
	// Embedder is required when any rule carries utterances.
	Embedder embedding.Embedder
	Logger   *slog.Logger
}

// New compiles the router and embeds the utterances of every semantic rule, so
// that routing a recording embeds only the transcription.
func New(ctx context.Context, opts Options) (*Router, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	r := &Router{
		defaultTo:  opts.Config.Default,
		classifier: opts.Classifier,
		targets:    opts.Targets,
		embedder:   opts.Embedder,
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
		case len(rule.Utterances) > 0:
			compiled.threshold = opts.Config.Threshold(rule)
		default:
			return nil, fmt.Errorf("route: rule %d has no prefix, regex or utterances", i)
		}
		r.rules = append(r.rules, compiled)
	}

	if err := r.embedUtterances(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Route decides what to do with a transcription.
func (r *Router) Route(ctx context.Context, transcription string) Decision {
	text := strings.TrimSpace(transcription)
	normalized := Normalize(text)

	// Exact matchers first: they cost nothing and leave no doubt.
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

	// Then meaning, which needs one embedding and no model call.
	if matches, err := r.semanticScores(ctx, text); err != nil {
		// A failed embedding must not lose the recording, so routing carries on
		// to the classifier and the default.
		r.log.Error("semantic routing failed", "error", err)
	} else if best, ok := bestSemantic(matches); ok {
		return Decision{
			Agent:     best.Agent,
			Prompt:    text,
			Reason:    fmt.Sprintf("rule %d semantic %.3f %q", best.Index, best.Score, best.Utterance),
			Tags:      r.rules[r.ruleAt(best.Index)].cfg.Tags,
			Score:     best.Score,
			Utterance: best.Utterance,
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

// ruleAt returns the position of the rule with the given configuration index.
func (r *Router) ruleAt(index int) int {
	for i, rule := range r.rules {
		if rule.index == index {
			return i
		}
	}
	return 0
}

// Explain reports the decision for a transcription along with the score of every
// semantic rule, so thresholds can be chosen from measurements.
func (r *Router) Explain(ctx context.Context, transcription string) (Decision, []semanticMatch, error) {
	matches, err := r.semanticScores(ctx, strings.TrimSpace(transcription))
	if err != nil {
		return Decision{}, nil, err
	}
	return r.Route(ctx, transcription), matches, nil
}

// Scores describes one semantic rule's score against a transcription.
type Scores = semanticMatch

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
