#!/usr/bin/env python3
"""Read one configured Fleet Secrets record into a private local file."""

import hashlib
import json
import os
import pathlib
import re
import stat
import subprocess
import sys
import tempfile
import urllib.parse


CONFIG_PATH = pathlib.Path.home() / ".config/aivoicebus/grokbot-token-handoff.json"
SCOPE_HOST = re.compile(r"^[a-z0-9.-]+$")
NAME = re.compile(r"^[a-z0-9][a-z0-9_-]{0,127}$")
SSH_ALIAS = re.compile(r"^[A-Za-z0-9_.-]{1,128}$")
MAX_SECRET_BYTES = 4096

REMOTE_READER = r'''
import json, subprocess, sys, urllib.parse, urllib.request
cfg = json.loads(__CONFIG_JSON__)
host = urllib.parse.urlsplit(cfg["vault_url"]).hostname
scope = host
args = ["fleet-runner", "key", "issue", cfg["key_slug"], "-user", cfg["consumer"],
        "-tier", "service", "-scope", scope, "-ttl", "5m", "-use-limit", "1"]
try:
    dry = subprocess.run(args + ["-dry-run"], capture_output=True, text=True, timeout=15)
    expected = f' would issue: user={cfg["consumer"]} scope={scope} tier="service" '
    dry_text = " " + dry.stdout + " " + dry.stderr + " "
    if dry.returncode or expected not in dry_text or "ttl=5m0s" not in dry_text:
        raise RuntimeError("dry-run")
    issued = subprocess.run(args, capture_output=True, text=True, timeout=15)
    key = issued.stdout.strip()
    if issued.returncode or not key or any(c.isspace() for c in key):
        raise RuntimeError("issue")
    listed = subprocess.run(["fleet-runner", "key", "list", "--filter", cfg["key_slug"]],
                            capture_output=True, text=True, timeout=15)
    listing = listed.stdout + listed.stderr
    if listed.returncode or cfg["consumer"] not in listing or scope not in listing:
        raise RuntimeError("list")
    record = urllib.parse.quote(cfg["secret_name"], safe="")
    url = cfg["vault_url"].rstrip("/") + "/secrets/" + record
    req = urllib.request.Request(url, headers={"X-API-Key": key})
    with urllib.request.urlopen(req, timeout=15) as response:
        payload = json.loads(response.read(16384))
    if payload.get("name") != cfg["secret_name"]:
        raise RuntimeError("record")
    value = payload.get("value")
    if not isinstance(value, str):
        raise RuntimeError("value")
    sys.stdout.write(json.dumps({"name": cfg["secret_name"], "value": value},
                                separators=(",", ":")))
except Exception:
    print("vault handoff failed", file=sys.stderr)
    raise SystemExit(1)
'''


def fail(reason):
    print(f"handoff=failed reason={reason}")
    raise SystemExit(1)


def read_config():
    try:
        info = CONFIG_PATH.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            fail("config_permissions")
        cfg = json.loads(CONFIG_PATH.read_text(encoding="utf-8"))
    except SystemExit:
        raise
    except Exception:
        fail("config_unavailable")
    required = {"ssh_target", "vault_url", "secret_name", "key_slug", "consumer", "output_file"}
    if not isinstance(cfg, dict) or set(cfg) != required:
        fail("config_fields")
    if not isinstance(cfg["ssh_target"], str) or not SSH_ALIAS.fullmatch(cfg["ssh_target"]):
        fail("ssh_target")
    for key in ("secret_name", "key_slug", "consumer"):
        if not isinstance(cfg[key], str) or not NAME.fullmatch(cfg[key]):
            fail("config_name")
    if cfg["key_slug"] != cfg["consumer"]:
        fail("key_identity")
    try:
        parsed = urllib.parse.urlsplit(cfg["vault_url"])
    except Exception:
        fail("vault_url")
    if (parsed.scheme != "https" or not parsed.hostname or not SCOPE_HOST.fullmatch(parsed.hostname)
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path not in ("", "/")):
        fail("vault_url")
    output = pathlib.Path(cfg["output_file"]).expanduser()
    home = pathlib.Path.home().resolve()
    if not output.is_absolute() or output.name in ("", ".", ".."):
        fail("output_path")
    try:
        output.resolve(strict=False).relative_to(home)
    except Exception:
        fail("output_outside_home")
    cfg["output_path"] = output
    return cfg


def fetch(cfg):
    remote_cfg = {key: cfg[key] for key in ("vault_url", "secret_name", "key_slug", "consumer")}
    encoded = json.dumps(remote_cfg, separators=(",", ":"))
    program = REMOTE_READER.replace("__CONFIG_JSON__", repr(encoded))
    try:
        result = subprocess.run(
            ["/usr/bin/ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
             cfg["ssh_target"], "python3", "-"],
            input=program, text=True, capture_output=True, timeout=45,
        )
    except Exception:
        fail("ssh_unavailable")
    if result.returncode or len(result.stdout) > 16384:
        fail("vault_request")
    try:
        response = json.loads(result.stdout)
    except Exception:
        fail("vault_response")
    if response.get("name") != cfg["secret_name"] or not isinstance(response.get("value"), str):
        fail("vault_record")
    value = response["value"].encode("utf-8")
    if not 16 <= len(value) <= MAX_SECRET_BYTES or any(b in value for b in (b"\x00", b"\r", b"\n")):
        fail("secret_format")
    return value


def store_private(path, value):
    uid = os.getuid()
    parent = path.parent
    try:
        parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    except FileExistsError:
        pass
    except Exception:
        fail("output_directory")
    try:
        parent_info = parent.lstat()
        if (not stat.S_ISDIR(parent_info.st_mode) or parent_info.st_uid != uid
                or stat.S_IMODE(parent_info.st_mode) != 0o700):
            fail("output_directory_permissions")
        if path.is_symlink():
            fail("output_symlink")
        if path.exists():
            info = path.lstat()
            if not stat.S_ISREG(info.st_mode) or info.st_uid != uid or stat.S_IMODE(info.st_mode) != 0o600:
                fail("output_file_permissions")
            if path.read_bytes() == value:
                return "already_present"
    except SystemExit:
        raise
    except Exception:
        fail("output_state")

    temp_name = None
    try:
        fd, temp_name = tempfile.mkstemp(prefix=".handoff-", dir=parent)
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "wb") as out:
            out.write(value)
            out.flush()
            os.fsync(out.fileno())
        os.replace(temp_name, path)
        temp_name = None
        info = path.stat()
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != uid or stat.S_IMODE(info.st_mode) != 0o600
                or path.read_bytes() != value):
            fail("output_verification")
    except SystemExit:
        raise
    except Exception:
        fail("output_write")
    finally:
        if temp_name:
            try:
                os.unlink(temp_name)
            except OSError:
                pass
    return "written"


def main():
    cfg = read_config()
    value = fetch(cfg)
    outcome = store_private(cfg["output_path"], value)
    digest = hashlib.sha256(value).hexdigest()[:8]
    print(f"handoff={outcome} len={len(value)} sha256_prefix={digest}")


if __name__ == "__main__":
    main()
