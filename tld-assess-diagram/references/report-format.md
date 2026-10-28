# Reader assessment report

Use this contract for every completed or blocked assessment. Return JSON in the answer unless the caller requests a file; write a requested report only to the designated output location. Do not write reports into the diagram workspace by default.

Use `protocol_version: "2"`. Record actual values rather than copying example budgets when the caller supplied overrides.

```json
{
  "protocol_version": "2",
  "status": "completed",
  "assessment_mode": "exploratory",
  "target": {
    "target": "local",
    "data_dir": null,
    "workspace": null,
    "overview_ref": "selected-overview",
    "overview_view_id": null
  },
  "scope": "The selected overview and reachable child views",
  "scenario": {
    "behavior": "Behavior selected from the overview",
    "responsibility_ref": "selected-responsibility",
    "selection_reason": "Why it is central to the stated purpose"
  },
  "budget": {
    "max_content_queries": 24,
    "max_distinct_views": 6,
    "max_content_characters": 60000,
    "content_queries_used": 0,
    "distinct_views_visited": [],
    "content_characters_returned": 0,
    "character_overrun": 0,
    "exhausted": false
  },
  "questions": [
    {
      "id": "Q1",
      "included": true,
      "points": 0,
      "answer": "What can be established from the diagram",
      "evidence": [
        {
          "query_id": "C1",
          "resource_type": "view",
          "ref": "selected-overview",
          "id": null,
          "field": "description",
          "observation": "A short exact excerpt or faithful paraphrase"
        }
      ],
      "gaps": ["Specific unsatisfied evidence requirement"],
      "reason": "not_found",
      "exclusion_basis": null
    }
  ],
  "scores": {
    "earned_points": 0,
    "possible_points": 14,
    "reader_answerability": 0.0,
    "source_correctness": null,
    "visual_readability": null,
    "whole_system_completeness": null
  },
  "findings": [
    {
      "dimension": "responsibility_clarity",
      "observation": "An observed obstacle or strength",
      "resource_refs": ["selected-responsibility"],
      "question_ids": ["Q2"],
      "effect_on_reader": "How the observation affected understanding or navigation"
    }
  ],
  "query_log": [
    {
      "id": "C1",
      "phase": "content",
      "command": "Exact tld command with the actual pinned destination flags",
      "resource_refs": ["selected-overview"],
      "content_characters": 0,
      "new_view_refs": ["selected-overview"],
      "question_ids": ["Q1", "Q2"],
      "outcome": "ok"
    }
  ],
  "limitations": [
    "Source correctness and rendered visual readability were not assessed"
  ],
  "contamination": [],
  "comparison_eligible": false
}
```

The example shows one question and one query for brevity; include **Q1–Q7 exactly once** and every actual query, including setup calls. Setup calls have `phase: "setup"` and do not count toward content-query limits. Record nonzero measured character counts for returned content. Reading an owned view through an element inspection counts as visiting that view when its children or connectors are exposed; a view ref merely mentioned by another resource does not.

Question `reason` is one of `supported`, `partial`, `not_found`, `ambiguous`, `contradictory`, `budget_exhausted`, `excluded`, or `tool_failure`. Excluded and unassessed questions have `points: null`; partial answers have 1, supported answers 2, and measured reader failures 0. Exclusions require an independent brief/caller instruction in `exclusion_basis`; diagram omissions cannot justify them.

`status` is `completed`, `incomplete`, or `blocked`. A completed run may exhaust its budget and retain unanswered questions scored 0. Infrastructure failures, incomplete accounting, or continued queries after budget exhaustion produce a null aggregate, while retaining any partial observations. A single response crossing the character limit is allowed when the reader stops immediately and uses only its in-budget prefix. Record protocol violations in `contamination` and limitations. Never convert unknown scores to zero. A valid empty diagram is assessable, not blocked.

Set `assessment_mode: "benchmark"` only when the caller supplies a frozen common brief/scenario, question set and budgets for a controlled comparison. Set `comparison_eligible: true` only for completed benchmark runs without contamination or target/content changes during assessment. Exploratory runs can inform rubric development but should not enter a controlled leaderboard. Note that independently chosen behaviors make two exploratory scores imperfectly comparable.

Use findings dimensions such as `responsibility_clarity`, `boundary_coherence`, `relationship_explanation`, `progressive_detail`, `evidence_precision`, and `navigation_effort`. Support a hierarchy defect with both the parent and child observations. If siblings remain unread, limit the claim accordingly. Disclose metadata-only candidate discovery, prior system knowledge, unexpected sibling content, and any unread/truncated content in limitations or contamination as appropriate.

An independent verifier can later check the cited claims against source or an answer specification. Keep those verdicts separate from this reader's score and do not supply expected answers to the reader before its investigation.
