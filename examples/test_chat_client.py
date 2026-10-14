"""Run with: python3 -m unittest discover -s examples -v"""
import contextlib
import io
import json
import threading
import unittest
from http.server import ThreadingHTTPServer

from chat_client import GatewayClient, GatewayError, run_demo
from mock_gateway import MockGateway


class ClientDemoTests(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), MockGateway)
        self.server.expected_token = "synthetic-token"
        self.server.logged_out = False
        self.server.disconnect = False
        self.server.requests = []
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.client = GatewayClient(f"http://127.0.0.1:{self.server.server_port}/v1", "synthetic-token", timeout=2)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def test_demo_round_trip_replays_complete_history_without_executing_tool(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            run_demo(self.client)
        requests = self.server.requests
        self.assertEqual(len(requests), 3)
        self.assertEqual([r["stream"] for r in requests], [False, True, False])
        self.assertEqual([len(r["messages"]) for r in requests], [1, 3, 5])
        calls = requests[2]["messages"][-2]["tool_calls"]
        self.assertEqual(calls[0]["id"], requests[2]["messages"][-1]["tool_call_id"])
        self.assertIn("未执行真实工具", requests[2]["messages"][-1]["content"])
        self.assertEqual([r["model"] for r in requests], ["auto"] * 3)
        self.assertIn("reasoning_content", output.getvalue())
        self.assertIn("final", output.getvalue())

    def test_rejects_nonlocal_or_credentialed_gateway_url(self):
        for url in ("http://127.0.0.1:8723@evil.example/v1", "http://example.com/v1", "http://localhost:8723/v1?x=1"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                GatewayClient(url)

    def test_bad_gateway_token_returns_status_without_leaking_token(self):
        client = GatewayClient(self.client.base_url, "wrong-synthetic-secret", timeout=2)
        with self.assertRaisesRegex(GatewayError, "HTTP 401") as caught:
            run_demo(client)
        self.assertNotIn("wrong-synthetic-secret", str(caught.exception))
        self.assertEqual(self.server.requests, [])

    def test_login_failure_has_actionable_error(self):
        self.server.logged_out = True
        with self.assertRaisesRegex(GatewayError, "login"):
            run_demo(self.client)

    def test_stream_disconnect_is_not_a_successful_completion(self):
        self.server.disconnect = True
        with contextlib.redirect_stdout(io.StringIO()), self.assertRaisesRegex(GatewayError, "stream ended without"):
            run_demo(self.client)
        self.assertEqual(len(self.server.requests), 2)


if __name__ == "__main__":
    unittest.main()
