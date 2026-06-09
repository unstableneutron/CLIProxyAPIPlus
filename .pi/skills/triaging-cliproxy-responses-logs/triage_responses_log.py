#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# ///

"""Triage CLIProxyAPI /v1/responses logs.

This helper is intentionally dependency-free so it can run locally through uv
or inspect a remote CLIProxyAPI instance over ssh.
"""

from __future__ import annotations

import argparse
import datetime as dt
import glob
import json
import os
import re
import shlex
import sqlite3
import subprocess
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from typing import Any
from zoneinfo import ZoneInfo


def parse_time(value: str, timezone: str) -> dt.datetime:
    value = value.strip()
    if value.endswith("Z"):
        return dt.datetime.fromisoformat(value[:-1] + "+00:00")
    if "T" in value and re.search(r"[+-]\d\d:\d\d$", value):
        return dt.datetime.fromisoformat(value)
    value = value.replace("T", " ")
    for fmt in ("%Y-%m-%d %H:%M:%S.%f", "%Y-%m-%d %H:%M:%S", "%m/%d/%Y %H:%M:%S"):
        try:
            return dt.datetime.strptime(value, fmt).replace(tzinfo=ZoneInfo(timezone))
        except ValueError:
            pass
    raise ValueError(f"unsupported timestamp: {value!r}")


def iso_utc(value: dt.datetime) -> str:
    return value.astimezone(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def read_text(path: Path) -> str:
    return path.read_text(encoding="utf-8", errors="replace")


def redact(value: str) -> str:
    value = re.sub(r"(Authorization:\s*Bearer\s+)[^\s,}]+", r"\1<redacted>", value, flags=re.I)
    value = re.sub(r"(ChatGPT?-Account-ID:\s*)87[0-9A-Za-z._-]+", r"\g<1>87...", value, flags=re.I)
    value = re.sub(r"auth=[^\s]+", "auth=<redacted>", value)
    return value


def ssh_capture(host: str, command: str) -> str:
    proc = subprocess.run(["ssh", host, command], text=True, capture_output=True, check=False)
    if proc.returncode != 0:
        raise RuntimeError(proc.stderr.strip() or f"ssh command failed: {command}")
    return proc.stdout


def remote_cat(host: str, path: str) -> str:
    proc = subprocess.run(["ssh", host, "cat", path], text=True, capture_output=True, check=False)
    if proc.returncode != 0:
        raise RuntimeError(proc.stderr.strip() or f"remote cat failed: {path}")
    return proc.stdout


def query_usage_local(db_path: Path, model: str, around: dt.datetime, window: dt.timedelta) -> list[dict[str, Any]]:
    lo = around - window
    hi = around + window
    con = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    con.row_factory = sqlite3.Row
    rows = []
    for row in con.execute(
        """
        select id, request_id, timestamp, provider, model, requested_model, resolved_model,
               endpoint, method, path, input_tokens, output_tokens, reasoning_tokens,
               cached_tokens, total_tokens, latency_ms, failed, fail_status_code,
               fail_summary, executor_type
        from usage_events
        where model like ? or requested_model like ? or resolved_model like ?
        order by timestamp_ms
        """,
        (f"%{model}%", f"%{model}%", f"%{model}%"),
    ):
        ts = parse_time(row["timestamp"], "UTC")
        if lo <= ts.astimezone(lo.tzinfo) <= hi:
            rows.append(dict(row))
    return rows


def query_usage_remote(host: str, root: str, model: str, around: dt.datetime, window: dt.timedelta) -> list[dict[str, Any]]:
    payload = {
        "db": f"{root.rstrip('/')}/cpa-manager/usage.sqlite",
        "model": model,
        "lo": iso_utc(around - window),
        "hi": iso_utc(around + window),
    }
    code = r'''
import datetime as dt, json, os, sqlite3, sys
p=json.loads(sys.stdin.read())
def parse(v):
    return dt.datetime.fromisoformat(v.replace('Z','+00:00'))
lo=parse(p['lo']); hi=parse(p['hi'])
db=os.path.expanduser(p['db'])
con=sqlite3.connect('file:'+db+'?mode=ro', uri=True)
con.row_factory=sqlite3.Row
out=[]
for row in con.execute("""
select id, request_id, timestamp, provider, model, requested_model, resolved_model,
       endpoint, method, path, input_tokens, output_tokens, reasoning_tokens,
       cached_tokens, total_tokens, latency_ms, failed, fail_status_code,
       fail_summary, executor_type
from usage_events
where model like ? or requested_model like ? or resolved_model like ?
order by timestamp_ms
""", ('%'+p['model']+'%', '%'+p['model']+'%', '%'+p['model']+'%')):
    ts=parse(row['timestamp'])
    if lo <= ts <= hi:
        out.append(dict(row))
print(json.dumps(out))
'''
    proc = subprocess.run(
        ["ssh", host, "python3 -c " + shlex.quote(code)],
        input=json.dumps(payload),
        text=True,
        capture_output=True,
        check=False,
    )
    if proc.returncode != 0:
        raise RuntimeError(proc.stderr.strip() or "remote sqlite query failed")
    return json.loads(proc.stdout or "[]")


def find_log_local(logs_dir: Path, request_id: str) -> Path | None:
    matches = sorted(logs_dir.glob(f"v1-responses-*-{request_id}.log"))
    return matches[-1] if matches else None


def find_log_remote(host: str, root: str, request_id: str) -> str | None:
    cmd = f"ls -1 {root.rstrip('/')}/logs/v1-responses-*-{request_id}.log 2>/dev/null | tail -1"
    out = ssh_capture(host, cmd).strip()
    return out or None


def parse_turn_metadata(text: str) -> dict[str, Any]:
    m = re.search(r"X-Codex-Turn-Metadata:\s*(\{[^\n]+\})", text)
    if not m:
        return {}
    try:
        return json.loads(m.group(1))
    except json.JSONDecodeError:
        return {}


def parse_request_body_summary(text: str) -> dict[str, Any]:
    for line in text.splitlines():
        if not line.startswith("{") or '"type":"response.create"' not in line:
            continue
        try:
            body = json.loads(line)
        except json.JSONDecodeError:
            continue
        return {
            "type": body.get("type"),
            "model": body.get("model"),
            "input_items": len(body.get("input", [])) if isinstance(body.get("input"), list) else None,
            "generate": body.get("generate"),
            "stream": body.get("stream"),
            "prompt_cache_key": body.get("prompt_cache_key"),
            "tools": len(body.get("tools", [])) if isinstance(body.get("tools"), list) else None,
        }
    return {}


def parse_response_log(text: str) -> dict[str, Any]:
    type_counts: dict[str, int] = {}
    texts: list[dict[str, Any]] = []
    completions: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    current_ts = ""
    for raw_line in text.splitlines():
        if raw_line.startswith("Timestamp:"):
            current_ts = raw_line.split(":", 1)[1].strip()
            continue
        if not raw_line.startswith("{"):
            continue
        try:
            frame = json.loads(raw_line)
        except json.JSONDecodeError:
            continue
        typ = frame.get("type", "(unknown)")
        type_counts[typ] = type_counts.get(typ, 0) + 1
        if typ == "response.output_text.done":
            texts.append({"timestamp": current_ts, "text": frame.get("text", ""), "item_id": frame.get("item_id")})
        if typ == "response.completed":
            completions.append({"timestamp": current_ts, "frame": frame})
        if "error" in typ or typ == "response.failed" or frame.get("error"):
            errors.append({"timestamp": current_ts, "frame": frame})
    seen: set[str] = set()
    unique_texts = []
    for item in texts:
        if item["text"] in seen:
            continue
        seen.add(item["text"])
        unique_texts.append(item)
    last_completion = completions[-1]["frame"] if completions else {}
    response = last_completion.get("response", {}) if isinstance(last_completion.get("response"), dict) else {}
    stop_field = "absent"
    if "stop" in response:
        stop_field = response["stop"]
    elif "stop" in last_completion:
        stop_field = last_completion["stop"]
    return {
        "event_type_counts": type_counts,
        "output_text_count": len(texts),
        "unique_output_text_count": len(unique_texts),
        "final_text": unique_texts[-1]["text"] if unique_texts else "",
        "final_text_timestamp": unique_texts[-1]["timestamp"] if unique_texts else None,
        "completed_count": len(completions),
        "error_count": len(errors),
        "status": response.get("status", last_completion.get("status")),
        "stop_field": stop_field,
        "error": response.get("error", last_completion.get("error")),
        "incomplete_details": response.get("incomplete_details"),
        "usage": response.get("usage"),
    }


def parse_header_summary(text: str) -> dict[str, Any]:
    first_ts = re.search(r"Timestamp:\s*([^\n]+)", text)
    url = re.search(r"URL:\s*([^\n]+)", text)
    method = re.search(r"Method:\s*([^\n]+)", text)
    account = re.search(r"Chatgpt-Account-Id:\s*([^\n]+)", text, re.I)
    thread = re.search(r"Thread-Id:\s*([^\n]+)", text)
    session = re.search(r"Session-Id:\s*([^\n]+)", text)
    parent = re.search(r"X-Codex-Parent-Thread-Id:\s*([^\n]+)", text)
    meta = parse_turn_metadata(text)
    return {
        "request_timestamp": first_ts.group(1).strip() if first_ts else None,
        "url": url.group(1).strip() if url else None,
        "method": method.group(1).strip() if method else None,
        "account_id": account.group(1).strip() if account else None,
        "session_id": meta.get("session_id") or (session.group(1).strip() if session else None),
        "thread_id": meta.get("thread_id") or (thread.group(1).strip() if thread else None),
        "parent_thread_id": meta.get("parent_thread_id") or (parent.group(1).strip() if parent else None),
        "request_kind": meta.get("request_kind"),
        "thread_source": meta.get("thread_source"),
        "window_id": meta.get("window_id"),
        "workspaces": sorted((meta.get("workspaces") or {}).keys()),
    }


def load_log(args: argparse.Namespace, request_id: str) -> tuple[str, str]:
    if args.ssh_host:
        path = find_log_remote(args.ssh_host, args.root, request_id)
        if not path:
            raise SystemExit(f"no remote log found for request id {request_id}")
        return path, remote_cat(args.ssh_host, path)
    path = find_log_local(Path(args.logs_dir), request_id)
    if not path:
        raise SystemExit(f"no log found for request id {request_id}")
    return str(path), read_text(path)


def command_final(args: argparse.Namespace) -> None:
    around = parse_time(args.around, args.timezone).astimezone(dt.timezone.utc)
    window = dt.timedelta(minutes=args.window_minutes)
    rows = query_usage_remote(args.ssh_host, args.root, args.model, around, window) if args.ssh_host else query_usage_local(Path(args.usage_db), args.model, around, window)
    if not rows:
        raise SystemExit("no usage rows matched model/time window")
    row = rows[-1]
    log_path, text = load_log(args, row["request_id"])
    summary = parse_response_log(text)
    header = parse_header_summary(text)
    request_body = parse_request_body_summary(text)
    out = {"usage_row": row, "log_file": log_path, "header": header, "request_body": request_body, **summary}
    print(json.dumps(out, indent=2, ensure_ascii=False))


def iter_logs_local(logs_dir: Path) -> list[Path]:
    return sorted(logs_dir.glob("v1-responses*.log"))


def iter_logs_remote(host: str, root: str) -> list[str]:
    out = ssh_capture(host, f"find {root.rstrip('/')}/logs -maxdepth 1 -type f -name 'v1-responses*.log' -printf '%p\\n'")
    return [line for line in out.splitlines() if line]


def command_burst(args: argparse.Namespace) -> None:
    around = parse_time(args.around, args.timezone)
    window = dt.timedelta(minutes=args.window_minutes)
    rows = []
    paths = iter_logs_remote(args.ssh_host, args.root) if args.ssh_host else iter_logs_local(Path(args.logs_dir))
    for path in paths:
        text = remote_cat(args.ssh_host, str(path)) if args.ssh_host else read_text(Path(path))
        header = parse_header_summary(text)
        if not header.get("request_timestamp"):
            continue
        try:
            ts = parse_time(header["request_timestamp"], args.timezone)
        except ValueError:
            continue
        if not (around - window <= ts.astimezone(around.tzinfo) <= around + window):
            continue
        account = header.get("account_id") or ""
        if args.account_prefix and not account.startswith(args.account_prefix):
            continue
        usage = parse_response_log(text).get("usage") or {}
        body = parse_request_body_summary(text)
        request_id = re.search(r"-([0-9a-f]{8})\.log$", str(path))
        rows.append({
            "request_id": request_id.group(1) if request_id else None,
            "log_file": os.path.basename(str(path)),
            "timestamp": header.get("request_timestamp"),
            "request_kind": header.get("request_kind"),
            "thread_source": header.get("thread_source"),
            "session_id": header.get("session_id"),
            "thread_id": header.get("thread_id"),
            "parent_thread_id": header.get("parent_thread_id"),
            "input_items": body.get("input_items"),
            "generate": body.get("generate"),
            "total_tokens": usage.get("total_tokens"),
            "input_tokens": usage.get("input_tokens"),
            "cached_tokens": (usage.get("input_tokens_details") or {}).get("cached_tokens") if isinstance(usage.get("input_tokens_details"), dict) else None,
            "output_tokens": usage.get("output_tokens"),
        })
    print(json.dumps(rows, indent=2, ensure_ascii=False))


class HelperTests(unittest.TestCase):
    def test_final_dedupes_and_reports_absent_stop(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            logs = root / "logs"
            logs.mkdir()
            db = root / "usage.sqlite"
            con = sqlite3.connect(db)
            con.execute(
                "create table usage_events (id integer, request_id text, timestamp text, provider text, model text, requested_model text, resolved_model text, endpoint text, method text, path text, input_tokens integer, output_tokens integer, reasoning_tokens integer, cached_tokens integer, total_tokens integer, latency_ms integer, failed integer, fail_status_code integer, fail_summary text, executor_type text, timestamp_ms integer)"
            )
            con.execute(
                "insert into usage_events values (1,'deadbeef','2026-06-07T10:47:25Z','mixed','gpt-5.5-nomoderation',null,null,'GET /v1/responses','GET','/v1/responses',10,2,1,8,12,100,0,200,null,'CodexWebsocketsExecutor',1)"
            )
            con.commit()
            (logs / "v1-responses-2026-06-07T185236-deadbeef.log").write_text(
                textwrap.dedent(
                    '''
                    === REQUEST INFO ===
                    URL: /v1/responses
                    Method: GET
                    Timestamp: 2026-06-07T18:47:25+08:00
                    === HEADERS ===
                    Chatgpt-Account-Id: 8772
                    X-Codex-Turn-Metadata: {"session_id":"s","thread_id":"t","request_kind":"turn"}
                    Timestamp: 2026-06-07T18:47:36+08:00
                    {"type":"response.output_text.done","text":"final answer","item_id":"m1"}
                    Timestamp: 2026-06-07T18:47:36+08:00
                    {"type":"response.output_text.done","text":"final answer","item_id":"m1"}
                    Timestamp: 2026-06-07T18:47:36+08:00
                    {"type":"response.completed","response":{"status":"completed","error":null,"incomplete_details":null,"usage":{"input_tokens":10,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1},"input_tokens_details":{"cached_tokens":8},"total_tokens":12}}}
                    '''
                ).lstrip(),
                encoding="utf-8",
            )
            rows = query_usage_local(db, "gpt-5.5-nomoderation", parse_time("2026-06-07 17:47:25", "Asia/Bangkok").astimezone(dt.timezone.utc), dt.timedelta(minutes=1))
            self.assertEqual(rows[0]["request_id"], "deadbeef")
            parsed = parse_response_log(read_text(logs / "v1-responses-2026-06-07T185236-deadbeef.log"))
            self.assertEqual(parsed["final_text"], "final answer")
            self.assertEqual(parsed["unique_output_text_count"], 1)
            self.assertEqual(parsed["status"], "completed")
            self.assertEqual(parsed["stop_field"], "absent")
            self.assertIsNone(parsed["error"])

    def test_burst_identifies_prewarm_shape(self) -> None:
        log = textwrap.dedent(
            '''
            === REQUEST INFO ===
            URL: /v1/responses
            Method: GET
            Timestamp: 2026-06-07T18:48:23+08:00
            === HEADERS ===
            Chatgpt-Account-Id: 8772
            X-Codex-Turn-Metadata: {"session_id":"s","thread_id":"t","thread_source":"user","request_kind":"prewarm","window_id":"t:0"}
            {"type":"response.create","model":"gpt-5.5","input":[],"generate":false,"stream":true,"prompt_cache_key":"t","tools":[{"name":"exec"}]}
            {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":11638,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"total_tokens":11638}}}
            '''
        ).lstrip()
        header = parse_header_summary(log)
        body = parse_request_body_summary(log)
        usage = parse_response_log(log)["usage"]
        self.assertEqual(header["request_kind"], "prewarm")
        self.assertEqual(body["input_items"], 0)
        self.assertFalse(body["generate"])
        self.assertEqual(usage["total_tokens"], 11638)

    def test_redacts_sensitive_headers(self) -> None:
        raw = "Authorization: Bearer secret\nChatGPT-Account-ID: 8772abcdef\nauth=codex-file.json"
        safe = redact(raw)
        self.assertIn("Authorization: Bearer <redacted>", safe)
        self.assertIn("ChatGPT-Account-ID: 87...", safe)
        self.assertIn("auth=<redacted>", safe)
        self.assertNotIn("secret", safe)
        self.assertNotIn("8772abcdef", safe)


def run_self_test() -> None:
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(HelperTests)
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    raise SystemExit(0 if result.wasSuccessful() else 1)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true", help="run embedded tests")
    sub = parser.add_subparsers(dest="command")

    def common(p: argparse.ArgumentParser) -> None:
        p.add_argument("--ssh-host", help="remote ssh host, e.g. vn3")
        p.add_argument("--root", default="~/CLIProxyAPI", help="remote CLIProxyAPI root")
        p.add_argument("--logs-dir", default="logs", help="local logs directory")
        p.add_argument("--timezone", default="Asia/Bangkok", help="timezone for naive --around values")
        p.add_argument("--around", required=True, help="center timestamp")
        p.add_argument("--window-minutes", type=float, default=5.0)

    final = sub.add_parser("final", help="recover final assistant response for model/time")
    common(final)
    final.add_argument("--usage-db", default="cpa-manager/usage.sqlite", help="local usage sqlite path")
    final.add_argument("--model", required=True)
    final.set_defaults(func=command_final)

    burst = sub.add_parser("burst", help="summarize account-specific responses burst")
    common(burst)
    burst.add_argument("--account-prefix", default="")
    burst.set_defaults(func=command_burst)
    return parser


def main(argv: list[str] | None = None) -> None:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.self_test:
        run_self_test()
    if not args.command:
        parser.print_help()
        raise SystemExit(2)
    args.func(args)


if __name__ == "__main__":
    main()
