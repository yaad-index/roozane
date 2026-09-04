package aggregate

import (
	"fmt"
	"strings"

	"github.com/yaad-index/roozane/internal/store"
)

// itemSystemPrompt drives the first pass. It is written to make suppression the
// path of least resistance: the model is told the default answer is "not
// relevant", and told plainly that an empty day is a correct outcome. A prompt
// that merely asks "is this relevant?" produces a bar that drifts downward,
// because saying yes always looks more useful than saying no.
const itemSystemPrompt = `You are the reading layer of a personal news engine. You are given one collected item and the reader's own relevance profile.

Do two things:
1. Extract the load-bearing data points — the specific facts a reader would want: numbers, names of organisations, dates, decisions, outcomes. Not a summary, not atmosphere. If the item carries no concrete data points, say so by returning none.
2. Judge the item against the profile.

Rules:
- SUPPRESSION IS THE DEFAULT. Mark an item relevant only if it clearly matches something the profile asks for. When in doubt, it is not relevant. A day where nothing is relevant is a correct and expected outcome, not a failure to find something.
- Judge against the profile as written. Do not infer additional interests from what the item happens to be about.
- Do not editorialise, rate quality, or predict importance. Match against the profile, that is all.

Respond with a single JSON object and nothing else, in this exact shape:
{"relevant": true|false, "score": 0.0-1.0, "points": ["...", "..."], "reason": "one sentence"}

- "score" is how strongly the item matches the profile, not how interesting it is.
- "points" holds the extracted data points, each a self-contained sentence. Empty when there are none.
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

// buildItemMessages assembles the first-pass call for one item.
func buildItemMessages(profile string, item store.StoredItem) []llmMessages {
	var b strings.Builder

	b.WriteString("# Reader's relevance profile\n\n")
	b.WriteString(strings.TrimSpace(profile))
	b.WriteString("\n\n# Collected item\n\n")

	if item.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", item.Title)
	}
	if item.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", item.URL)
	}
	fmt.Fprintf(&b, "Source: %s\n\n", item.Source)
	b.WriteString(item.Content)

	return []llmMessages{
		{Role: roleSystem, Content: itemSystemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}

// buildDigestMessages assembles the second-pass call from the judged items that
// cleared the bar.
func buildDigestMessages(profile string, judged []judgedItem) []llmMessages {
	var b strings.Builder

	b.WriteString("# Reader's relevance profile\n\n")
	b.WriteString(strings.TrimSpace(profile))
	b.WriteString("\n\n# Data points from today's matching items\n")

	for _, j := range judged {
		b.WriteString("\n## ")
		switch {
		case j.Item.Title != "":
			b.WriteString(j.Item.Title)
		case j.Item.URL != "":
			b.WriteString(j.Item.URL)
		default:
			b.WriteString(j.Item.Source)
		}
		b.WriteString("\n")

		if j.Item.URL != "" {
			fmt.Fprintf(&b, "Source: %s (%s)\n", j.Item.Source, j.Item.URL)
		} else {
			fmt.Fprintf(&b, "Source: %s\n", j.Item.Source)
		}
		for _, point := range j.Judgement.Points {
			fmt.Fprintf(&b, "- %s\n", point)
		}
	}

	return []llmMessages{
		{Role: roleSystem, Content: digestSystemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}
