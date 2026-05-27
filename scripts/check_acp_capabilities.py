#!/usr/bin/env python3
"""
Probe an ACP agent's initialize response and print supported capabilities.

Examples:
  python scripts/check_acp_capabilities.py --config config.yaml --agent codex
  python scripts/check_acp_capabilities.py -- zed-agent --mode stdio
"""

from __future__ import annotations

import argparse
import json
import queue
import subprocess
import sys
import threading
from pathlib import Path
from typing import Any


def load_agent_from_config(path: Path, agent_name: str) -> list[str]:
    try:
        import yaml  # type: ignore
    except ImportError:
        return load_agent_from_simple_yaml(path, agent_name)

    with path.open("r", encoding="utf-8") as f:
        cfg = yaml.safe_load(f) or {}

    for agent in cfg.get("agents", []):
        if agent.get("name") == agent_name:
            command = agent.get("command")
            args = agent.get("args") or []
            if not command:
                raise SystemExit(f"Agent {agent_name!r} has no command in {path}")
            return [command, *args]

    raise SystemExit(f"Agent {agent_name!r} not found in {path}")


def load_agent_from_simple_yaml(path: Path, agent_name: str) -> list[str]:
    """Small fallback parser for this repo's config.yaml shape."""
    current: dict[str, Any] | None = None
    agents: list[dict[str, Any]] = []

    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue

        if line.startswith("- name:"):
            current = {"name": parse_yaml_scalar(line.split(":", 1)[1].strip())}
            agents.append(current)
            continue

        if current is None:
            continue

        if line.startswith("command:"):
            current["command"] = parse_yaml_scalar(line.split(":", 1)[1].strip())
        elif line.startswith("args:"):
            current["args"] = parse_inline_list(line.split(":", 1)[1].strip())

    for agent in agents:
        if agent.get("name") == agent_name:
            command = agent.get("command")
            args = agent.get("args") or []
            if not command:
                raise SystemExit(f"Agent {agent_name!r} has no command in {path}")
            return [command, *args]

    raise SystemExit(
        f"Agent {agent_name!r} not found in {path}. Install PyYAML if your config is more complex."
    )


def parse_yaml_scalar(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value


def parse_inline_list(value: str) -> list[str]:
    if not value.startswith("[") or not value.endswith("]"):
        return []
    inner = value[1:-1].strip()
    if not inner:
        return []
    return [parse_yaml_scalar(part.strip()) for part in inner.split(",")]


def read_stdout(proc: subprocess.Popen[str], out: queue.Queue[dict[str, Any] | Exception]) -> None:
    assert proc.stdout is not None
    for line in proc.stdout:
        line = line.strip()
        if not line:
            continue
        try:
            out.put(json.loads(line))
        except json.JSONDecodeError as exc:
            out.put(RuntimeError(f"Invalid JSON from agent stdout: {line!r}: {exc}"))


def read_stderr(proc: subprocess.Popen[str]) -> None:
    assert proc.stderr is not None
    for line in proc.stderr:
        sys.stderr.write(f"[agent stderr] {line}")


def send_message(proc: subprocess.Popen[str], message: dict[str, Any]) -> None:
    assert proc.stdin is not None
    proc.stdin.write(json.dumps(message, separators=(",", ":")) + "\n")
    proc.stdin.flush()


def respond_to_agent_request(proc: subprocess.Popen[str], message: dict[str, Any]) -> None:
    method = message.get("method")
    msg_id = message.get("id")
    if msg_id is None:
        return

    if method == "session/requestPermission":
        result: dict[str, Any] = {"outcome": "approved"}
    else:
        result = {}

    send_message(proc, {"jsonrpc": "2.0", "id": msg_id, "result": result})


def initialize(proc: subprocess.Popen[str], timeout: float) -> dict[str, Any]:
    messages: queue.Queue[dict[str, Any] | Exception] = queue.Queue()
    threading.Thread(target=read_stdout, args=(proc, messages), daemon=True).start()
    threading.Thread(target=read_stderr, args=(proc,), daemon=True).start()

    request_id = 1
    send_message(
        proc,
        {
            "jsonrpc": "2.0",
            "id": request_id,
            "method": "initialize",
            "params": {
                "protocolVersion": 1,
                "clientCapabilities": {
                    "fs": {"readTextFile": True, "writeTextFile": True},
                    "terminal": True,
                },
                "clientInfo": {
                    "name": "acp-capability-checker",
                    "title": "ACP Capability Checker",
                    "version": "1.0.0",
                },
            },
        },
    )

    while True:
        if proc.poll() is not None:
            raise RuntimeError(f"Agent exited before initialize completed with code {proc.returncode}")

        try:
            message = messages.get(timeout=timeout)
        except queue.Empty as exc:
            raise TimeoutError(f"Timed out waiting {timeout:g}s for initialize response") from exc

        if isinstance(message, Exception):
            raise message

        if message.get("id") == request_id:
            if message.get("error"):
                raise RuntimeError(f"Initialize failed: {json.dumps(message['error'], ensure_ascii=False)}")
            result = message.get("result")
            if not isinstance(result, dict):
                raise RuntimeError(f"Initialize response result is not an object: {result!r}")
            return result

        if "method" in message:
            respond_to_agent_request(proc, message)


def flatten_capabilities(value: Any, prefix: str = "") -> list[tuple[str, Any]]:
    if isinstance(value, dict):
        rows: list[tuple[str, Any]] = []
        for key in sorted(value):
            name = f"{prefix}.{key}" if prefix else key
            rows.extend(flatten_capabilities(value[key], name))
        return rows
    return [(prefix, value)]


def print_report(result: dict[str, Any], raw_json: bool) -> None:
    if raw_json:
        print(json.dumps(result, indent=2, ensure_ascii=False))
        return

    info = result.get("agentInfo") or {}
    caps = result.get("agentCapabilities") or {}
    auth_methods = result.get("authMethods")

    print("ACP initialize result")
    print(f"  protocolVersion: {result.get('protocolVersion', '<missing>')}")

    if info:
        print("  agentInfo:")
        for key in ("name", "title", "version"):
            if key in info:
                print(f"    {key}: {info[key]}")

    print("  agentCapabilities:")
    if caps:
        for name, value in flatten_capabilities(caps):
            print(f"    {name}: {value}")
    else:
        print("    <none reported>")

    if auth_methods is not None:
        print("  authMethods:")
        if auth_methods:
            for method in auth_methods:
                print(f"    - {method}")
        else:
            print("    <none>")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Check ACP agent capabilities via initialize.")
    parser.add_argument("--config", type=Path, help="Path to config.yaml")
    parser.add_argument("--agent", help="Agent name in config.yaml")
    parser.add_argument("--timeout", type=float, default=10.0, help="Initialize timeout in seconds")
    parser.add_argument("--json", action="store_true", help="Print raw initialize result JSON")
    parser.add_argument("command", nargs=argparse.REMAINDER, help="Agent command after --")
    return parser.parse_args()


def main() -> int:
    args = parse_args()

    command = args.command
    if command and command[0] == "--":
        command = command[1:]

    if args.config or args.agent:
        if not args.config or not args.agent:
            raise SystemExit("--config and --agent must be used together")
        command = load_agent_from_config(args.config, args.agent)

    if not command:
        raise SystemExit("Provide --config config.yaml --agent NAME, or pass an agent command after --")

    try:
        proc = subprocess.Popen(
            command,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            encoding="utf-8",
            bufsize=1,
        )
    except OSError as exc:
        raise SystemExit(f"Failed to start agent command {command!r}: {exc}") from exc

    try:
        result = initialize(proc, args.timeout)
        print_report(result, args.json)
        return 0
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                proc.kill()


if __name__ == "__main__":
    raise SystemExit(main())
