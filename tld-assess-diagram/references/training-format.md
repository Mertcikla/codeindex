# Auditable reader labels for training

Use this extension when the caller requests training/evaluation data. Preserve the main reader evidence boundary. These outputs supervise reader outcomes and diagnose explanation gaps. They are **not** verified fact-to-concept membership, ideal architecture targets, factual correctness labels, or preferences between diagrams. A separate source-aware annotator can join them to an indexed snapshot later.

## Capture a reproducible run

Ask for no additional permissions when an output directory was supplied. Save `report.json` and every actual CLI stdout under that directory, including setup/help and failed calls. Capture exact UTF-8 text before parsing; count Unicode characters, not bytes or abbreviated tool output. Include raw stderr separately for failures. Do not silently truncate transcripts. Record for each query:

- Existing query-log fields, plus `raw_path` (relative to report directory), `sha256` (of raw stdout bytes), and `readable_characters`.
- `readable_characters` equals the full output length for setup calls. For content it is the minimum of returned length and remaining character allowance before the call. Stop immediately after an overrun and do not use the unread suffix as evidence.
- Freeze scenario selection and budgets in the report. Record actual versions with `tld version` or supported equivalent if available; a failed version probe belongs in the setup log and does not invalidate an otherwise accessible diagram.

Set `budget.exhausted` to true exactly when content queries used, distinct views visited, or returned content characters reach or exceed their configured maximum; otherwise set it to false.

Add `training_export` to the protocol-2 report:

```json
{
  "run_id": "a unique locally generated identifier",
  "skill_sha256": "hash of the SKILL.md used for this run",
  "reference_sha256": {"report-format.md": "hash", "training-format.md": "hash"},
  "indexed_snapshot_id": null,
  "diagram_revision": null,
  "reader_outcome_eligible": true,
  "preference_eligible": false,
  "source_membership_eligible": false
}
```

Use null for revisions unavailable through the allowed CLI; do not invent them from timestamps. Before joining to facts, the harness must pin the diagram and source snapshot. Reader-outcome eligibility requires completed execution, valid accounting, and no prior-system/cross-candidate contamination. Exploratory observations may be reader-outcome eligible but never preference eligible. Preference eligibility additionally requires comparison_eligible and a controlled comparison; this single-candidate skill never emits a winner/loser label. Source-membership eligibility is always false.

## Label the requirements separately

Add `criteria` to each Q1–Q7 entry, exactly two objects using the IDs below. Each object contains `requirement_id`, `verdict`, `reason`, and `evidence`.

| Question | Requirement IDs | Interpretation |
| --- | --- | --- |
| Q1 | Q1.purpose, Q1.scope | Purpose/outcome; scope boundary |
| Q2 | Q2.responsibilities, Q2.distinction | Main responsibilities; supported distinction |
| Q3 | Q3.participants, Q3.relationships | Start/consumer and result/provider roles; supporting typed/directed collaboration |
| Q4 | Q4.boundary, Q4.crossing | Ownership/grouping boundary; interaction crossing it or explicit self-contained scope |
| Q5 | Q5.refinement, Q5.parent_connection | Useful newly revealed detail; how it refines the parent's responsibility |
| Q6 | Q6.change_location, Q6.related_inspection | Plausible place to start; evidenced related place to inspect |
| Q7 | Q7.element_anchors, Q7.relationship_evidence | Two relevant precise anchors; evidence for a relationship claim |

Verdicts: `supported`, `partial`, `not_established`, `unassessed`. Reasons explain the observable basis; distinguish not_found, ambiguous, contradictory, budget_exhausted, excluded, and tool_failure. With two supported criteria the question gets 2 points; with some supported/partial evidence it gets 1; otherwise 0. Excluded or infrastructure-unassessed questions retain null points and unassessed criteria. A budget-limited unknown is not an architecture defect: use not_established with reason budget_exhausted.

Keep absent information local to inspected resources. Record all evidence needed for credit: Q7.element_anchors needs two anchors, and Q5 needs parent and child evidence. File names and counts alone do not establish responsibilities. Do not synthesize replacement concepts or desired memberships from system knowledge.

## Machine-checkable evidence

Each criterion evidence entry retains query_id, resource_type, ref, id, and observation, and adds:

```json
{
  "query_id": "C1",
  "resource_type": "element",
  "ref": "actual-returned-ref",
  "id": 123,
  "field": "description",
  "json_pointer": "/element/description",
  "value": "exact JSON value at the pointer",
  "observation": "How the visible value supports this assessment"
}
```

Use actual refs/IDs; null IDs are allowed if the CLI does not expose them. JSON pointers follow RFC 6901 escaping. `value` must equal the parsed value, not a paraphrase. For absence, cite the containing object or inspected list and explain what is missing; do not invent a pointer to a missing field. Do not cite unparseable or unread content. Prefer small field values to whole responses.

## Export and validate

Run:

```bash
python3 PATH_TO_SKILL/scripts/validate_training.py OUTPUT_DIR/report.json --output OUTPUT_DIR/training.jsonl
```

Repair format/accounting/citation errors from the already captured evidence. Do not issue new content queries after budgets are exhausted or fabricate evidence to satisfy validation. If the evidence cannot support a label, downgrade it or mark the run ineligible. Validation checks traceability and consistency, not whether the reader's interpretation is correct. Report semantic uncertainties separately.

The exporter produces one `reader_requirement` record per criterion (14 by default), carrying run provenance, target, scenario, verdict, evidence, and eligibility. Excluded/unassessed records remain explicitly masked. In a downstream dataset, retain false/null eligibility flags and never convert missing/source-unverified labels into factual negatives.
