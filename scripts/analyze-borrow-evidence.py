#!/usr/bin/env python3
"""Read immutable borrowing snapshots; never call the service or launch a test.

Usage: analyze-borrow-evidence.py SNAPSHOT --records RECORDS --output NEW_FILE
The output uses exclusive creation so previous evidence cannot be overwritten.
"""
import argparse
from collections import Counter, defaultdict
from datetime import datetime
import json
import math
from pathlib import Path
import statistics


VERDICTS = {
    "target_probe_passed", "target_ticket_changed", "target_route_changed",
    "target_probe_inconclusive", "target_continue_timeout",
}


def read(path):
    return json.loads(path.read_text())


def stamp(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def distribution(values):
    values = sorted(values)
    if not values:
        return {"n": 0}
    return {"n": len(values), "median": statistics.median(values),
            "p95": values[math.ceil(len(values) * .95) - 1],
            "total": sum(values)}


def phase_costs(probes):
    groups = defaultdict(lambda: defaultdict(list))
    counts = Counter()
    seen = set()
    for row in probes:
        if row["id"] in seen:
            continue
        seen.add(row["id"])
        d = row.get("details", {}).get("diagnostic")
        if not d:
            counts["missing_diagnostic"] += 1
            continue
        phase = row.get("phase", "unknown")
        canceled = d.get("cancellation_cause") in {
            "user_stop", "batch_success", "service_shutdown", "target_lease_lost"
        }
        group = phase + ("/cancelled" if canceled else "/noncancelled")
        counts[group] += 1
        events = d.get("events", {})
        # Each duration has its own coverage count; missing events are unknown.
        segments = {
            "slot_wait_ms": (0, events.get("slot_acquired")),
            "db_admission_ms": (events.get("slot_acquired"), events.get("admitted")),
            "connect_ms": (events.get("admitted"), events.get("connection_ready")),
            "headers_wait_ms": (events.get("request_written"), events.get("response_headers")),
            "body_ms": (events.get("response_headers"), d.get("duration_ms")),
        }
        for name, (start, end) in segments.items():
            if start is not None and end is not None and end >= start:
                groups[group][name].append(end - start)
    return {"counts": dict(counts), "durations": {
        group: {name: distribution(values) for name, values in costs.items()}
        for group, costs in groups.items()}}


def gateway_history(events):
    unique = {}
    candidates = {}
    for event in events:
        d = event.get("details") or {}
        cid = d.get("candidate_id")
        if cid and d.get("cookie_hash") and event.get("reason") == "source_cookie_acquired":
            candidates[cid] = d
        if cid and event.get("stage") == "target" and event.get("reason") in VERDICTS:
            unique.setdefault((event["target_id"], cid), event)
    # Record previous *completed* verdict at probe start, never use future results.
    history = defaultdict(list)
    for event in unique.values():
        d = event["details"]
        history[(event["target_id"], d.get("gateway", ""), d.get("target_revision"),
                 d.get("target_identity_hash"))].append(event)
    strata = defaultdict(Counter)
    selection = Counter()
    for rows in history.values():
        rows.sort(key=lambda e: stamp(e["created_at"]))
        for event in rows:
            d = event["details"]
            at = d.get("verification_prepared_at")
            prior = [p for p in rows if at and 0 <= stamp(at) - stamp(p["created_at"]) <= 120]
            label = "no_observed_recent_verdict"
            if prior:
                label = "recent_success" if prior[-1]["reason"] == "target_probe_passed" else "recent_nonsuccess"
            strata[label][event["reason"]] += 1
            audit = d.get("gateway_selection")
            if audit is None:
                selection["missing_selection_snapshot"] += 1
            else:
                selection["multiple_gateways" if audit["gateways"] > 1 else "at_most_one_gateway"] += 1
            selection["priority/" + d.get("gateway_priority", "unknown")] += 1
    diversity = {}
    for label, get in {
        "cookies": lambda d: d.get("cookie_hash"),
        "gateway_names": lambda d: d.get("gateway"),
        "proxy_configurations": lambda d: d.get("source", {}).get("configured_proxy_hash"),
        "fingerprints": lambda d: d.get("source", {}).get("profile_hash"),
    }.items():
        diversity[label] = len({get(d) for d in candidates.values() if get(d)})
    return {"unique_target_candidate_verdicts": len(unique), "diversity": diversity,
            "previous_completed_verdict_within_120s": dict(strata), "selection_coverage": dict(selection)}


def quality_groups(records):
    groups = defaultdict(list)
    missing = []
    totals = Counter()
    for row in records:
        verdict = (row.get("quality") or {}).get("verdict", "ungraded")
        totals[verdict] += 1
        # Only actual egress can attribute a request. observed_routes is a
        # pre-dispatch view and must never fill a missing route.
        attempts = [a for a in row.get("attempts", []) if
                    (a.get("kind") == "egress" or (a.get("route_id") and a.get("sent_at") is not None)) and
                    "request_written" in a.get("events", {})]
        routes = {a.get("borrow_route_id", a.get("route_id")) for a in attempts}
        routes.discard(None)
        if len(routes) != 1:
            missing.append(row["index"])
            continue
        key = (next(iter(routes)), row.get("model"), row.get("effort"), row.get("type_id", row.get("kind")))
        groups[key].append({"request": row["index"], "verdict": verdict,
                            "route_age_s": row.get("route_age_s"),
                            "admission_remaining_s": row.get("remaining_s"),
                            "ttft_s": row.get("ttft_seconds")})
    return {"totals": dict(totals), "missing_or_multiple_actual_routes": missing,
            "same_route_model_effort_question": [
                {"route": key[0], "model": key[1], "effort": key[2], "question_type": key[3], "samples": rows}
                for key, rows in groups.items() if len(rows) > 1]}


def analyze(snapshot, records):
    # Failed requests may have no usage row. Join the saved operations errors by
    # request trace, retaining only their actually recorded attempts.
    error_file = snapshot / "business-errors.json"
    errors = read(error_file) if error_file.exists() else []
    by_trace = defaultdict(list)
    for error in errors:
        for event in error.get("upstream_errors") or []:
            if event and event.get("attempt"):
                by_trace[error.get("request_id")].append(event["attempt"])
    records = [dict(r, attempts=r.get("attempts") or by_trace.get(r.get("request_trace"), [])) for r in records]
    return {
        "gateway": gateway_history(read(snapshot / "events.json")),
        "probe_cost": phase_costs(read(snapshot / "probes.json")),
        "quality": quality_groups(records),
        "interpretation": [
            "Historical strata are observational, not randomized strategy comparisons.",
            "Proxy configuration hashes do not identify public exit IPs.",
            "Repeated same-route samples are correlated; no quality TTL is inferred.",
            "Cancelled probes remain separate from noncancelled attempts and business failures.",
            "An on/off rolling comparison needs interleaved equal-duration runs; this report does not infer it.",
        ],
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("snapshot", type=Path)
    parser.add_argument("--records", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = analyze(args.snapshot, read(args.records))
    with args.output.open("x") as file:
        json.dump(result, file, ensure_ascii=False, indent=2)
        file.write("\n")
