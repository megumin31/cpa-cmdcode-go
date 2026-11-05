#!/usr/bin/env python3
"""Publish one validated, parentless model snapshot with an explicit Git lease.

Run prepare before generation/validation, then publish the exact candidate.
Only models-data is writable. Code, release tags, and the working index are not
changed. The branch has one reachable commit; GitHub controls object retention.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys

spec = importlib.util.spec_from_file_location("extract", Path(__file__).with_name("extract-models.py"))
extract = importlib.util.module_from_spec(spec)
spec.loader.exec_module(extract)
REF = "refs/heads/models-data"


def git(*args, data=None, env=None, check=True):
    result = subprocess.run(["git", *args], input=data, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, env=env)
    if check and result.returncode:
        raise RuntimeError(result.stderr.decode(errors="replace").strip())
    return result


def output(*args, **kwargs):
    return git(*args, **kwargs).stdout.decode().strip()


def load_catalog(raw):
    if len(raw) > 4 << 20:
        raise ValueError("model snapshot exceeds runtime size limit")
    doc = extract.parse_json(raw)
    extract.validate_document(doc)
    return doc


def prepare(directory, remote, bootstrap):
    directory.mkdir(parents=True, exist_ok=True)
    # Do not let a failed new attempt reuse a previous candidate/state file.
    for name in ("state.json", "candidate.json"):
        (directory / name).unlink(missing_ok=True)
    found = git("ls-remote", "--exit-code", remote, REF, check=False)
    if found.returncode == 2:
        expected = ""
        raw = bootstrap.read_bytes()
    elif found.returncode:
        raise RuntimeError(found.stderr.decode(errors="replace").strip())
    else:
        # The branch may advance after ls-remote. Use exactly what fetch saw as
        # both the comparison snapshot and the expected SHA in the later lease.
        git("fetch", "--no-tags", remote, REF)
        expected = output("rev-parse", "FETCH_HEAD")
        headers = output("cat-file", "-p", expected).split("\n\n", 1)[0].splitlines()
        if any(line.startswith("parent ") for line in headers):
            raise ValueError("models-data head must be a parentless snapshot")
        names = output("ls-tree", "--name-only", expected).splitlines()
        if names != ["models.json"]:
            raise ValueError("models-data must contain only models.json")
        raw = git("show", expected + ":models.json").stdout
    load_catalog(raw)
    (directory / "previous.json").write_bytes(raw)
    state = {"remote": remote, "expected": expected,
             "previous_sha256": hashlib.sha256(raw).hexdigest()}
    (directory / "state.json").write_text(json.dumps(state) + "\n")
    print("Prepared " + (expected or "first publication"))


def publish(directory):
    state = json.loads((directory / "state.json").read_text())
    remote, expected = state["remote"], state["expected"]
    if not remote or remote.startswith("-") or "\n" in remote:
        raise ValueError("invalid remote")
    if expected and not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", expected):
        raise ValueError("invalid expected commit")
    previous = (directory / "previous.json").read_bytes()
    if hashlib.sha256(previous).hexdigest() != state["previous_sha256"]:
        raise ValueError("previous snapshot changed after prepare")
    raw = (directory / "candidate.json").read_bytes()
    old, new = load_catalog(previous), load_catalog(raw)
    if expected and extract.semantic_catalog(old) == extract.semantic_catalog(new):
        print("No Go roster or metadata change; no commit or push")
        return None
    blob = output("hash-object", "-w", "--stdin", data=raw)
    tree = output("mktree", data=f"100644 blob {blob}\tmodels.json\n".encode())
    env = dict(os.environ)
    for key in ("AUTHOR", "COMMITTER"):
        env["GIT_" + key + "_NAME"] = "github-actions[bot]"
        env["GIT_" + key + "_EMAIL"] = "41898282+github-actions[bot]@users.noreply.github.com"
    # Intentionally omit -p: the data branch keeps no reachable parent history.
    commit = output("commit-tree", tree, "-m", "chore(models): publish Go roster snapshot", env=env)
    # Empty expected SHA means the ref must not exist. Never use plain --force,
    # an implicit remote-tracking lease, or an automatic conflict retry.
    git("push", f"--force-with-lease={REF}:{expected}", remote, f"{commit}:{REF}")
    print("Published models-data " + commit)
    return commit


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("prepare", "publish"))
    parser.add_argument("--directory", type=Path, default=Path("build/model-data"))
    parser.add_argument("--remote", default="origin")
    parser.add_argument("--bootstrap", type=Path, default=Path("data/models.snapshot.json"))
    args = parser.parse_args()
    if not args.remote or args.remote.startswith("-") or "\n" in args.remote:
        parser.error("invalid remote")
    if args.action == "prepare":
        prepare(args.directory, args.remote, args.bootstrap)
    else:
        publish(args.directory)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, KeyError, TypeError, RuntimeError) as exc:
        print(f"publish-models: {exc}", file=sys.stderr)
        sys.exit(2)
