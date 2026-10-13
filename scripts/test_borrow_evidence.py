import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("evidence", Path(__file__).with_name("analyze-borrow-evidence.py"))
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


class EvidenceTests(unittest.TestCase):
    def test_missing_traces_and_observed_routes_cannot_claim_actual_route(self):
        records = [{"index": 1, "observed_routes": [{"id": "r"}], "quality": {"verdict": "request_failed"}},
                   {"index": 2, "attempts": [{"kind": "egress", "borrow_route_id": "r", "events": {}}]}]
        self.assertEqual([1, 2], evidence.quality_groups(records)["missing_or_multiple_actual_routes"])

    def test_raw_and_normalized_egress_are_grouped_but_retry_routes_are_not(self):
        records = []
        for index, attempt in enumerate([
            {"kind": "egress", "borrow_route_id": "r", "events": {"request_written": 1}},
            {"route_id": "r", "sent_at": 1, "events": {"request_written": 1}},
        ]):
            records.append({"index": index, "attempts": [attempt], "model": "m", "effort": "low", "type_id": 1})
        report = evidence.quality_groups(records)
        self.assertEqual(2, len(report["same_route_model_effort_question"][0]["samples"]))
        records[0]["attempts"].append({"kind": "egress", "borrow_route_id": "another", "events": {"request_written": 1}})
        self.assertEqual([0], evidence.quality_groups(records)["missing_or_multiple_actual_routes"])

    def test_future_result_is_not_a_previous_failure_and_duplicate_is_not_counted(self):
        events = []
        for cid, prepared, ended in [("slow", "00:00:00", "00:00:20"), ("fast", "00:00:05", "00:00:10")]:
            events.append({"stage": "target", "target_id": 1, "reason": "target_ticket_changed",
                           "created_at": "2026-10-13T" + ended + "+00:00",
                           "details": {"candidate_id": cid, "gateway": "g", "verification_prepared_at": "2026-10-13T" + prepared + "+00:00"}})
        report = evidence.gateway_history(events + [events[0]])
        self.assertEqual(2, report["unique_target_candidate_verdicts"])
        self.assertEqual({"no_observed_recent_verdict": {"target_ticket_changed": 2}}, report["previous_completed_verdict_within_120s"])

    def test_missing_phase_events_are_not_zero_latency_and_cancellation_is_separate(self):
        rows = [{"id": "1", "phase": "mint", "details": {"diagnostic": {"events": {"slot_acquired": 3000}, "duration_ms": 9000}}},
                {"id": "2", "phase": "mint", "details": {"diagnostic": {"cancellation_cause": "batch_success", "events": {"slot_acquired": 100}}}}]
        result = evidence.phase_costs(rows + [rows[0]])
        self.assertEqual({"mint/noncancelled": 1, "mint/cancelled": 1}, result["counts"])
        self.assertNotIn("body_ms", result["durations"]["mint/noncancelled"])
        self.assertEqual(3000, result["durations"]["mint/noncancelled"]["slot_wait_ms"]["median"])


if __name__ == "__main__":
    unittest.main()
