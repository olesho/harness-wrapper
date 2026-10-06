import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

from harness_chat import Client, Conversation


class _Handler(BaseHTTPRequestHandler):
    hits = 0

    def do_GET(self):  # noqa: N802 (http.server API)
        type(self).hits += 1
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        self.wfile.write(b'data: {"type":"turn","turn":{"id":"t1","state":"complete"}}\n\n')

    def log_message(self, *_args):
        pass


class EventsSubscribeTest(unittest.TestCase):
    def test_events_connects_before_iteration(self):
        # The server does not replay events, so events() must subscribe when
        # called, letting callers open the stream before send().
        server = HTTPServer(("127.0.0.1", 0), _Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            conv = Conversation(Client(f"http://127.0.0.1:{server.server_port}"), "c1")
            events = conv.events()
            self.assertEqual(_Handler.hits, 1, "events() did not connect until iterated")
            first = next(iter(events))
            self.assertIsNotNone(first.turn)
            self.assertEqual(first.turn.id, "t1")
        finally:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    unittest.main()
