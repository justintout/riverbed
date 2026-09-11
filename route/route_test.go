package route

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/justintout/riverbed/agent"
	"github.com/justintout/riverbed/config"
)

// fakeAgent answers with a fixed reply, or fails.
type fakeAgent struct {
	name    string
	reply   string
	err     error
	prompts []string
}

func (f *fakeAgent) Name() string { return f.name }

func (f *fakeAgent) Run(_ context.Context, req agent.Request) (*agent.Response, error) {
	f.prompts = append(f.prompts, req.Prompt)
	if f.err != nil {
		return nil, f.err
	}
	return &agent.Response{Text: f.reply}, nil
}

func router(t *testing.T, cfg config.Router, classifier agent.Agent, targets []string) *Router {
	t.Helper()
	r, err := New(t.Context(), Options{Config: cfg, Classifier: classifier, Targets: targets})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNormalize(t *testing.T) {
	for input, want := range map[string]string{
		"Hey Shelley, turn on the lights.": "hey shelley turn on the lights",
		"  NOTE:  buy milk  ":              "note buy milk",
		"What's the time?":                 "what's the time",
		"...":                              "",
		"Turn on light #3":                 "turn on light 3",
	} {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPrefixRuleStrips(t *testing.T) {
	r := router(t, config.Router{
		Default: config.JournalAgent,
		Rules: []config.Rule{
			{Prefix: "hey shelley", Agent: "shelley", Strip: true, Tags: []string{"shelley"}},
		},
	}, nil, nil)

	d := r.Route(t.Context(), "Hey Shelley, what's on my calendar?")
	if d.Agent != "shelley" {
		t.Errorf("agent = %q", d.Agent)
	}
	if d.Prompt != "what's on my calendar?" {
		t.Errorf("prompt = %q, want the prefix removed with casing kept", d.Prompt)
	}
	if len(d.Tags) != 1 || d.Tags[0] != "shelley" {
		t.Errorf("tags = %v", d.Tags)
	}
	if !strings.Contains(d.Reason, "prefix") {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestPrefixRuleKeepsTextWhenNotStripping(t *testing.T) {
	r := router(t, config.Router{
		Default: config.JournalAgent,
		Rules:   []config.Rule{{Prefix: "note", Agent: config.JournalAgent}},
	}, nil, nil)

	d := r.Route(t.Context(), "Note: buy milk")
	if d.Prompt != "Note: buy milk" {
		t.Errorf("prompt = %q, want it unchanged", d.Prompt)
	}
	if !d.Journaled() {
		t.Error("the journal route should report Journaled")
	}
}

func TestPrefixMatchesOnWordBoundary(t *testing.T) {
	r := router(t, config.Router{
		Default: config.JournalAgent,
		Rules:   []config.Rule{{Prefix: "note", Agent: "notes", Strip: true}},
	}, nil, nil)

	if d := r.Route(t.Context(), "Nothing much happened today"); d.Agent != config.JournalAgent {
		t.Errorf("%q should not match the prefix %q", "Nothing", "note")
	}
	if d := r.Route(t.Context(), "Note buy milk"); d.Agent != "notes" {
		t.Error("a real prefix should match")
	}
}

func TestBarePrefixKeepsTheText(t *testing.T) {
	r := router(t, config.Router{
		Default: config.JournalAgent,
		Rules:   []config.Rule{{Prefix: "hey shelley", Agent: "shelley", Strip: true}},
	}, nil, nil)

	d := r.Route(t.Context(), "Hey Shelley")
	if d.Agent != "shelley" {
		t.Errorf("agent = %q", d.Agent)
	}
	if d.Prompt != "Hey Shelley" {
		t.Errorf("prompt = %q: stripping everything would leave nothing to answer", d.Prompt)
	}
}

func TestRulesAreOrdered(t *testing.T) {
	r := router(t, config.Router{
		Default: config.JournalAgent,
		Rules: []config.Rule{
			{Prefix: "hey shelley note", Agent: "notes", Strip: true},
			{Prefix: "hey shelley", Agent: "shelley", Strip: true},
		},
	}, nil, nil)

	if d := r.Route(t.Context(), "Hey Shelley note the milk"); d.Agent != "notes" {
		t.Errorf("the first matching rule should win, got %q", d.Agent)
	}
	if d := r.Route(t.Context(), "Hey Shelley turn on the lights"); d.Agent != "shelley" {
		t.Errorf("agent = %q", d.Agent)
	}
}

func TestRegexRule(t *testing.T) {
	r := router(t, config.Router{
		Default: config.JournalAgent,
		Rules:   []config.Rule{{Regex: `^(turn|set|dim) `, Agent: "home", Tags: []string{"home"}}},
	}, nil, nil)

	d := r.Route(t.Context(), "Turn on the kitchen lights.")
	if d.Agent != "home" {
		t.Errorf("agent = %q", d.Agent)
	}
	if d.Prompt != "Turn on the kitchen lights." {
		t.Errorf("prompt = %q, want it unchanged", d.Prompt)
	}
	if !strings.Contains(d.Reason, "regex") {
		t.Errorf("reason = %q", d.Reason)
	}
	if d := r.Route(t.Context(), "I should turn in early"); d.Agent != config.JournalAgent {
		t.Error("the anchored regex should not match mid sentence")
	}
}

func TestDefaultWhenNothingMatches(t *testing.T) {
	r := router(t, config.Router{Default: config.JournalAgent}, nil, nil)
	d := r.Route(t.Context(), "a loose thought")
	if d.Agent != config.JournalAgent || d.Reason != "default" {
		t.Errorf("decision = %+v", d)
	}
}

func TestDefaultIsJournalWhenUnset(t *testing.T) {
	r := router(t, config.Router{}, nil, nil)
	if d := r.Route(t.Context(), "x"); d.Agent != config.JournalAgent {
		t.Errorf("agent = %q, want the journal", d.Agent)
	}
}

func TestClassifierDecidesUnmatchedText(t *testing.T) {
	classifier := &fakeAgent{name: "local", reply: "shelley"}
	r := router(t, config.Router{Default: config.JournalAgent},
		classifier, []string{config.JournalAgent, "shelley"})

	d := r.Route(t.Context(), "what is the weather like")
	if d.Agent != "shelley" {
		t.Errorf("agent = %q", d.Agent)
	}
	if !strings.Contains(d.Reason, "classifier") {
		t.Errorf("reason = %q", d.Reason)
	}
	if len(classifier.prompts) != 1 {
		t.Fatal("the classifier should have been asked once")
	}
	prompt := classifier.prompts[0]
	for _, want := range []string{"journal", "shelley", "what is the weather like"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the classifier prompt should contain %q:\n%s", want, prompt)
		}
	}
}

func TestRulesSkipTheClassifier(t *testing.T) {
	classifier := &fakeAgent{name: "local", reply: "shelley"}
	r := router(t, config.Router{
		Default: config.JournalAgent,
		Rules:   []config.Rule{{Prefix: "note", Agent: config.JournalAgent, Strip: true}},
	}, classifier, []string{config.JournalAgent, "shelley"})

	d := r.Route(t.Context(), "note the milk")
	if d.Agent != config.JournalAgent {
		t.Errorf("agent = %q", d.Agent)
	}
	if len(classifier.prompts) != 0 {
		t.Error("a matched rule should cost no model call")
	}
}

func TestClassifierAnswerShapes(t *testing.T) {
	for name, reply := range map[string]string{
		"bare":        "shelley",
		"padded":      "  shelley\n",
		"sentence":    "I would choose shelley for this.",
		"quoted":      `"shelley"`,
		"json":        `{"handler":"shelley"}`,
		"json agent":  `{"agent":"shelley"}`,
		"capitalized": "Shelley",
	} {
		t.Run(name, func(t *testing.T) {
			r := router(t, config.Router{Default: config.JournalAgent},
				&fakeAgent{name: "local", reply: reply}, []string{config.JournalAgent, "shelley"})
			if d := r.Route(t.Context(), "ambiguous"); d.Agent != "shelley" {
				t.Errorf("reply %q gave agent %q", reply, d.Agent)
			}
		})
	}
}

func TestClassifierFailureFallsBackToDefault(t *testing.T) {
	r := router(t, config.Router{Default: config.JournalAgent},
		&fakeAgent{name: "local", err: errors.New("model is down")},
		[]string{config.JournalAgent, "shelley"})

	d := r.Route(t.Context(), "ambiguous")
	if d.Agent != config.JournalAgent || d.Reason != "default" {
		t.Errorf("decision = %+v: a classifier failure must not lose the recording", d)
	}
}

func TestUnusableClassifierAnswerFallsBackToDefault(t *testing.T) {
	r := router(t, config.Router{Default: config.JournalAgent},
		&fakeAgent{name: "local", reply: "I have no idea what you mean"},
		[]string{config.JournalAgent, "shelley"})

	if d := r.Route(t.Context(), "ambiguous"); d.Agent != config.JournalAgent {
		t.Errorf("agent = %q, want the default", d.Agent)
	}
}

func TestNewRejectsBadRules(t *testing.T) {
	if _, err := New(t.Context(), Options{Config: config.Router{Rules: []config.Rule{{Regex: "(["}}}}); err == nil {
		t.Error("want an error for a bad regex")
	}
	if _, err := New(t.Context(), Options{Config: config.Router{Rules: []config.Rule{{}}}}); err == nil {
		t.Error("want an error for a rule with no matcher")
	}
	if _, err := New(t.Context(), Options{Config: config.Router{Rules: []config.Rule{{Prefix: "..."}}}}); err == nil {
		t.Error("want an error for a prefix of only punctuation")
	}
}

func TestStripPrefix(t *testing.T) {
	for _, tc := range []struct{ text, prefix, want string }{
		{"Hey Shelley, turn on the lights", "hey shelley", "turn on the lights"},
		{"Note: buy milk", "note", "buy milk"},
		{"note buy milk", "note", "buy milk"},
		{"Hey Shelley", "hey shelley", ""},
	} {
		if got := stripPrefix(tc.text, tc.prefix); got != tc.want {
			t.Errorf("stripPrefix(%q, %q) = %q, want %q", tc.text, tc.prefix, got, tc.want)
		}
	}
}
