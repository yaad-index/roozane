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
//
// One-event-one-entry is stated here, and it replaced a rule that pointed the
// other way. The prompt used to say "a flat list is fine and usually better",
// and four articles about one municipal data breach duly became the digest's
// four leading rows. Scoring was working as specified — a significant event
// scores high, and every article about it scores high for the same reason, so
// the more important the story the more of the digest it took.
//
// ⚠️ Sameness is decided from the data points rather than the wording, because
// the lexical version of this test does not work: the two top headlines in that
// digest were one story with essentially disjoint vocabulary, and a
// title-similarity pass over them dropped none of the four. Only the substance
// separates a second report of one event from a report of a second event.
//
// The rule fails safe. A merge that does not happen leaves the previous
// behaviour, which is a digest that repeats itself; a merge that should not have
// happened loses a distinct story. So the instruction is asymmetric on purpose —
// merge only the genuinely same occurrence, and when in doubt keep them apart.
const digestSystemPrompt = `You are the writing layer of a personal news engine. You are given the reader's relevance profile and the data points extracted from the items that matched it.

Write the day's digest in Markdown.

Rules:
- LENGTH FOLLOWS SIGNAL. Two small items get two lines. Do not pad to a familiar shape, do not add an introduction, a conclusion, or a "nothing else of note" line.
- Lead with the data points. The reader wants the facts, not a description of the news.
- An item with no data points carries a SUMMARY line instead, written by the pass that read the item. Write that item's entry from it: it is given fact, and it is there so an item whose facts did not reduce to points is still more than its headline. An item that has points carries no summary line — there, the points are what you have.
- ONE EVENT IS ONE ENTRY. Several of the items below often report the same underlying event. Merge those into a single entry carrying the fullest set of facts between them, and name the sources that reported it — several sources agreeing is worth showing. Never give one event several entries: to the reader that is repetition rather than emphasis, and it crowds out everything else that happened.
- Decide sameness from what the data points describe, not from wording. Two reports of one event routinely share almost no vocabulary — a different headline, a different angle, sometimes a different language. If the facts point at the same occurrence, it is one event.
- Merge only what is genuinely the same occurrence. Items that are merely related stay separate, and when in doubt keep them separate. A short heading may group related entries where that helps the reader, but a heading is not a merge.
- Preserve specifics exactly: numbers, names, dates. Do not round, soften, or generalise them.
- Do not invent anything that is not in the points or the summary you were given, and do not speculate about what an item implies.
- No preamble about what you are doing. Output the digest only.`

// groupSystemPrompt drives the grouping pass, which runs once per edition over
// the items that edition selected (ADR-0007 §2).
//
// It is asked to describe and never to decide. The ceiling is arithmetic over
// what comes back, so a pass that also dropped items would make its own answer
// unauditable: a subject it merged and an item it left out arrive as the same
// thing, a shorter list, and nothing downstream — or in a test — could tell
// them apart.
//
// ⚠️ Every item must come back exactly once, and the reply is checked for that
// rather than trusted. An omitted index would delete an item from the digest
// with nothing recorded, which is precisely the failure this pass exists to
// prevent; parseGrouping rejects the reply instead, and a rejected grouping
// leaves the digest unbalanced and says so.
//
// The instruction to group by substance rather than by publisher or by kind is
// the one the whole decision turns on. Enrichment already carries a category
// and tags, and neither is a subject: the category names the KIND of item, and
// tags are free-form and multi-valued. A ceiling counted over either would be
// permanently satisfied and never binding.
//
// Granularity is stated in terms of a reader's standing interests, because the
// two obvious readings are both wrong in the same direction. One subject per
// story makes every item its own subject and the ceiling never binds; one
// subject per broad domain collapses unrelated things and it binds far too
// hard. What the reader perceives is the level in between.
const groupSystemPrompt = `You are the grouping layer of a news engine. You are given the items one reader's digest has selected today, numbered.

Sort them into subjects: what each item is ABOUT.

Rules:
- EVERY item appears in exactly one subject. Never leave an item out, and never put one in two subjects. If an item shares its subject with nothing else, it is a subject of one — that is a correct answer, not a failure to place it.
- Group by substance, not by publisher and not by the kind of item. Two publishers covering one field are one subject. An announcement and an analysis about the same field are one subject.
- A subject is the size of a standing interest someone would name — the field, the pursuit, the area — not the size of a single story and not the size of a whole domain. Several developments in one field are one subject; two unrelated fields are never one subject because both happen to be technical.
- DESCRIBE, do not judge. Do not rank the items, do not say which matter, and do not leave anything out for being minor. Something else decides what the digest keeps.

Respond with a single JSON object and nothing else, in this exact shape:
{"subjects": [{"subject": "...", "items": [0, 2, 5]}]}

- "subject" is a short lower-case label for what those items are about.
- "items" are the numbers of the items belonging to it, taken from the list you were given.`

// buildGroupMessages assembles one edition's grouping call.
//
// It sends the data points rather than the titles alone, and it carries no
// profile. Both follow the pass's job: the subject of an item is a property of
// the item and not of the reader, and one story's headlines routinely share
// almost no vocabulary — the writing pass learned that when a lexical
// similarity test over four articles about one event dropped none of them.
//
// The numbers are positions in the selected slice, so the reply can be checked
// against what was asked. A reply naming filenames would have to be matched by
// string, where a near-miss reads as an item the grouping simply omitted.
func buildGroupMessages(selected []selectedItem) []llmMessages {
	var b strings.Builder

	b.WriteString("# Items in today's digest\n")

	for i, s := range selected {
		fmt.Fprintf(&b, "\n## %d. ", i)
		switch {
		case s.Item.Title != "":
			b.WriteString(s.Item.Title)
		case s.Item.URL != "":
			b.WriteString(s.Item.URL)
		default:
			b.WriteString(s.Item.Source)
		}
		b.WriteString("\n")

		if summary := strings.TrimSpace(s.Enrichment.Summary); summary != "" {
			fmt.Fprintf(&b, "%s\n", summary)
		}
		for _, point := range s.Enrichment.Points {
			fmt.Fprintf(&b, "- %s\n", point)
		}
	}

	return []llmMessages{
		{Role: roleSystem, Content: groupSystemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}

// titleSystemPrompt drives the title pass, which runs once per edition over the
// titles that edition selected.
//
// It asks for omissions rather than echoes: a title already in the reader's
// language is left out of the reply entirely. That is what makes the common case
// free — no output tokens are spent restating a title that needed nothing — and
// it also means "unchanged" and "translated to something identical" arrive as
// the same answer, which they are.
//
// Translating is stated as the ONLY job, because a model given a headline and no
// constraint improves it: it expands abbreviations, adds the context the article
// supplies, and returns something more useful than a title and no longer the
// same one. A title that no longer matches the page it links to is the failure
// this pass has to avoid, not the one it is fixing.
const titleSystemPrompt = `You are the title pass of a news engine. You are given a reader's language and a numbered list of headlines as their publishers wrote them.

Return each headline that is NOT already in the reader's language, rendered in the reader's language.

Rules:
- TRANSLATE ONLY. Do not improve, expand, shorten, explain or re-punctuate a headline. It has to stay the same headline.
- Omit a headline that is already in the reader's language. Saying nothing about it is the answer.
- Keep names, numbers, dates and organisations exactly as they appear. Names of people, places, products and companies are not translated.
- Do not add anything a headline does not say, and do not resolve an abbreviation the headline leaves short.

Respond with a single JSON object and nothing else, in this exact shape:
{"titles": [{"index": 0, "title": "..."}]}

- "index" is the number the headline was given in the list.
- "title" is that headline in the reader's language.
- An empty list is a correct answer: it says every headline was already in the reader's language.`

// numberedTitle is one headline and the position of the item it belongs to.
// The number is the item's own index and not a position in this list, so items
// without a headline can be left out of the request without the remaining
// numbers shifting under the reply.
type numberedTitle struct {
	index int
	title string
}

// buildTitleMessages assembles one edition's title pass. Titles are numbered
// rather than sent bare, because the reply has to be attached back to the items
// it came from and a positional list gives a misaligned reply no way to announce
// itself — an index the caller can check against what it asked does.
func buildTitleMessages(language string, titles []numberedTitle) []llmMessages {
	var b strings.Builder

	fmt.Fprintf(&b, "# Reader's language\n\n%s\n\n# Headlines\n\n", language)
	for _, t := range titles {
		fmt.Fprintf(&b, "%d. %s\n", t.index, t.title)
	}

	return []llmMessages{
		{Role: roleSystem, Content: titleSystemPrompt},
		{Role: roleUser, Content: b.String()},
	}
}

// digestLanguageRule is appended to the writing pass's instructions when the
// edition names a language. It is a separate string rather than an edit to
// digestSystemPrompt so that an edition with no language configured sends
// byte-identical instructions to the ones sent before this pass existed.
func digestLanguageRule(language string) string {
	return fmt.Sprintf(`
- WRITE IN %s. Everything you produce is for a reader of that language: headings, prose, and the entries themselves.
- Some items below carry both a headline in %s and the publisher's original headline. Lead with the first and keep the original alongside it, because the original is what the reader sees on the page when they follow the link. Never replace the original, and never drop it.
- An item with only one headline has one because it was already in %s. Use it as it stands.`, language, language, language)
}

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
//
// The language rule is appended to the system prompt rather than folded into it,
// so an edition that names no language sends exactly what it sent before.
//
// The enriched summary is sent only for an item with no data points. The select
// and grouping passes send it unconditionally; this pass does not, because its
// instructions are tuned to lead with the points and prose competing with them
// on every item is a regression no test here can see — nothing exercises a real
// model. Restricting it to the items that have no points keeps the message for
// every other item byte-identical, which is a guarantee rather than a judgement.
func buildDigestMessages(profile, language string, selected []selectedItem) []llmMessages {
	var b strings.Builder

	b.WriteString("# Reader's relevance profile\n\n")
	b.WriteString(strings.TrimSpace(profile))
	b.WriteString("\n\n# Data points from today's matching items\n")

	for _, s := range selected {
		b.WriteString("\n## ")
		switch {
		case s.TitleTranslated != "":
			b.WriteString(s.TitleTranslated)
		case s.Item.Title != "":
			b.WriteString(s.Item.Title)
		case s.Item.URL != "":
			b.WriteString(s.Item.URL)
		default:
			b.WriteString(s.Item.Source)
		}
		b.WriteString("\n")

		// Only when it differs, so the writer is never handed the same headline
		// twice and asked to treat one of them as an original.
		if s.TitleTranslated != "" && s.Item.Title != "" {
			fmt.Fprintf(&b, "Original headline: %s\n", s.Item.Title)
		}

		if s.Item.URL != "" {
			fmt.Fprintf(&b, "Source: %s (%s)\n", s.Item.Source, s.Item.URL)
		} else {
			fmt.Fprintf(&b, "Source: %s\n", s.Item.Source)
		}

		// Only where there are no points. An item that has points sends the same
		// bytes it always has, so the tuned rules above cannot regress on it, and
		// the change reaches exactly the items that could otherwise only come out
		// as a headline. The cliff that creates is deliberate: an item with one
		// thin point gets no summary, which leaves it exactly as it is rather than
		// making it worse.
		if len(s.Enrichment.Points) == 0 {
			if summary := strings.TrimSpace(s.Enrichment.Summary); summary != "" {
				fmt.Fprintf(&b, "Summary: %s\n", summary)
			}
		}
		for _, point := range s.Enrichment.Points {
			fmt.Fprintf(&b, "- %s\n", point)
		}
	}

	system := digestSystemPrompt
	if language != "" {
		system += digestLanguageRule(language)
	}

	return []llmMessages{
		{Role: roleSystem, Content: system},
		{Role: roleUser, Content: b.String()},
	}
}
