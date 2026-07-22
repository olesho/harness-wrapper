import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from harness_chat import Client

BASE_KEYS = {"harness", "binary_path", "args", "working_dir", "env", "cols", "rows"}


class _Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        self.server.captured = json.loads(self.rfile.read(n))
        payload = b'{"id":"c1"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args):  # silence stderr access logs
        pass


class OpenBodyTest(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        self.server.captured = None
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        host, port = self.server.server_address
        self.client = Client(f"http://{host}:{port}")

    def test_defaults_omit_the_typed_knobs(self):
        self.client.open(harness="codex", binary_path="/usr/local/bin/codex")
        self.assertEqual(set(self.server.captured), BASE_KEYS)

    def test_effort_and_model_are_sent(self):
        self.client.open(
            harness="codex",
            binary_path="/usr/local/bin/codex",
            effort="high",
            model="gpt-5",
        )
        body = self.server.captured
        self.assertEqual(set(body), BASE_KEYS | {"effort", "model"})
        self.assertEqual(body["effort"], "high")
        self.assertEqual(body["model"], "gpt-5")

    def test_empty_effort_is_sent_not_dropped(self):
        self.client.open(harness="codex", binary_path="/x", effort="")
        self.assertEqual(self.server.captured["effort"], "")

    def test_permission_mode_is_sent(self):
        self.client.open(
            harness="claude-code",
            binary_path="/usr/local/bin/claude",
            permission_mode="plan",
        )
        body = self.server.captured
        self.assertEqual(set(body), BASE_KEYS | {"permission_mode"})
        self.assertEqual(body["permission_mode"], "plan")

    def test_empty_permission_mode_is_sent_not_dropped(self):
        # Presence, not truthiness: "" is a server-side no-op, but a falsy
        # check here would silently drop the key instead of sending it.
        self.client.open(harness="codex", binary_path="/x", permission_mode="")
        self.assertEqual(self.server.captured["permission_mode"], "")


if __name__ == "__main__":
    unittest.main()
