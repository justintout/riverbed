---
name: riverbed-routing
description: Add and tune Riverbed routing rules, including semantic rules that match a voice note by meaning with no model call. Use when the user wants to route notes to an agent, add a wake word or a spoken prefix, match notes by meaning or by example phrases, choose or fix a similarity threshold, or work out why a note went to the wrong route or was filed instead of acted on.
license: MIT
compatibility: Needs a built riverbed binary and a configuration file. Semantic rules need an embedding model configured.
---

# Riverbed routing rules

Riverbed decides what each voice note is for using only its text, because the
webhook carries no other signal. Your job is to express the user's intent as rules,
then measure them with `riverbed route` before reporting anything as working.

## How a decision is made

Riverbed tries three mechanisms in increasing order of cost, and uses the first that
decides.

1. **Prefix and regular expression rules**, in configuration order. Exact and free.
2. **Semantic rules**, which carry example utterances. The transcription is embedded
   once and compared to each example by cosine similarity. The highest scoring rule
   wins if it reaches its threshold. No model call.
3. **The classifier agent**, if configured, for anything still unmatched. This costs
   a model call for every such note.

If nothing decides, `router.default` applies. `journal` is a reserved route that
stores and embeds the note and calls no agent.

An exact match always beats a semantic match, even when the semantic rule also
clears its threshold. A wake word therefore stays reliable.

## Which kind of rule to use

| The user describes | Use |
| --- | --- |
| A wake word or a fixed phrase they say on purpose, such as "hey shelley" or "note" | `prefix`, with `strip = true` |
| A narrow grammatical pattern, such as commands starting with a verb | `regex` |
| A topic or intent they will phrase differently each time, such as controlling lights or asking about the calendar | `utterances` |
| Anything left over | the classifier, or `router.default` |

Prefer a semantic rule over the classifier when the set of intents is known. It is
faster, it costs nothing per note, and the decision is reproducible. Keep the
classifier for genuinely open-ended notes.

Only one matcher per rule. A rule with both a prefix and utterances is rejected at
startup.

## Writing a semantic rule

```toml
[router]
default = "journal"
semantic_threshold = 0.45

[[router.rule]]
utterances = [
  "turn on the kitchen lights",
  "dim the lamp in the bedroom",
  "switch off the porch light",
  "close the blinds",
]
agent = "home"
tags = ["home"]
threshold = 0.5      # optional, overrides semantic_threshold for this rule
```

Write utterances the way the user actually speaks to the device. Aim for four to
eight per rule, covering the vocabulary they use rather than restating one sentence.
Ask the user for real examples instead of inventing them; their phrasing is the
thing being matched.

Semantic rules need an embedding model. If `[embedding]` has no model, Riverbed
refuses to start and says so. Add a model first.

## Choosing the threshold

Do not guess a threshold, and do not copy one from another project. The usable range
depends on the embedding model. Measure it.

```sh
riverbed route -config riverbed.toml "kill the lights in the kitchen please"
```

This prints the decision and the score of every semantic rule, and stores nothing:

```
  route:  home
  reason: rule 1 semantic 0.717 "turn on the kitchen lights"
  semantic scores:
    * home         0.717  (threshold 0.45, closest "turn on the kitchen lights")
      assistant    0.033  (threshold 0.45, closest "what is on my calendar today")
```

Check a batch at once by passing `-`:

```sh
printf '%s\n' \
  "make the bedroom lamp brighter" \
  "do I have anything booked tomorrow" \
  "I keep thinking about the garden" \
  | riverbed route -config riverbed.toml -
```

The procedure:

1. Collect from the user several phrasings that **should** match each rule, and
   several that **should not**.
2. Run them all through `riverbed route`.
3. Set the threshold between the lowest score that should match and the highest that
   should not. Leave a margin; do not sit on the boundary.
4. If those two groups overlap, the utterances are the problem, not the threshold.
   Go to the next section.

Measured reference points, as a starting estimate only:

| Model | Should match | Should not match | Workable threshold |
| --- | --- | --- | --- |
| `potion-base-8M` | 0.58 to 0.79 | 0.03 to 0.20 | about 0.45 |
| contextual models such as `goformer` | higher | also much higher | higher, measure it |

Static embedding models such as `potion` score unrelated text near zero, which gives
wide separation. Contextual models score everything higher, so a threshold that
works for one will not transfer.

## When a note takes the wrong route

Run it through `riverbed route` first. The output says which mechanism decided and
what every semantic rule scored. Then:

**It went to the journal and should have matched a rule.** Look at the score. If it
is just under the threshold, the fix is almost always another utterance covering that
phrasing, not a lower threshold. A real example: `"when am I meeting the contractor"`
scored 0.407 against a calendar rule and fell through. Adding the utterance
`"when am I meeting someone"` took the same phrase to 0.639, without touching the
threshold. Lowering the threshold instead would have pulled in unrelated notes.

**It matched the wrong rule.** Two rules are too close in meaning. Either merge them
into one route, or make each rule's utterances more specific so the scores separate.
Check the scores of both rules to see the margin you are working with.

**An unrelated note matched a rule.** One utterance is too general. Find which one
from the `closest` field in the output and make it narrower, or remove it.

**A wake word was ignored.** Prefixes match after normalization, so case and
punctuation do not matter, but the words must be right and must come first. Confirm
with `riverbed route`, whose `reason` names the rule that matched.

## After any change

1. Confirm the configuration still loads: `riverbed migrate -config riverbed.toml`.
   It reports every problem at once.
2. Re-run the whole set of phrasings from step 1 of the calibration procedure, not
   only the one you were fixing. Narrowing an utterance to exclude one note often
   excludes others.
3. Restart `riverbed serve`. Utterances are embedded at startup, so a running server
   does not pick up new rules.

Recordings already stored keep the route they were given. Routing is not re-run over
history.

## Reporting back

Tell the user which rules you added, the threshold you chose and the measurements
that justify it, and the scores for each phrasing they gave you. If any phrasing
still routes somewhere they did not want, say so plainly rather than leaving them to
find it.
