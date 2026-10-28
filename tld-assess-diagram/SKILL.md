---
name: tld-assess-diagram
description: Assess an architecture diagram as a reader using read-only tld CLI queries. Answer generic architecture questions, cite diagram evidence, and report answerability, navigation effort, and abstraction weaknesses. Export auditable reader-outcome labels for training when requested. Use for evaluating generated or authored diagrams, not creating or repairing them.
---

# Assess a Diagram as a Reader

Measure what a reader can establish from a selected diagram within a bounded investigation. Query diagram content through `tld` CLI only. Evaluate the diagram's explanation, not your prior knowledge of its system or its author.

## Evidence boundary

- Use diagram names, descriptions, placements, hierarchy, connectors, source-link metadata, and evidence descriptions returned by the CLI.
- Do not read application source, repository documentation, raw databases, cached diagram YAML, browser pages, earlier assessments, or other candidate diagrams. Do not open source links. Parsing or saving CLI JSON and doing arithmetic locally are allowed.
- Do not run indexing, synchronization, imports, layout changes, or diagram mutations. Do not run `validate`: it can inspect source and its workspace-wide warnings are not a reader score.
- Treat descriptions as claims made by the diagram. A label or file link does not verify a claim against implementation. Record source correctness and visual readability as **not assessed**. CLI output cannot establish crossings, occlusion, or legibility.
- If you already know this system or have seen another candidate, disclose contamination. A benchmark harness should run each candidate in a fresh reader context without author/model labels or expected answers. Ignore any author information encountered in metadata.

## Select the target and freeze the assessment

Accept a target/data directory or workspace, an overview view ref/ID, and optional audience, reader scenario, question exclusions, and budgets. Use supplied values; never default to a different database when the requested one is unavailable. If the overview is ambiguous, list view metadata to identify it and ask only when necessary. Metadata used to select a candidate is not evidence for answers.

Run `tld context --format json` with the destination flags. Pin the same `--target`, `--data-dir`, and/or `--workspace` on every later command, and use `--source live --format json` for reads. If given a database filename, use its containing directory as `--data-dir` only when it is the normal `tld.db` file; verify the resolved destination. Inspect command help when flags or identifier semantics are uncertain.

The candidate is its overview and reachable child views. Ancestor galleries and sibling alternatives are out of scope. A connector's off-view endpoint may be inspected for context, but that does not authorize reading a sibling candidate or its subtree. An off-view connector is not broken merely because an endpoint is not placed in the current view.

When training data is requested, also read [references/training-format.md](references/training-format.md) before executing queries. It adds evidence capture and per-requirement labels; it does not authorize source inspection or change this reader into an architecture author.

Before reading content, record the scope, seven question IDs below, equal weights, and budgets. Only caller-declared exclusions or an assessment brief established independently of the candidate can remove questions. Missing detail is not a reason to mark a question inapplicable. If no brief was supplied, use the default questions and mark the assessment exploratory.

## Query and navigate

Use the selected overview's ref/ID in place of `VIEW_REF` below. Append the pinned destination flags to every command:

```bash
tld inspect VIEW_REF --type view --source live --format json
tld render VIEW_REF --source live --format json
tld list connectors --view VIEW_REF --source live --format json
tld inspect ELEMENT_REF --type element --source live --format json
tld inspect CONNECTOR_KEY --type connector --source live --format json
```

Resolve the overview ID with `inspect --type view`, then use the returned view **ref** for `--view` filters. Some CLI versions accept a numeric ID in `inspect` but silently return an empty connector list for that ID in `list connectors --view`. If list results disagree with inspected connector keys, retry once using the resolved ref; do not label the diagram empty. For view inspection, use the target `sources[].id` as the view ID; `element.metadata.id` identifies its owner, not the view.

`inspect --type view` exposes its owner, child refs, connector keys, and visibility diagnostics. `render` provides a compact Mermaid representation of directly placed elements and relationships. Inspect relevant elements for responsibilities, source-link metadata, and child views. Use `--type` to distinguish an element from its owned view. Preserve the returned ref, numeric ID, and type when citing resources; element and view IDs can differ.

Mermaid omits off-view relationships. Check the reported omitted/off-view connectors through `list connectors --view` or `inspect` before calling a relationship absent. Preserve directions and distinguish dependency, containment, implementation, and runtime/data flow; a generic dependency does not establish an execution sequence.

After the overview, record the behavior and responsibility you will follow, before exploring children. Prefer the supplied scenario. Otherwise choose a behavior and responsibility presented as central by the overview; record why, and keep them fixed even if their explanation proves poor. Avoid choosing an easy peripheral branch after seeing failures. Inspect a contrasting sibling when assessing boundaries. Follow further levels when they help answer a question; do not reward depth for itself.

Maintain a query ledger with command, resource refs, returned character count, newly visited views, and question IDs served. Record dead ends and backtracking. Reuse already-read content without another CLI call. Stop when the questions have sufficient evidence, the budget is exhausted, or access fails. Tool/access failures are evaluation failures, not diagram defects; allow one help-informed correction of a malformed command, then report a blocked or incomplete assessment if access remains unavailable.

## Generic questions and evidence requirements

| ID | Reader question | Evidence needed for full credit |
| --- | --- | --- |
| Q1 | What does this system or selected subsystem do, and what is in scope? | A supported purpose/outcome and a recognizable scope boundary. A repository or product name alone is insufficient. |
| Q2 | What are its main responsibilities, and how do they differ? | Identify the main responsibilities presented by the overview; explain the distinction between at least two using diagram evidence. For an explicitly single-responsibility system, explain that responsibility and its stated limits. |
| Q3 | How does one representative behavior or collaboration work? | Identify a depicted start/consumer, participating responsibilities, and result/provider; cite relationships supporting their roles and direction. Use a static dependency explanation when that is what the diagram depicts, without inventing runtime order. |
| Q4 | Where are the meaningful boundaries, and what crosses them? | Explain one grouping/ownership boundary and a depicted interaction across it, or an explicit statement that the scope is self-contained. Distinguish internal responsibilities from external context when shown. Absence of edges is not proof of isolation. |
| Q5 | What useful detail do I learn by entering a major responsibility? | Cite the parent and child view, explain newly revealed responsibilities or interactions, and show how they refine the parent's purpose. Repeated names, arbitrary partitions, and extra nesting alone do not qualify. If needed detail has no drill-down, record that gap. |
| Q6 | Where would I start changing the selected behavior, and what else should I inspect? | Identify a plausible responsibility to change and a related consumer/provider or explicit isolation boundary, supported by diagram relationships. Describe inspection candidates, not guaranteed runtime impact. |
| Q7 | Can I trace the explanation to implementation or other supporting evidence? | Report the anchors attached to two relevant elements and the evidence attached to one relevant relationship, explaining which claims they purport to support. Generic root-folder links or bare assertions are insufficient for full credit. Report precision, not whether those links or claims are true. |

Apply these distinctions before scoring:

- Q1 asks about the depicted system/subsystem. “Generated map for code grounding” describes the artifact, not the system's purpose. An element count or inventory alone does not establish a meaningful scope boundary.
- Q2 needs duties or behavior. Bare names and dependency counts may guide navigation, but earn no responsibility credit without an interpretable duty supported by the diagram.
- Q4 needs an explanation of what a boundary encloses or owns. A parent pointer establishes containment; counts alone do not explain the boundary's meaning.
- Q6 needs evidence connecting the selected behavior to a responsibility or operation. Merely repeating an endpoint supplied in the scenario does not establish a change location. A typed relationship can still support the related-inspection criterion.
- A connector explicitly marked `both` depicts a bidirectional relationship. Do not call its direction ambiguous merely because it lacks a single direction; distinguish that from missing participant roles or behavior descriptions.

For each included question, assign:

- **2 — supported:** all evidence requirements are met; cite the resources and explain how they support the answer.
- **1 — partial:** some requirements are supported, but name the specific missing distinction, interaction, refinement, or anchor.
- **0 — not established:** the answer is absent, ambiguous, contradictory, or requires guessing from source knowledge. Distinguish `not_found`, `ambiguous`, `contradictory`, and `budget_exhausted` in the reason.

Budget exhaustion is a measured reader failure under this protocol, not proof that the whole diagram lacks the answer. A CLI failure or corrupted response instead makes the run incomplete and its aggregate score null. Empty but successfully retrieved diagrams receive zero for unsupported questions.

## Score and report

Compute `reader_answerability = 100 * sum(points) / (2 * included_question_count)`, rounded to one decimal. The default denominator is 14. Use null if the run is blocked/incomplete or no questions apply. This is an anchored reader rubric, not independently verified correctness or an overall architecture-quality percentage. Do not interpret a ratio of these scores as “80% as good.”

Report navigation effort and concrete abstraction failures separately from the score. Do not award points for node/edge totals, modularity, hierarchy depth, label verbosity, source-link presence alone, or matching another diagram. Reusing an element across views or representing multiple responsibilities from one file can be legitimate.

Return the structured report specified in [references/report-format.md](references/report-format.md), plus a short summary of the strongest explanations and most consequential reader obstacles. List only observed defects, cite exact resources, and distinguish an unclear name from an incoherent boundary. Never infer an extraction failure from diagram absence alone. Leave the diagram unchanged.
