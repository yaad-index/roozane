# ADR-0008: A digest is filled to a length subject by subject, and the spoken reading becomes a sink rather than a bypass

**Status:** Proposed

## Context

The engine writes a digest to `digests/<edition>/<day>.{md,json}` and its sinks deliver it. **The reader does not receive any of that.** Delivery on the live deployment is an external script that reads the digest JSON, renders its own list, and sends it — so the delivery layer is bypassed rather than used, and every property the engine gains reaches the reader only if the script happens to preserve it.

ADR-0007 put the subject-share ceiling in the engine for exactly this reason: a rule about the emitted set has to live where the emitted set is held. That decision is correct and it is **not sufficient**, which was established by arithmetic rather than by argument:

| | denominator | allowance |
|---|---|---|
| the engine, `subject_share: 0.25` | the **selected set**, 49 items | 12 |
| the script, `MAX_PER_TOPIC` | its own cap, `max(1, 8 // 4)` | **2** |

🔑 **The two rules are the same words over different denominators, and the script's applies last.** Its per-subject limit was never an independent quarter rule — it is derived from `MAX_ITEMS = 8`, so it is a quarter *of what the reader receives*. The engine trims 49 to about 44 and the script then takes 8, so the engine's ceiling is invisible behind it.

⚠️ **The reader's instruction was always about what he receives.** "No subject over about a quarter" is a statement about the digest in front of him, not about an intermediate set he never sees. Measured against the selected set, a quarter is a far weaker rule than the one he asked for.

So the engine has the balance and the script has the length, each rule enforced against a different set, and neither artifact is the one the reader reads.

### The constraint that makes this more than plumbing

**The script does not only truncate. It produces a spoken reading**: it renders a separate voice script with no emoji, URLs or scores, sends that for text-to-speech, and delivers audio alongside the text.

🚨 **The voice was an explicit request from the reader.** Moving delivery to the built-in telegram sink as it stands would give him the whole written digest and take the audio away — a strictly worse trade than the length regression, and one that would be discovered after the switch rather than before it.

## Decision

**1. An edition may state a target number of entries.** Optional, per edition, no default — the same posture as `subject_share`, and for the same reason: a default would silently change what every existing configuration produces.

**2. When a target length is configured, the subject share is measured against IT, not against the selected set.** This supersedes ADR-0007 §3's denominator for that case; with no target configured, ADR-0007 stands unchanged.

🔑 The share is a property of the digest, and the digest is the thing being filled. Measuring it against the selected set answers a question nobody asked — which is precisely the defect this ADR exists to close, and it was invisible for as long as the two rules lived in different programs.

**3. Length and share are ONE selection problem, not two filters. A single pass fills the digest subject by subject.**

Strongest item first within each subject, subjects taken in a stable order, one item at a time, stopping when the target is reached or when every subject sits at its allowance. **There is no ordering to get wrong, because nothing is applied after anything else.**

⛔ **Rejected: cap first, then the ceiling.** Take the strongest N, then trim each subject to its allowance. If the strongest N happen to be one subject, the ceiling then cuts them to the allowance and the digest is two entries — the cap and the ceiling each behaving correctly and jointly producing a result neither wanted.

⛔ **Rejected: the ceiling first, then the cap.** Balance the selected set, then take the strongest N of it. The cap can take N items from a single subject, **undoing the balance the previous step just imposed** — the second filter silently discards the first filter's work, and the output looks like a digest that was never balanced.

🔑 **Both degenerate cases exist only because the rules are applied in sequence.** A fill has neither, because a subject never takes a slot while another subject is still waiting for one. That is also why this is a decision rather than an implementation detail: the three candidates produce different digests from identical inputs, and the difference is not visible from either rule in isolation.

**4. The fill stops short rather than padding.** When every subject has reached its allowance and the target is not met, the digest is shorter than the target and that is the correct outcome.

This is ADR-0007 §7 unchanged and the engine's standing position that a short or empty digest is valid. ⚠️ **It is also what makes a lopsided day come out SHORT rather than unbalanced** — the failure mode is the one the engine already treats as correct, rather than the one the reader complained about.

**5. Subject order within the fill is deterministic**: by each subject's strongest item, ties broken by the subject label. Re-running a day must produce the same digest, for the reason ADR-0007 §5 gives — a digest that reshuffled between runs would make every comparison between two runs meaningless.

**6. An item the fill leaves out is recorded, and "the digest was full" is a DIFFERENT reason from "its subject was at its share."**

The report doctrine applies unchanged: these must never be folded into one string. A rising count of the first says there was more news than this reader's chosen length admits — a statement about the length. A rising count of the second says the source list has tilted towards one subject — a statement about the configuration. **The same reason for both answers neither question.**

**7. Delivery moves into the delivery layer, and the spoken reading becomes an exec sink under ADR-0003 rather than a bypass.**

- ⛔ **The engine gains no speech.** It stays provider-agnostic (ADR-0001 §3): text-to-speech is an external program, exactly as a collector is.
- The script **keeps** what only it does: rendering a voice script, obtaining audio, and sending audio with text.
- The script **loses** what is now the engine's: truncation and per-subject balance.
- Its failures become the engine's to report. Today a delivery that fails is invisible to the run that produced the digest.

**8. The sink envelope gains the written markdown, additively — and this is required rather than convenient.**

⚠️ **An exec sink is not handed the prose today, and it is easy to assume otherwise.** The ADR-0003 §3 envelope carries `digest` only, and that is the structured JSON; `internal/deliver/exec.go` marshals the structured bytes and nothing else. Stated here because an earlier draft of §7 asserted the opposite, and the mistake is the natural one — the sink is called a digest sink and the field is called `digest`.

🚨 **The consequence is larger than the correction, and it inverts the decision above.** `items[]` is one entry per **article** and pre-merge. One-event-one-entry is a property of the *writing pass's prose* and exists nowhere else — it is exactly why #58's known limitation is that the merge does not reach `items[]` or the report's counts. So a voice reading rendered from `items[]` gives one event several entries.

**That is the defect #63 was filed about, reproduced inside the fix for it.** Moving the script behind the plugin contract without the prose would relocate the bypass rather than close it, and the result would look like a delivery running through the delivery layer — which is the strongest possible disguise for the thing being fixed.

- The field is **additive and the contract stays at 1.** An existing plugin ignores a field it does not read; a plugin that needs the prose checks for its presence.
- ⛔ **Rejected: bumping the contract to 2.** ADR-0003 §7 makes a plugin needing a newer contract fail loudly — correct behaviour, and here it would fail every existing plugin over a field none of them uses.
- The structured bytes stay exactly as they are on disk, unchanged, alongside it.

🚨 **Ordering, and it is the whole point of writing this down:** configuring a target length changes nothing for the reader while the script still truncates last. The engine's fill is invisible behind the script's cap in exactly the way the share ceiling already was. **The sink move is not a follow-up to this decision — it is what makes this decision reach anyone.**

## Consequences

- ADR-0007's balance pass becomes the fill. Same stage, same grouping call, same deterministic-arithmetic split; the trim-each-subject step is replaced by a fill-to-target step, and the allowance's denominator changes when a target is configured.
- An edition that configures neither setting is unchanged, and pays for no extra call.
- 🔑 **The target is not always reachable, and the shortfall has a formula.** The most a fill can produce is `min(target, subjects × allowance)`. With `allowance = max(1, floor(share × target))`, a day carrying few subjects cannot fill the target however much news there is: at a quarter and a target of eight, three subjects yield six and two yield four.

  ⚠️ **This is stricter than "fewer than `1 / share` subjects", because the flooring compounds it.** The real threshold is `subjects ≥ ceil(target / allowance)` — at a quarter and a target of **ten**, the allowance floors to two, so **five** subjects are needed rather than four. Whenever `share × target` is not a whole number, the reachable length falls further below the target than the share alone suggests.

  This is §4 working as intended and is not an objection to it. It is stated because **"the digest is short today" will have two causes that look identical to the reader** — little news, or few subjects — and only the second is surprising to someone who configured a target of eight. §6's two absence reasons are what make them distinguishable after the fact.
- **Two settings can now disagree in a way worth stating:** a target length with no share fills round-robin over a single subject if that is all there is, which is correct — a one-subject day is carried, shortened to the target. A share with no target keeps ADR-0007's behaviour exactly.
- The sink envelope gains a field, so a plugin author reading ADR-0003 §3 would see a payload that no longer matches the example there. **Updating that example is part of implementing this decision, not part of taking it** — the example is correct today and stays correct until the field exists.
- The report gains one absence reason and the digest's item list becomes shorter than `Selected` more often. That seam is already documented on `ReportEdition`; this widens the gap between articles-selected and entries-delivered, and every removal remains individually accounted for.
- ⚠️ **What is still not covered by any test: the grouping.** The fill is deterministic and testable; the partition it fills from is a model judgement, and a poor partition produces a well-formed digest. Unchanged from ADR-0007, and worth restating because this decision gives the grouping more influence — it now shapes length as well as balance.

## Not decided here

- **The target number.** The script uses 8. Whether that is right is a question for the reader and for the configuration, and the mechanism is indifferent to it.
- **Whether the report's counts move from articles to events.** Untouched, and still its own decision.
- **Anything about the voice** — how it is rendered, what produces the audio, or what the reading sounds like. This decision only says the program that does it is a sink rather than a bypass, and that it is handed the prose.
- **Whether the report's absence lists should reconcile against events rather than articles** once the prose is what a sink renders. The seam widens here and closing it is still ADR-0006's open question, not this one.
