#!/usr/bin/env python3

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


AUTOMATIC_REVIEW_PREFIX = "AUTOMATIC REVIEW:"
MAX_REVISIONS = 3
REVIEW_TIMEOUT_SECONDS = 540


def read_payload():
    try:
        value = json.load(sys.stdin)
    except (json.JSONDecodeError, OSError):
        return {}
    return value if isinstance(value, dict) else {}


def emit(value):
    json.dump(value, sys.stdout, ensure_ascii=False)
    sys.stdout.write("\n")


def run_git(root, *args):
    return subprocess.run(
        ["git", *args],
        cwd=root,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    ).stdout


def find_root(payload):
    cwd = Path(payload.get("cwd") or Path.cwd()).resolve()
    try:
        return Path(run_git(cwd, "rev-parse", "--show-toplevel").decode().strip())
    except (OSError, subprocess.CalledProcessError):
        return Path(__file__).resolve().parent.parent


def changed_paths(root):
    paths = set()
    for args in (
        ("diff", "--name-only", "-z", "HEAD", "--"),
        ("ls-files", "--others", "--exclude-standard", "-z"),
    ):
        try:
            output = run_git(root, *args).decode(errors="replace")
        except (OSError, subprocess.CalledProcessError):
            continue
        paths.update(path for path in output.split("\0") if path)
    return sorted(paths)


def untracked_paths(root):
    try:
        output = run_git(root, "ls-files", "--others", "--exclude-standard", "-z")
    except (OSError, subprocess.CalledProcessError):
        return []
    return [path for path in output.decode(errors="replace").split("\0") if path]


def workspace_fingerprint(root):
    digest = hashlib.sha256()
    try:
        digest.update(run_git(root, "rev-parse", "HEAD"))
        digest.update(run_git(root, "diff", "--binary", "HEAD", "--"))
    except (OSError, subprocess.CalledProcessError):
        digest.update(run_git(root, "diff", "--binary", "--cached", "--"))
        digest.update(run_git(root, "diff", "--binary", "--"))

    for relative_path in untracked_paths(root):
        path = root / relative_path
        if path.is_symlink():
            digest.update(relative_path.encode())
            digest.update(os.readlink(path).encode())
            continue
        if not path.is_file():
            continue
        digest.update(relative_path.encode())
        try:
            with path.open("rb") as source:
                while chunk := source.read(1024 * 1024):
                    digest.update(chunk)
        except OSError:
            digest.update(b"unreadable")
    return digest.hexdigest()


def state_path(root, payload):
    session_id = re.sub(r"[^A-Za-z0-9_.-]", "_", str(payload.get("session_id") or "unknown"))
    try:
        git_dir = Path(run_git(root, "rev-parse", "--absolute-git-dir").decode().strip())
    except (OSError, subprocess.CalledProcessError):
        git_dir = root / ".git"
    return git_dir / "agent-review-hooks" / f"{session_id}.json"


def load_state(path):
    try:
        value = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return {}
    return value if isinstance(value, dict) else {}


def save_state(path, state):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(state, ensure_ascii=False))
    temporary.replace(path)


def snapshot(payload, root, path):
    prompt = str(payload.get("prompt") or "")
    state = load_state(path)
    if prompt.startswith(AUTOMATIC_REVIEW_PREFIX) and state:
        return
    save_state(
        path,
        {
            "baseline": workspace_fingerprint(root),
            "baseline_paths": changed_paths(root),
            "blocks": 0,
            "prompt": prompt,
        },
    )


def requests_planning(prompt):
    planning_commands = (
        "/opsx:propose",
        "/opsx:update",
        "$openspec-propose",
        "$openspec-update-change",
    )
    lowered = prompt.lower()
    return any(value in lowered for value in planning_commands) or bool(
        re.search(
            r"\bplan(?:ning)?\b|\bproposal\b|\bплан(?:а|е|ом|у|ы|ов)?\b|\bпланирован\w*\b|\bспланир\w*\b|\bпроектир\w*\b",
            lowered,
        )
    )


def review_mode(payload, state, paths):
    prompt = str(state.get("prompt") or "")
    if payload.get("permission_mode") == "plan" or requests_planning(prompt):
        return "planning"
    if paths and all(path.startswith("openspec/") for path in paths):
        return "planning"
    return "implementation"


def build_prompt(root, payload, state, mode, paths):
    instructions = (Path(__file__).parent / "review-prompt.md").read_text()
    original_prompt = str(state.get("prompt") or "(not captured)")
    assistant_message = str(payload.get("last_assistant_message") or "(not available)")
    path_list = "\n".join(f"- {path}" for path in paths) or "- none"
    baseline_paths = state.get("baseline_paths") or []
    baseline_path_list = "\n".join(f"- {path}" for path in baseline_paths) or "- none"
    return f"""{instructions}

Review mode: {mode}
Repository root: {root}

Latest user request:
<user_request>
{original_prompt}
</user_request>

Candidate response:
<candidate_response>
{assistant_message}
</candidate_response>

Paths that already had changes before this request:
{baseline_path_list}

Currently changed paths:
{path_list}
"""


def schema_path():
    return Path(__file__).parent / "review-result.schema.json"


def parse_json_output(text):
    value = json.loads(text)
    if isinstance(value, dict) and isinstance(value.get("structured_output"), dict):
        value = value["structured_output"]
    if not isinstance(value, dict):
        raise ValueError("reviewer returned a non-object result")
    if not isinstance(value.get("ok"), bool) or not isinstance(value.get("reason"), str):
        raise ValueError("reviewer result does not match the required schema")
    return value


def run_codex(root, prompt):
    with tempfile.TemporaryDirectory(prefix="codex-stop-review-") as temporary_dir:
        output_path = Path(temporary_dir) / "result.json"
        command = [
            "codex",
            "exec",
            "--ephemeral",
            "--disable",
            "hooks",
            "--model",
            "gpt-5.6-sol",
            "--config",
            'model_reasoning_effort="low"',
            "--sandbox",
            "read-only",
            "--cd",
            str(root),
            "--output-schema",
            str(schema_path()),
            "--output-last-message",
            str(output_path),
            "--color",
            "never",
            "-",
        ]
        environment = os.environ.copy()
        environment["AGENT_REVIEW_HOOK_ACTIVE"] = "1"
        environment.pop("CODEX_SESSION_ID", None)
        environment.pop("CODEX_THREAD_ID", None)
        process = subprocess.run(
            command,
            input=prompt,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
            timeout=REVIEW_TIMEOUT_SECONDS,
        )
        if process.returncode != 0:
            raise RuntimeError(process.stderr.strip() or f"codex exited with {process.returncode}")
        return parse_json_output(output_path.read_text())


def run_claude(root, prompt):
    schema = json.loads(schema_path().read_text())
    command = [
        "claude",
        "--print",
        "--model",
        "opus",
        "--effort",
        "low",
        "--permission-mode",
        "plan",
        "--tools",
        "Read,Glob,Grep,Bash",
        "--allowed-tools",
        "Read,Glob,Grep,Bash(git status *),Bash(git diff *),Bash(git ls-files *),Bash(git rev-parse *)",
        "--max-turns",
        "20",
        "--no-session-persistence",
        "--prompt-suggestions",
        "false",
        "--output-format",
        "json",
        "--json-schema",
        json.dumps(schema),
        prompt,
    ]
    environment = os.environ.copy()
    environment["AGENT_REVIEW_HOOK_ACTIVE"] = "1"
    environment["CLAUDE_CODE_EFFORT_LEVEL"] = "low"
    environment.pop("CLAUDECODE", None)
    environment.pop("CLAUDE_CODE_SESSION_ID", None)
    process = subprocess.run(
        command,
        cwd=root,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=environment,
        timeout=REVIEW_TIMEOUT_SECONDS,
    )
    if process.returncode != 0:
        raise RuntimeError(process.stderr.strip() or f"claude exited with {process.returncode}")
    return parse_json_output(process.stdout)


def stop(client, payload, root, path):
    state = load_state(path)
    fingerprint = workspace_fingerprint(root)
    paths = changed_paths(root)
    should_review = (
        payload.get("permission_mode") == "plan"
        or requests_planning(str(state.get("prompt") or ""))
        or bool(payload.get("stop_hook_active"))
        or int(state.get("blocks") or 0) > 0
        or state.get("baseline") != fingerprint
    )
    if not should_review:
        emit({})
        return

    mode = review_mode(payload, state, paths)
    prompt = build_prompt(root, payload, state, mode, paths)
    try:
        result = run_codex(root, prompt) if client == "codex" else run_claude(root, prompt)
    except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as error:
        message = str(error).strip()[-1200:] or "unknown reviewer error"
        emit(
            {
                "continue": False,
                "stopReason": f"Automatic {client} review could not run: {message}",
                "systemMessage": f"Automatic {client} review failed. The result was not accepted.",
            }
        )
        return

    if result.get("ok") is True:
        state.update({"baseline": fingerprint, "blocks": 0})
        save_state(path, state)
        emit({})
        return

    reason = str(result.get("reason") or "The reviewer rejected the result without a reason.").strip()
    blocks = int(state.get("blocks") or 0) + 1
    state["blocks"] = blocks
    save_state(path, state)
    if blocks > MAX_REVISIONS:
        emit(
            {
                "continue": False,
                "stopReason": f"Automatic review still fails after {MAX_REVISIONS} revisions: {reason}",
                "systemMessage": "Automatic review reached its revision limit. The result was not accepted.",
            }
        )
        return
    emit(
        {
            "decision": "block",
            "reason": f"{AUTOMATIC_REVIEW_PREFIX}\n{reason}\nFix the result, update OpenSpec when requested, and finish with another review.",
        }
    )


def main():
    payload = read_payload()
    if os.environ.get("AGENT_REVIEW_HOOK_ACTIVE") == "1":
        if payload.get("hook_event_name") == "Stop":
            emit({})
        return

    root = find_root(payload)
    path = state_path(root, payload)
    action = sys.argv[1] if len(sys.argv) > 1 else ""
    if action == "snapshot":
        snapshot(payload, root, path)
        return
    if action in {"codex", "claude"}:
        stop(action, payload, root, path)
        return
    emit({"continue": False, "stopReason": f"Unknown review hook action: {action}"})


if __name__ == "__main__":
    main()
