#!/usr/bin/env python3
"""List the tool names an MCP server publishes over Streamable HTTP. Standard library only.

    python3 scripts/mcp-list.py http://127.0.0.1:8080/mcp
"""
import json
import sys
import urllib.request


def post(url, payload, session=None):
    headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
    if session:
        headers["Mcp-Session-Id"] = session
    req = urllib.request.Request(url, json.dumps(payload).encode(), headers)
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = resp.read().decode()
        session = resp.headers.get("Mcp-Session-Id") or session
    # The server answers with JSON or with one server-sent event.
    for line in body.splitlines():
        if line.startswith("data:"):
            body = line[5:].strip()
            break
    return (json.loads(body) if body.strip() else {}), session


def main(url):
    _, session = post(url, {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
        "protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "mcp-list", "version": "1"}}})
    post(url, {"jsonrpc": "2.0", "method": "notifications/initialized"}, session)
    result, _ = post(url, {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, session)
    for tool in sorted(t["name"] for t in result["result"]["tools"]):
        print(tool)


if __name__ == "__main__":
    main(sys.argv[1])
