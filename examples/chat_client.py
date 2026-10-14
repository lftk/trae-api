"""Offline OpenAI Chat-compatible HTTP client demo; stdlib only, no tool execution."""
import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request


class GatewayError(Exception):
    pass


class GatewayClient:
    def __init__(self, base_url, token="", timeout=10):
        parsed = urllib.parse.urlsplit(base_url)
        if (parsed.scheme != "http" or parsed.hostname not in ("127.0.0.1", "localhost")
                or parsed.port is None or parsed.username is not None or parsed.password is not None
                or parsed.path != "/v1" or parsed.query or parsed.fragment):
            raise ValueError("demo only accepts http://127.0.0.1:<port>/v1 or localhost equivalent")
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def chat(self, messages, stream=False, tools=None, model="auto"):
        body = {"model": model, "messages": messages, "stream": stream}
        if tools is not None:
            body["tools"] = tools
        request = urllib.request.Request(
            self.base_url + "/chat/completions",
            data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
            headers={"Content-Type": "application/json", **({"Authorization": "Bearer " + self.token} if self.token else {})},
            method="POST",
        )
        # No proxy, redirect following or automatic retries for credential-bearing requests.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect)
        try:
            with opener.open(request, timeout=self.timeout) as response:
                if stream:
                    return self._read_stream(response)
                return json.load(response)["choices"][0]["message"]
        except urllib.error.HTTPError as exc:
            # Never print response bodies: upstream may echo messages/authorization data.
            exc.close()
            if exc.code == 401:
                raise GatewayError("HTTP 401: check gateway token or run trae-api login") from None
            raise GatewayError("HTTP {} from gateway".format(exc.code)) from None
        except (urllib.error.URLError, OSError, ValueError, KeyError, IndexError) as exc:
            raise GatewayError("gateway connection/response failed ({})".format(type(exc).__name__)) from None

    @staticmethod
    def _read_stream(response):
        text = ""
        reasoning = ""
        calls = {}
        finished = False
        done = False
        while True:
            raw = response.readline()
            if not raw:
                break
            line = raw.decode("utf-8").strip()
            if line == "data: [DONE]":
                done = True
                break
            if not line.startswith("data: "):
                continue  # SSE comment/heartbeat and blank line
            event = json.loads(line[6:])
            if "error" in event:
                raise GatewayError("gateway reported an in-stream error (partial output discarded)")
            for choice in event.get("choices", []):
                delta = choice.get("delta", {})
                text += delta.get("content") or ""
                reasoning += delta.get("reasoning_content") or ""
                for call in delta.get("tool_calls", []):
                    index = call["index"]
                    value = calls.setdefault(index, {"id": "", "type": "function", "function": {"name": "", "arguments": ""}})
                    value["id"] += call.get("id") or ""
                    fn = call.get("function") or {}
                    value["function"]["name"] += fn.get("name") or ""
                    value["function"]["arguments"] += fn.get("arguments") or ""
                if choice.get("finish_reason"):
                    finished = True
        if not done or not finished:
            raise GatewayError("stream ended without [DONE] or finish_reason (partial output discarded)")
        message = {"role": "assistant", "content": text}
        if reasoning:
            message["reasoning_content"] = reasoning
        if calls:
            message["tool_calls"] = [calls[index] for index in sorted(calls)]
        return message


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def run_demo(client):
    """Two Chat turns and a client-supplied *synthetic* tool result."""
    history = [{"role": "user", "content": "请解释这个离线演示。"}]
    first = client.chat(history)
    print(json.dumps({"step": "nonstream", "answer": first.get("content"),
                      "reasoning_content": first.get("reasoning_content")}, ensure_ascii=False))
    # reasoning_content is output-only: do not replay it as assistant input.
    history.append({"role": "assistant", "content": first["content"]})
    history.append({"role": "user", "content": "请调用 sample_lookup 演示工具协议。"})
    tools = [{"type": "function", "function": {"name": "sample_lookup", "description": "离线协议样例，不执行", "parameters": {"type": "object", "properties": {"key": {"type": "string"}}, "required": ["key"]}}}]
    second = client.chat(history, stream=True, tools=tools)
    print(json.dumps({"step": "stream", "reasoning_content": second.get("reasoning_content"),
                      "tool_calls": second.get("tool_calls", [])}, ensure_ascii=False))
    calls = second.get("tool_calls", [])
    if not calls or any(not c["id"] or c["function"]["name"] != "sample_lookup" for c in calls):
        raise GatewayError("mock did not return expected tool calls; no tool was executed")
    # Preserve the assistant tool_calls + every matching tool result in full history.
    history.append({"role": "assistant", "content": second["content"], "tool_calls": calls})
    for call in calls:
        history.append({"role": "tool", "tool_call_id": call["id"],
                        "content": json.dumps({"note": "未执行真实工具；这是固定的离线模拟结果"}, ensure_ascii=False)})
    final = client.chat(history, tools=tools)
    print(json.dumps({"step": "final", "answer": final.get("content")}, ensure_ascii=False))


def main():
    parser = argparse.ArgumentParser(description="Local-only, no external model or tool calls")
    parser.add_argument("--base-url", default="http://127.0.0.1:8723/v1")
    parser.add_argument("--token", default=os.environ.get("TRAE_API_TOKEN", ""), help="gateway token (prefer TRAE_API_TOKEN environment variable)")
    args = parser.parse_args()
    try:
        run_demo(GatewayClient(args.base_url, args.token))
    except (GatewayError, ValueError) as exc:
        print("demo failed: {}".format(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
