#!/usr/bin/env python3
"""Validate protocol-2 reader reports and export criterion-level JSONL labels.

This deliberately validates traceability and arithmetic, not the semantic truth
of a reader's interpretation. It only uses the Python standard library.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import re
import sys
import tempfile
from pathlib import Path
from typing import Any


CRITERIA = {
    "Q1": ("Q1.purpose", "Q1.scope"),
    "Q2": ("Q2.responsibilities", "Q2.distinction"),
    "Q3": ("Q3.participants", "Q3.relationships"),
    "Q4": ("Q4.boundary", "Q4.crossing"),
    "Q5": ("Q5.refinement", "Q5.parent_connection"),
    "Q6": ("Q6.change_location", "Q6.related_inspection"),
    "Q7": ("Q7.element_anchors", "Q7.relationship_evidence"),
}
VERDICTS = {"supported", "partial", "not_established", "unassessed"}
REASONS = {
    "supported", "partial", "not_found", "ambiguous", "contradictory",
    "budget_exhausted", "excluded", "tool_failure", "not_established",
}
PHASES = {"setup", "content"}
SHA256 = re.compile(r"^[0-9a-f]{64}$")


class ValidationError(Exception):
    pass


def fail(message: str) -> None:
    raise ValidationError(message)


def need(condition: bool, message: str) -> None:
    if not condition:
        fail(message)


def pointer_get(document: Any, pointer: str) -> Any:
    need(isinstance(pointer, str) and (pointer == "" or pointer.startswith("/")),
         "json_pointer must be an RFC 6901 pointer")
    value = document
    if pointer == "":
        return value
    for raw in pointer[1:].split("/"):
        need(not re.search(r"~(?![01])", raw), f"invalid JSON pointer escape in {pointer!r}")
        token = raw.replace("~1", "/").replace("~0", "~")
        if isinstance(value, dict):
            need(token in value, f"JSON pointer does not exist: {pointer}")
            value = value[token]
        elif isinstance(value, list):
            need(token.isdigit() and (token == "0" or not token.startswith("0")),
                 f"invalid array index in JSON pointer: {pointer}")
            index = int(token)
            need(index < len(value), f"JSON pointer does not exist: {pointer}")
            value = value[index]
        else:
            fail(f"JSON pointer traverses a scalar: {pointer}")
    return value


def identity_values(value: Any, keys: set[str]) -> set[Any]:
    """Collect scalar identity values, including nested metadata/source IDs."""
    found: set[Any] = set()
    if isinstance(value, dict):
        for key, child in value.items():
            if key in keys and isinstance(child, (str, int)) and not isinstance(child, bool):
                found.add(child)
            found.update(identity_values(child, keys))
    elif isinstance(value, list):
        for child in value:
            found.update(identity_values(child, keys))
    return found


def load_report(path: Path) -> dict[str, Any]:
    try:
        report = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        fail(f"cannot read report JSON: {exc}")
    need(isinstance(report, dict), "report must be a JSON object")
    return report


def validate(report: dict[str, Any], report_path: Path) -> list[dict[str, Any]]:
    need(report.get("protocol_version") == "2", "protocol_version must be '2'")
    need(report.get("status") in {"completed", "incomplete", "blocked"}, "invalid status")
    questions = report.get("questions")
    need(isinstance(questions, list), "questions must be an array")
    by_id: dict[str, dict[str, Any]] = {}
    for q in questions:
        need(isinstance(q, dict) and q.get("id") in CRITERIA, "question id must be Q1..Q7")
        qid = q["id"]
        need(qid not in by_id, f"duplicate question {qid}")
        by_id[qid] = q
    need(set(by_id) == set(CRITERIA), "questions must contain Q1..Q7 exactly once")

    qlog = report.get("query_log")
    need(isinstance(qlog, list), "query_log must be an array")
    queries: dict[str, dict[str, Any]] = {}
    raw_docs: dict[str, Any] = {}
    content_characters_before: dict[str, int] = {}
    running_content_characters = 0
    for query in qlog:
        need(isinstance(query, dict), "query_log entries must be objects")
        qid = query.get("id")
        need(isinstance(qid, str) and qid and qid not in queries, "query ids must be unique strings")
        phase = query.get("phase")
        need(phase in PHASES, f"query {qid}: phase must be setup or content")
        raw_path = query.get("raw_path")
        need(isinstance(raw_path, str) and raw_path, f"query {qid}: raw_path required")
        raw_file = (report_path.parent / raw_path).resolve()
        try:
            raw_file.relative_to(report_path.parent.resolve())
        except ValueError:
            fail(f"query {qid}: raw_path must stay within report directory")
        try:
            raw_bytes = raw_file.read_bytes()
            raw_text = raw_bytes.decode("utf-8")
        except (OSError, UnicodeError) as exc:
            fail(f"query {qid}: cannot read UTF-8 stdout at {raw_path}: {exc}")
        digest = hashlib.sha256(raw_bytes).hexdigest()
        need(query.get("sha256") == digest, f"query {qid}: stdout sha256 mismatch")
        char_count = len(raw_text)
        need(query.get("content_characters") == char_count,
             f"query {qid}: content_characters must equal measured Unicode character count ({char_count})")
        readable = query.get("readable_characters")
        need(isinstance(readable, int) and not isinstance(readable, bool) and 0 <= readable <= char_count,
             f"query {qid}: invalid readable_characters")
        budget = report.get("budget", {})
        if phase == "setup":
            need(readable == char_count, f"query {qid}: setup output must be fully readable")
        else:
            limit = budget.get("max_content_characters") if isinstance(budget, dict) else None
            prior = running_content_characters
            if isinstance(limit, int):
                need(prior < limit, f"query {qid}: content query issued after character budget exhausted")
                need(readable == min(char_count, max(0, limit - prior)),
                     f"query {qid}: readable_characters does not match remaining character budget")
            content_characters_before[qid] = prior
            running_content_characters += readable
        queries[qid] = query
        # Parse all stdout that can be used as machine-checkable evidence. For
        # overrun outputs we conservatively forbid citations: the full structure
        # may depend on an unread suffix and JSON prefixes are often incomplete.
        if readable == char_count:
            try:
                raw_docs[qid] = json.loads(raw_text)
            except json.JSONDecodeError:
                raw_docs[qid] = None

    criterion_rows: list[tuple[str, dict[str, Any]]] = []
    for qid, expected_ids in CRITERIA.items():
        q = by_id[qid]
        need(isinstance(q.get("included"), bool), f"{qid}: included must be boolean")
        if q["included"] is False:
            basis = q.get("exclusion_basis")
            need(isinstance(basis, str) and basis.strip(), f"{qid}: excluded question requires exclusion_basis")
        criteria = q.get("criteria")
        need(isinstance(criteria, list) and len(criteria) == 2, f"{qid}: exactly two criteria required")
        got_ids = [c.get("requirement_id") if isinstance(c, dict) else None for c in criteria]
        need(set(got_ids) == set(expected_ids) and len(set(got_ids)) == 2,
             f"{qid}: criteria IDs must be {', '.join(expected_ids)}")
        for criterion in criteria:
            req = criterion["requirement_id"]
            verdict = criterion.get("verdict")
            reason = criterion.get("reason")
            need(verdict in VERDICTS, f"{req}: invalid verdict")
            need(isinstance(reason, str) and reason.strip(), f"{req}: reason required")
            if verdict in {"not_established", "unassessed"}:
                need(any(tag in reason.lower() for tag in REASONS),
                     f"{req}: reason must identify the observable basis/category")
            evidence = criterion.get("evidence")
            need(isinstance(evidence, list), f"{req}: evidence must be an array")
            if verdict == "not_established" and not any(
                    category in reason.lower() for category in ("budget_exhausted", "tool_failure", "excluded")):
                need(bool(evidence), f"{req}: not_established from inspected content needs evidence")
            for ev in evidence:
                need(isinstance(ev, dict), f"{req}: evidence entries must be objects")
                for key in ("query_id", "resource_type", "ref", "id", "observation", "json_pointer", "value"):
                    need(key in ev, f"{req}: evidence missing {key}")
                query_id = ev["query_id"]
                need(query_id in queries, f"{req}: unknown evidence query {query_id!r}")
                query = queries[query_id]
                need(query["phase"] == "content", f"{req}: evidence cannot cite setup output")
                need(isinstance(ev["resource_type"], str) and ev["resource_type"], f"{req}: invalid resource_type")
                need(isinstance(ev["ref"], str) and ev["ref"], f"{req}: ref must identify an actual resource")
                need(ev["id"] is None or isinstance(ev["id"], (str, int)), f"{req}: id must be string, integer, or null")
                need(isinstance(ev["observation"], str) and ev["observation"].strip(), f"{req}: observation required")
                need(queries[query_id]["readable_characters"] == queries[query_id]["content_characters"],
                     f"{req}: cannot cite content from an overrun/truncated query")
                document = raw_docs.get(query_id)
                need(document is not None, f"{req}: cited stdout is not a complete JSON document")
                actual = pointer_get(document, ev["json_pointer"])
                need(actual == ev["value"], f"{req}: evidence value does not equal raw JSON pointer")
                # Inspect responses may put an element's metadata.id beside its
                # description and the owning view's ID under sources[].id.
                # Validate identities against the raw resource document rather
                # than requiring them to be direct ancestors of the field.
                ref_values = identity_values(document, {
                    "ref", "view_ref", "element_ref", "resource_ref", "name", "id",
                })
                need(ev["ref"] in ref_values,
                     f"{req}: cited ref {ev['ref']!r} is absent from raw resource identities")
                if ev["id"] is not None:
                    id_values = identity_values(document, {
                        "id", "element_id", "view_id", "resource_id",
                    })
                    need(ev["id"] in id_values,
                         f"{req}: cited id {ev['id']!r} is absent from raw resource identities")
            if verdict in {"supported", "partial"}:
                need(bool(evidence), f"{req}: supported criterion requires evidence")
            if req == "Q7.element_anchors" and verdict in {"supported", "partial"}:
                if verdict == "supported":
                    need(len(evidence) >= 2, "Q7.element_anchors: supported requires two precise anchors")
                    anchors = {(ev["ref"], ev["id"]) for ev in evidence}
                    need(len(anchors) >= 2, "Q7.element_anchors: anchors must identify two distinct elements")
            if req in {"Q5.refinement", "Q5.parent_connection"} and verdict in {"supported", "partial"}:
                need(len(evidence) >= 2, f"{req}: credit requires parent and child evidence")
                resources = {(ev["resource_type"], ev["ref"], ev["id"]) for ev in evidence}
                need(len(resources) >= 2,
                     f"{req}: evidence must identify parent and child resources")
            criterion_rows.append((qid, criterion))

        c_by_id = {c["requirement_id"]: c for c in criteria}
        assessed = [c for c in criteria if c["verdict"] != "unassessed"]
        if q.get("included") is False or len(assessed) == 0:
            expected_points = None
        elif len(assessed) != len(criteria):
            fail(f"{qid}: partially unassessed criteria must be marked unassessed together")
        elif all(c["verdict"] == "supported" for c in criteria):
            expected_points = 2
        elif any(c["verdict"] in {"supported", "partial"} for c in criteria):
            expected_points = 1
        else:
            expected_points = 0
        need(q.get("points") == expected_points, f"{qid}: points inconsistent with criterion verdicts")
        if expected_points is None:
            need(all(c["verdict"] == "unassessed" for c in criteria), f"{qid}: masked question criteria must be unassessed")
            if q["included"]:
                need(report["status"] != "completed", f"{qid}: completed assessment cannot leave an included question unassessed")
        del c_by_id

    scores = report.get("scores")
    need(isinstance(scores, dict), "scores must be an object")
    assessed_questions = [q for q in by_id.values() if q.get("points") is not None]
    earned = sum(q["points"] for q in assessed_questions)
    possible = 2 * len(assessed_questions)
    aggregate_is_null = report["status"] != "completed" or possible == 0
    aggregate = (None, None, None) if aggregate_is_null else (earned, possible, round(100 * earned / possible, 1))
    need(scores.get("earned_points") == aggregate[0], "earned_points inconsistent with assessment status/verdicts")
    need(scores.get("possible_points") == aggregate[1], "possible_points inconsistent with assessment status/questions")
    reported_answerability = scores.get("reader_answerability")
    if aggregate[2] is None:
        need(reported_answerability is None, "reader_answerability must be null for blocked, incomplete, or zero-question assessments")
    else:
        need(isinstance(reported_answerability, (int, float)) and not isinstance(reported_answerability, bool)
             and math.isclose(reported_answerability, aggregate[2], rel_tol=0, abs_tol=1e-9),
             "reader_answerability must equal rounded percentage earned/possible")

    budget = report.get("budget")
    need(isinstance(budget, dict), "budget must be an object")
    content_queries = [q for q in qlog if q["phase"] == "content"]
    need(budget.get("content_queries_used") == len(content_queries), "content_queries_used inconsistent with query_log")
    need(budget.get("content_characters_returned") == sum(q["content_characters"] for q in content_queries),
         "content_characters_returned inconsistent with measured stdout")
    overrun = sum(max(0, q["content_characters"] - q["readable_characters"]) for q in content_queries)
    need(budget.get("character_overrun") == overrun, "character_overrun inconsistent with query_log")
    max_queries = budget.get("max_content_queries")
    max_views = budget.get("max_distinct_views")
    max_chars = budget.get("max_content_characters")
    need(isinstance(max_queries, int) and not isinstance(max_queries, bool) and max_queries >= 0,
         "max_content_queries must be a nonnegative integer")
    need(isinstance(max_views, int) and not isinstance(max_views, bool) and max_views >= 0,
         "max_distinct_views must be a nonnegative integer")
    need(isinstance(max_chars, int) and not isinstance(max_chars, bool) and max_chars >= 0,
         "max_content_characters must be a nonnegative integer")
    need(len(content_queries) <= max_queries, "content query issued after max_content_queries was exhausted")
    observed_views = []
    observed_set = set()
    for query in content_queries:
        refs = query.get("new_view_refs")
        need(isinstance(refs, list) and all(isinstance(ref, str) and ref for ref in refs),
             f"query {query['id']}: new_view_refs must be an array of view refs")
        for ref in refs:
            if ref not in observed_set:
                observed_set.add(ref)
                observed_views.append(ref)
        need(len(observed_set) <= max_views, "content query issued after max_distinct_views was exhausted")
    visited = budget.get("distinct_views_visited")
    need(isinstance(visited, list) and all(isinstance(ref, str) and ref for ref in visited),
         "distinct_views_visited must be an array of view refs")
    need(len(set(visited)) == len(visited) and set(visited) == observed_set,
         "distinct_views_visited inconsistent with query_log new_view_refs")
    reached_limit = (len(content_queries) >= max_queries or len(observed_set) >= max_views
                     or budget["content_characters_returned"] >= max_chars)
    need(budget.get("exhausted") is reached_limit,
         "budget.exhausted must reflect whether any configured budget limit was reached")
    if overrun:
        later = False
        seen_overrun = False
        for q in content_queries:
            if seen_overrun:
                later = True
            if q["content_characters"] > q["readable_characters"]:
                seen_overrun = True
        need(not later, "content query issued after character overrun")

    export = report.get("training_export")
    need(isinstance(export, dict), "training_export must be an object")
    need(isinstance(export.get("run_id"), str) and export["run_id"].strip(), "training_export.run_id required")
    need(isinstance(export.get("skill_sha256"), str) and SHA256.fullmatch(export["skill_sha256"]), "invalid skill_sha256")
    ref_hashes = export.get("reference_sha256")
    need(isinstance(ref_hashes, dict) and set(ref_hashes) == {"report-format.md", "training-format.md"},
         "reference_sha256 must include report-format.md and training-format.md")
    need(all(isinstance(v, str) and SHA256.fullmatch(v) for v in ref_hashes.values()), "invalid reference_sha256")
    need(export.get("source_membership_eligible") is False, "source_membership_eligible must be false")
    need(export.get("preference_eligible") is False, "single-run export cannot be preference eligible")
    reader_eligible = export.get("reader_outcome_eligible")
    need(isinstance(reader_eligible, bool), "reader_outcome_eligible must be boolean")
    contamination = report.get("contamination")
    need(isinstance(contamination, list), "contamination must be an array")
    if reader_eligible:
        need(report.get("status") == "completed", "reader outcome eligibility requires completed execution")
        need(not contamination, "reader outcome eligibility requires no contamination")
    need(isinstance(budget.get("exhausted"), bool), "budget exhausted flag must be boolean")

    records = []
    common = {
        "run_id": export["run_id"],
        "protocol_version": report["protocol_version"],
        "skill_sha256": export["skill_sha256"],
        "reference_sha256": ref_hashes,
        "indexed_snapshot_id": export.get("indexed_snapshot_id"),
        "diagram_revision": export.get("diagram_revision"),
        "target": report.get("target"),
        "scope": report.get("scope"),
        "scenario": report.get("scenario"),
        "assessment_mode": report.get("assessment_mode"),
        "budget": budget,
        "query_log": qlog,
        "record_type": "reader_requirement",
        "allowed_use": "reader_outcome_only",
        "source_verified": False,
        "reader_outcome_eligible": reader_eligible,
        "preference_eligible": False,
        "source_membership_eligible": False,
    }
    for qid in CRITERIA:
        for criterion in by_id[qid]["criteria"]:
            question = by_id[qid]
            verdict = criterion["verdict"]
            records.append({**common, "question_id": qid,
                            "requirement_id": criterion["requirement_id"],
                            "verdict": verdict,
                            "evidence": criterion["evidence"],
                            "reason": criterion["reason"],
                            "included": question["included"],
                            "question_points": question["points"],
                            "exclusion_basis": question.get("exclusion_basis"),
                            "masked": not question["included"] or verdict == "unassessed",
                            "record_eligible": reader_eligible and question["included"] and verdict != "unassessed"})
    return records


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", nargs="?", help="protocol-2 report JSON")
    parser.add_argument("--output", help="output JSONL path")
    parser.add_argument("--self-test", action="store_true", help="run built-in validator regression tests")
    args = parser.parse_args(argv)
    if args.self_test:
        return self_test()
    if not args.report or not args.output:
        parser.error("REPORT and --output OUTPUT.jsonl are required")
    report_path = Path(args.report).resolve()
    output_path = Path(args.output).resolve()
    try:
        records = validate(load_report(report_path), report_path)
        output_path.parent.mkdir(parents=True, exist_ok=True)
        with output_path.open("w", encoding="utf-8", newline="\n") as stream:
            for record in records:
                stream.write(json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n")
    except (ValidationError, OSError) as exc:
        print(f"validate_training: {exc}", file=sys.stderr)
        return 1
    print(f"wrote {len(records)} reader_requirement records to {output_path}")
    return 0


def self_test() -> int:
    """Small stdlib-only regression suite for valid export and key failures."""
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        report_path = root / "report.json"
        raw = json.dumps({
            "view": {"ref": "view-root", "sources": [{"id": "view-a"}]},
            "element": {"metadata": {"id": 7}, "description": "visible λ"},
            "child": {"metadata": {"id": 8}, "description": "child λ"},
        }, ensure_ascii=False)
        (root / "C1.txt").write_text(raw, encoding="utf-8")
        raw_bytes = raw.encode()
        query = {"id": "C1", "phase": "content", "raw_path": "C1.txt",
                 "sha256": hashlib.sha256(raw_bytes).hexdigest(), "content_characters": len(raw),
                 "readable_characters": len(raw), "new_view_refs": ["view-a"]}
        evidence = {"query_id": "C1", "resource_type": "element", "ref": "view-a", "id": 7,
                    "field": "description", "json_pointer": "/element/description",
                    "value": "visible λ", "observation": "visible description"}
        child_evidence = {"query_id": "C1", "resource_type": "element", "ref": "view-a", "id": 8,
                          "field": "description", "json_pointer": "/child/description",
                          "value": "child λ", "observation": "visible child description"}
        qs = []
        for qid, ids in CRITERIA.items():
            criteria = []
            for req in ids:
                verdict, evs = "not_established", [dict(evidence)]
                reason = "not_found in inspected content"
                if qid == "Q1":
                    verdict, evs, reason = "supported", [dict(evidence)], "supported by visible evidence"
                elif qid == "Q2" and req == ids[0]:
                    verdict, evs, reason = "supported", [dict(evidence)], "supported by visible evidence"
                elif qid == "Q2" and req == ids[1]:
                    verdict, evs, reason = "partial", [dict(evidence)], "partial evidence in visible description"
                elif qid == "Q5" and req == ids[0]:
                    verdict, evs, reason = "partial", [dict(evidence), dict(child_evidence)], "partial parent and child observations"
                elif qid == "Q7" and req == ids[0]:
                    verdict, evs, reason = "partial", [dict(evidence)], "partial evidence from one visible anchor"
                criteria.append({"requirement_id": req, "verdict": verdict, "reason": reason, "evidence": evs})
            points = 2 if all(c["verdict"] == "supported" for c in criteria) else (
                1 if any(c["verdict"] in {"supported", "partial"} for c in criteria) else 0)
            qs.append({"id": qid, "included": True, "points": points, "criteria": criteria})
        report = {"protocol_version": "2", "status": "completed", "assessment_mode": "exploratory",
                  "target": {"target": "local"}, "scope": "test scope", "scenario": {"behavior": "test"},
                  "budget": {"max_content_characters": 10000, "max_content_queries": 24,
                             "max_distinct_views": 6, "distinct_views_visited": ["view-a"],
                             "content_queries_used": 1,
                             "content_characters_returned": len(raw), "character_overrun": 0,
                             "exhausted": False},
                  "questions": qs, "scores": {"earned_points": 5, "possible_points": 14,
                                                "reader_answerability": 35.7},
                  "query_log": [query], "contamination": [],
                  "training_export": {"run_id": "test-run", "skill_sha256": "a" * 64,
                                      "reference_sha256": {"report-format.md": "b" * 64,
                                                           "training-format.md": "c" * 64},
                                      "indexed_snapshot_id": None, "diagram_revision": None,
                                      "reader_outcome_eligible": True, "preference_eligible": False,
                                      "source_membership_eligible": False}}
        try:
            report_path.write_text(json.dumps(report), encoding="utf-8")
            records = validate(report, report_path)
            assert len(records) == 14 and all(r["record_type"] == "reader_requirement" for r in records)
            assert next(r for r in records if r["requirement_id"] == "Q7.element_anchors")["record_eligible"]
            blocked = json.loads(json.dumps(report))
            blocked["status"] = "blocked"
            blocked["scores"] = {"earned_points": None, "possible_points": None,
                                 "reader_answerability": None}
            blocked["training_export"]["reader_outcome_eligible"] = False
            assert len(validate(blocked, report_path)) == 14
            false_exhausted = json.loads(json.dumps(report))
            false_exhausted["budget"]["exhausted"] = True
            try:
                validate(false_exhausted, report_path)
                raise AssertionError("premature exhausted flag accepted")
            except ValidationError as exc:
                assert "budget.exhausted" in str(exc)
            reached = json.loads(json.dumps(report))
            reached["budget"]["max_distinct_views"] = 1
            reached["budget"]["exhausted"] = True
            assert len(validate(reached, report_path)) == 14
            bad_hash = json.loads(json.dumps(report))
            bad_hash["query_log"][0]["sha256"] = "0" * 64
            try:
                validate(bad_hash, report_path)
                raise AssertionError("corrupt hash accepted")
            except ValidationError as exc:
                assert "sha256 mismatch" in str(exc)
            bad_evidence = json.loads(json.dumps(report))
            bad_evidence["questions"][0]["criteria"][0]["evidence"][0]["json_pointer"] = "/elements/9/description"
            try:
                validate(bad_evidence, report_path)
                raise AssertionError("made-up evidence pointer accepted")
            except ValidationError as exc:
                assert "JSON pointer does not exist" in str(exc)
            bad_scores = json.loads(json.dumps(report))
            bad_scores["questions"][0]["points"] = 1
            try:
                validate(bad_scores, report_path)
                raise AssertionError("wrong score accepted")
            except ValidationError as exc:
                assert "points inconsistent" in str(exc)
            bad_percentage = json.loads(json.dumps(report))
            bad_percentage["scores"]["reader_answerability"] = 5 / 14
            try:
                validate(bad_percentage, report_path)
                raise AssertionError("fractional answerability accepted")
            except ValidationError as exc:
                assert "rounded percentage" in str(exc)
        except (ValidationError, AssertionError) as exc:
            print(f"self-test failed: {exc}", file=sys.stderr)
            return 1
    print("self-test passed (valid export, corrupt hash, bad pointer, wrong score)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
