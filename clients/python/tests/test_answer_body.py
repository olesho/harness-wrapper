import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from harness_chat import Client, Conversation, HarnessChatError

SCREEN = {"text": "hi", "cols": 80, "rows": 24, "cursor_col": 1, "cursor_row": 2, "generation": 7}


class _Handler(BaseHTTPRequestHandler):
    def do_POST(self):  # noqa: N802 (http.server API)
        n = int(self.headers.get("Content-Length", 0))
        self.server.captured = (self.path, json.loads(self.rfile.read(n)))
        self.send_response(204)
        self.end_headers()

    def do_GET(self):  # noqa: N802 (http.server API)
        self.server.captured = (self.path, None)
        payload = json.dumps(SCREEN).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args):
        pass


class AnswerAndScreenTest(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self.server.captured = None
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        host, port = self.server.server_address
        self.conv = Conversation(Client(f"http://{host}:{port}"), "c1")

    def test_answer_requires_control(self):
        with self.assertRaises(HarnessChatError):
            self.conv.answer("r1", option_id="proceed")
        self.assertIsNone(self.server.captured)

    def test_answer_posts_only_given_fields(self):
        self.conv._token = "tok"
        self.conv.answer("r1", option_id="proceed")
        path, body = self.server.captured
        self.assertEqual(path, "/v1/conversations/c1/input")
        self.assertEqual(body, {"token": "tok", "request_id": "r1", "option_id": "proceed"})

    def test_answer_multi_select_and_text(self):
        self.conv._token = "tok"
        self.conv.answer("r2", option_ids=["a", "b"])
        self.assertEqual(self.server.captured[1], {"token": "tok", "request_id": "r2", "option_ids": ["a", "b"]})
        self.conv.answer("", text="hello")
        self.assertEqual(self.server.captured[1], {"token": "tok", "text": "hello"})

    def test_screen_reads_the_snapshot(self):
        self.assertEqual(self.conv.screen(), SCREEN)
        self.assertEqual(self.server.captured[0], "/v1/conversations/c1/screen")


if __name__ == "__main__":
    unittest.main()
