"""Test verifier fail-closed guards; these are not native mobility acceptance."""
import copy
import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("mobility_extended", Path(__file__).with_name("extended.py"))
EXT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EXT)


class GuardTests(unittest.TestCase):
    def test_native_completion_uses_persisted_ids_not_provider_request_ordinals(self):
        events = [{"type": "response_item", "payload": {"type": "custom_tool_call"}},
                  {"type": "event_msg", "payload": {"type": "task_started", "turn_id": "turn-alpha"}},
                  {"type": "event_msg", "payload": {"type": "task_complete", "turn_id": "turn-alpha"}},
                  {"type": "response_item", "payload": {"type": "custom_tool_call_output"}},
                  {"type": "event_msg", "payload": {"type": "task_complete", "turn_id": "turn-beta"}}]
        self.assertEqual(EXT.completed_turns(events, ["turn-alpha"], 2, "turn-beta"), ["turn-alpha", "turn-beta"])
        for prefix, count, boundary in [(["wrong-prefix"], 2, "turn-beta"), ([], 4, "turn-beta"),
                                        ([], 2, "turn-alpha")]:
            with self.assertRaisesRegex(RuntimeError, "completed turn IDs"):
                EXT.completed_turns(events, prefix, count, boundary)
        events[-1]["payload"]["turn_id"] = "turn-alpha"
        with self.assertRaisesRegex(RuntimeError, "completed turn IDs"):
            EXT.completed_turns(events, [], 2, "turn-alpha")

    def test_pending_move_requires_retained_exact_source_and_no_destination(self):
        active = {"launch": {"allocationId": "source-allocation"}, "incarnation": {"resourceUid": "source-uid"}}
        checkpoint = {"metadata": {"sha256": "completed-checkpoint"}}
        record = {"pendingOperation": "move-id", "phase": "stopping", "active": active, "checkpoint": checkpoint,
                  "operations": [{"id": "move-id", "stage": "stop", "complete": False}]}
        self.assertTrue(EXT.pending_source(record, active, checkpoint, "move-id"))
        self.assertFalse(EXT.pending_source(record, active, checkpoint, "different-move"))
        for changed in [{"active": {"incarnation": {"resourceUid": "replacement"}}},
                        {"checkpoint": {"metadata": {"sha256": "new-before-safe-boundary"}}},
                        {"operations": [{"id": "move-id", "stage": "prepare"}]},
                        {"operations": [{"id": "move-id", "stage": "stop", "launch": {"allocationId": "destination"}}]},
                        {"operations": [{"id": "move-id", "stage": "stop", "incarnation": {"resourceUid": "destination"}}]}]:
            malformed = copy.deepcopy(record)
            malformed.update(changed)
            with self.assertRaises(RuntimeError):
                EXT.pending_source(malformed, active, checkpoint, "move-id")


if __name__ == "__main__":
    unittest.main()
