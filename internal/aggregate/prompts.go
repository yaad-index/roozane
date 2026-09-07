package aggregate

import (
	"fmt"
	"strings"

	"github.com/yaad-index/roozane/internal/store"
)

// enrichSystemPrompt drives the neutral pass. It is given no profile and no
// reader, and that absence is the point: the result is cached per item and
// served to every edition, so anything audience-specific in it would be both
// wrong for somebody and a leak from one audience into another.
//
// The salience scale is defined here rather than left implicit, because a floor
// and a report are both stated in terms of it — a number on an undefined scale
// is not comparable to anything.
const enrichSystemPrompt = `You are the reading layer of a news engine. You are given one collected item and no reader.

Do three things:
1. Write a short factual summary of what the item says. Someone deciding whether they care will read this instead of the item, so it must carry the substance, not describe it.
2. Extract the load-bearing data points — the specific facts: numbers, names of organisations, dates, decisions, outcomes. Not atmosphere. If the item carries no concrete data points, return none.
3. Classify and score it.

Rules:
- YOU DO NOT KNOW WHO WILL READ THIS. Never judge whether an item is interesting, important, or worth someone's attention. Somebody else decides that later.
- Do not editorialise, praise, or predict significance.
- Preserve specifics exactly: numbers, names, dates. Do not round, soften or generalise them.

Respond with a single JSON object and nothing else, in this exact shape:
{"summary": "...", "points": ["...", "..."], "tags": ["...", "..."], "category": "...", "salience": 0.0-1.0}

- "summary" is a few sentences of substance, in plain prose.
- "points" holds the extracted data points, each a self-contained sentence. Empty when there are none.
- "tags" are short lower-case topic labels, a handful at most.
- "category" is one short lower-case word for the kind of thing this is, such as: announcement, analysis, release, incident, interview, opinion, listing.
- "salience" is HOW SUBSTANTIVE THE ITEM IS, on this scale, and nothing else:
    0.0 — no substance at all: navigation, boilerplate, an empty stub, a page that failed to load, pure duplication of a heading.
    0.3 — a passing mention: it names something happened but carries no detail a reader could act on or verify.
    0.6 — ordinary reporting: it carries specific, checkable facts.
    1.0 — dense with concrete, checkable detail: figures, named parties, dates, stated outcomes.
  It is NOT how interesting, important or relevant the item is. A thorough article about something nobody cares about scores high; a breathless one-line teaser about something crucial scores low.`

// selectSystemPrompt drives the per-edition pass. It keeps the property that
// used to live in the item prompt: suppression is the path of least resistance,
// stated as a rule, because a prompt that merely asks "is this relevant?"
// produces a bar that drifts downward — saying yes always looks more useful
// than saying no.
const selectSystemPrompt = `You are the selection layer of a news engine. You are given one already-summarised item and one reader's own relevance profile.

Decide whether this item belongs in that reader's digest.

Rules:
- SUPPRESSION IS THE DEFAULT. Select an item only if it clearly matches something the profile asks for. When in doubt, do not select it. A day where nothing is selected is a correct and expected outcome, not a failure to find something.
- Judge against the profile as written. Do not infer additional interests from what the item happens to be about.
- Do not re-rate the item's quality or importance. Match it against the profile, that is all.
- You are seeing a summary, not the original. Do not speculate about what the full item might also contain.

Respond with a single JSON object and nothing else, in this exact shape:
{"selected": true|false, "score": 0.0-1.0, "reason": "one sentence"}

- "score" is how strongly the item matches this profile, not how interesting it is.
- "reason" states briefly why it does or does not match the profile.`

// digestSystemPrompt drives the second pass. Length-follows-signal has to be
// stated as a rule with a floor of zero, because the default behaviour of a
// summariser is to produce a consistent amount of prose regardless of input.
const digestSystemPrompt = `You are the writing layer of a personal news engine. You are given the reader's relevance profile and the data points extracted from the items that matched it.

Write the day's digest in Markdown.

Rules:
- LENGTH FOLLOWS SIGNAL. Two small items get two lines. Do not pad to a familiar shape, do not add an introduction, a conclusion, or a "nothing else of note" line.
- Lead with the data points. The reader wants the facts, not a description of the news.
- Group related items under a short heading only when grouping genuinely helps. A flat list is fine and usually better.
- Preserve specifics exactly: numbers, names, dates. Do not round, soften, or generalise them.
- Do not invent anything that is not in the points you were given, and do not speculate about what an item implies.
- No preamble about what you are doing. Output the digest only.`

// buildEnrichMessages assembles the neutral pass's call for one item. It
// carries no profile: there is deliberately nothing here to tell the model who
// is going to read the result.
func buildEnrichMessages(item store.StoredItem) []llmMessages {
	var b strings.Builder

	b.WriteString("# Collected item\n\n")

	if item.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", item.Title)
	}
	if item.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", item.URL)
	}
	fmt.Fprintf(&b, "Source: %s\n\n", item.Source)
	b.WriteString(item.Content)

	return []llmMessages{
		{Role: roleSystem, Content: enrichSystemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}

// buildSelectMessages assembles one edition's call for one enriched item. It
// sends the summary and data points rather than the item, which is what makes
// selection cheaper than the judgement it replaces.
func buildSelectMessages(profile string, candidate enrichedItem) []llmMessages {
	var b strings.Builder

	b.WriteString("# Reader's relevance profile\n\n")
	b.WriteString(strings.TrimSpace(profile))
	b.WriteString("\n\n# Item\n\n")

	if candidate.Item.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", candidate.Item.Title)
	}
	fmt.Fprintf(&b, "Source: %s\n", candidate.Item.Source)
	if candidate.Enrichment.Category != "" {
		fmt.Fprintf(&b, "Category: %s\n", candidate.Enrichment.Category)
	}
	if len(candidate.Enrichment.Tags) > 0 {
		fmt.Fprintf(&b, "Tags: %s\n", strings.Join(candidate.Enrichment.Tags, ", "))
	}

	fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(candidate.Enrichment.Summary))

	if len(candidate.Enrichment.Points) > 0 {
		b.WriteString("\nData points:\n")
		for _, point := range candidate.Enrichment.Points {
			fmt.Fprintf(&b, "- %s\n", point)
		}
	}

	return []llmMessages{
		{Role: roleSystem, Content: selectSystemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}

// buildDigestMessages assembles the writing call from the items one edition
// selected, using that edition's own profile so the digest is written in its
// voice rather than a shared one.
func buildDigestMessages(profile string, selected []selectedItem) []llmMessages {
	var b strings.Builder

	b.WriteString("# Reader's relevance profile\n\n")
	b.WriteString(strings.TrimSpace(profile))
	b.WriteString("\n\n# Data points from today's matching items\n")

	for _, s := range selected {
		b.WriteString("\n## ")
		switch {
		case s.Item.Title != "":
			b.WriteString(s.Item.Title)
		case s.Item.URL != "":
			b.WriteString(s.Item.URL)
		default:
			b.WriteString(s.Item.Source)
		}
		b.WriteString("\n")

		if s.Item.URL != "" {
			fmt.Fprintf(&b, "Source: %s (%s)\n", s.Item.Source, s.Item.URL)
		} else {
			fmt.Fprintf(&b, "Source: %s\n", s.Item.Source)
		}
		for _, point := range s.Enrichment.Points {
			fmt.Fprintf(&b, "- %s\n", point)
		}
	}

	return []llmMessages{
		{Role: roleSystem, Content: digestSystemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}
