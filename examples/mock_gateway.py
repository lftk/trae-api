"""Local synthetic Chat gateway fixture. It does not contact Trae or execute tools."""
import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class MockGateway(BaseHTTPRequestHandler):
    def log_message(self, format, *args):
        pass  # Do not log request payloads, URLs or authorization headers.

    def send_json(self, status, data):
        body = json.dumps(data, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_json(404, {"error": {"message": "not found"}})
            return
        if self.headers.get("Authorization", "") != "Bearer " + self.server.expected_token:
            self.send_json(401, {"error": {"message": "invalid API token"}})
            return
        if self.server.logged_out:
            self.send_json(401, {"error": {"message": "login required"}})
            return
        try:
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            messages = body["messages"]
            turn = len(messages)
            if body.get("model") != "auto" or turn not in (1, 3, 5) or (turn == 3 and not body.get("tools")):
                raise ValueError("unexpected request")
            if turn == 5:
                if (messages[1].get("role"), messages[2].get("role"), messages[3].get("role"), messages[4].get("role")) != ("assistant", "user", "assistant", "tool"):
                    raise ValueError("history incomplete")
                calls = messages[3]["tool_calls"]
                if len(calls) != 1 or messages[4]["tool_call_id"] != calls[0]["id"]:
                    raise ValueError("tool ID mismatch")
                if "未执行真实工具" not in messages[4]["content"]:
                    raise ValueError("not a synthetic tool result")
            self.server.requests.append(body)
        except (ValueError, KeyError, TypeError, IndexError):
            self.send_json(400, {"error": {"message": "invalid mock request"}})
            return
        if turn == 3:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            deltas = [
                {"role": "assistant"},
                {"reasoning_content": "模拟推理片段"},
                {"tool_calls": [{"index": 0, "id": "call_demo", "type": "function", "function": {"name": "sample_lookup", "arguments": '{"key":'}}]},
                {"tool_calls": [{"index": 0, "function": {"arguments": '"demo"}'}}]},
            ]
            for delta in deltas:
                self.wfile.write(("data: " + json.dumps({"choices": [{"index": 0, "delta": delta, "finish_reason": None}]}, ensure_ascii=False) + "\n\n").encode("utf-8"))
                self.wfile.flush()
            if self.server.disconnect:
                return  # Intentional incomplete stream, no finish or [DONE].
            self.wfile.write(b'data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}\n\ndata: [DONE]\n\n')
            self.wfile.flush()
        else:
            answer = "模拟第一轮回答" if turn == 1 else "模拟最终回答"
            self.send_json(200, {"id": "chatcmpl-mock", "object": "chat.completion", "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": answer, "reasoning_content": "模拟推理，仅供输出"}}]})


def main():
    parser = argparse.ArgumentParser(description="127.0.0.1-only synthetic gateway")
    parser.add_argument("--port", type=int, default=18723)
    parser.add_argument("--token", default="synthetic-token")
    parser.add_argument("--logged-out", action="store_true")
    parser.add_argument("--disconnect", action="store_true")
    args = parser.parse_args()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), MockGateway)
    server.expected_token = args.token
    server.logged_out = args.logged_out
    server.disconnect = args.disconnect
    server.requests = []
    print("mock gateway listening on http://127.0.0.1:{}/v1".format(server.server_port), flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
