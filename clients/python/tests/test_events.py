import unittest

from harness_chat import TurnEvent


class TurnEventTest(unittest.TestCase):
    def test_input_request_has_no_turn(self):
        ev = TurnEvent.from_json({"type": "input_request", "input": {"id": "i1"}})
        self.assertEqual(ev.type, "input_request")
        self.assertIsNone(ev.turn)
        self.assertEqual(ev.input["id"], "i1")

    def test_turn_frame(self):
        ev = TurnEvent.from_json({"type": "turn", "turn": {"id": "t1"}})
        self.assertEqual(ev.turn.id, "t1")


if __name__ == "__main__":
    unittest.main()
