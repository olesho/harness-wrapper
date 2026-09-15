import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from harness_chat import Client, Containment, Conversation, HarnessChatError

APPLIED = {"kind": "landlock", "fingerprint": "sha256:abc", "supervision": {"mode": "cgroup"}}


class _Handler(BaseHTTPRequestHandler):
    def _reply(self, status, payload):
        raw = json.dumps(payload).encode() if payload is not None else b""
        self.send_response(status)
        if raw:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        self.server.seen.append(f"GET {self.path}")
        if self.path == "/v1/capabilities":
            caps = self.server.caps
            if caps is None:
                return self._reply(404, {"error": "not found"})
            return self._reply(200, caps)
        self._reply(404, {"error": "not found"})

    def do_POST(self):
        self.server.seen.append(f"POST {self.path}")
        n = int(self.headers.get("Content-Length", 0))
        self.server.bodies.append(json.loads(self.rfile.read(n) or b"null"))
        if self.path.endswith("/control"):
            return self._reply(200, {"token": "t1"})
        if self.path.endswith("/messages"):
            return self._reply(202, {"turn_id": "turn1"})
        self._reply(*self.server.open_reply)

    def do_DELETE(self):
        self.server.seen.append(f"DELETE {self.path}")
        self._reply(204, None)

    def log_message(self, *_args):
        pass


class ContainmentTest(unittest.TestCase):
    def serve(self, caps, open_reply=(201, {"id": "c1", "containment": APPLIED})):
        srv = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        srv.caps, srv.open_reply, srv.seen, srv.bodies = caps, open_reply, [], []
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        host, port = srv.server_address
        return srv, Client(f"http://{host}:{port}")

    LANDLOCK = {"containment": {"kinds": ["landlock"]}}

    def test_complete_object_with_unset_fields_omitted(self):
        srv, client = self.serve(self.LANDLOCK)
        conv = client.open(
            harness="claude-code",
            binary_path="/bin/claude",
            containment=Containment(read_write=["/w"], restrict_tcp=True, connect_tcp=[443]),
        )
        self.assertEqual(srv.seen, ["GET /v1/capabilities", "POST /v1/conversations"])
        self.assertEqual(
            srv.bodies[0]["containment"],
            {"kind": "landlock", "read_write": ["/w"], "restrict_tcp": True, "connect_tcp": [443]},
        )
        self.assertEqual(conv.containment["fingerprint"], "sha256:abc")

    def test_deny_all_tcp_keeps_an_empty_port_list(self):
        srv, client = self.serve(self.LANDLOCK)
        client.open(harness="codex", binary_path="/x", containment=Containment(restrict_tcp=True, connect_tcp=[]))
        self.assertEqual(srv.bodies[0]["containment"], {"kind": "landlock", "restrict_tcp": True, "connect_tcp": []})

    def test_ports_without_restriction_are_sent_as_written(self):
        srv, client = self.serve(self.LANDLOCK, (400, {"error": "bad", "code": "invalid_config"}))
        with self.assertRaises(HarnessChatError) as cm:
            client.open(harness="codex", binary_path="/x", containment=Containment(connect_tcp=[443]))
        self.assertEqual(cm.exception.code, "invalid_config")
        self.assertEqual(srv.bodies[0]["containment"], {"kind": "landlock", "connect_tcp": [443]})

    def test_old_server_never_receives_containment(self):
        srv, client = self.serve(None)
        with self.assertRaises(HarnessChatError) as cm:
            client.open(harness="codex", binary_path="/x", containment=Containment())
        self.assertEqual(cm.exception.code, "containment_unsupported")
        self.assertEqual(srv.seen, ["GET /v1/capabilities"])

    def test_server_listing_no_kind_never_receives_containment(self):
        srv, client = self.serve({"containment": {"kinds": []}})
        with self.assertRaises(HarnessChatError) as cm:
            client.open(harness="codex", binary_path="/x", containment=Containment())
        self.assertEqual(cm.exception.code, "containment_unsupported")
        self.assertEqual(srv.seen, ["GET /v1/capabilities"])

    def test_missing_echo_is_refused_and_the_conversation_closed(self):
        srv, client = self.serve(self.LANDLOCK, (201, {"id": "c1"}))
        with self.assertRaises(HarnessChatError) as cm:
            client.open(harness="codex", binary_path="/x", containment=Containment())
        self.assertEqual(cm.exception.code, "containment_not_applied")
        self.assertEqual(srv.seen, ["GET /v1/capabilities", "POST /v1/conversations", "DELETE /v1/conversations/c1"])

    def test_uncontained_open_skips_capabilities(self):
        srv, client = self.serve(self.LANDLOCK, (201, {"id": "c1"}))
        conv = client.open(harness="codex", binary_path="/x")
        self.assertEqual(srv.seen, ["POST /v1/conversations"])
        self.assertNotIn("containment", srv.bodies[0])
        self.assertIsNone(conv.containment)

    def test_send_restates_containment_only_when_given(self):
        srv, client = self.serve(self.LANDLOCK)
        conv = Conversation(client, "c1")
        conv.acquire()
        conv.send("hi")
        conv.send("again", containment=Containment(read_only=["/r"]))
        self.assertNotIn("containment", srv.bodies[1])
        self.assertEqual(srv.bodies[-1]["containment"], {"kind": "landlock", "read_only": ["/r"]})


if __name__ == "__main__":
    unittest.main()
