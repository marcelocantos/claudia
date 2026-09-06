#!/usr/bin/env python3
"""Run the hermetic test suite and declare its residue (🎯T48).

`go test ./...` answers one question: did anything fail. It says nothing
about what did not run. Every skip guard in this suite — live gates, missing
binaries, POSIX-only fakes — turns a test into a no-op that prints `ok`, and a
runner without tmux passes the whole Session suite by not running it.

This script runs `go test -json` and makes the skips visible and accountable:

  1. failures are reported with their full output (and the -shuffle seed that
     produced them, so the order can be replayed);
  2. every skipped test is listed with its reason and classified against the
     residue this repo declares below — live gates (`make live`), subprocess
     helpers, and host-bound tests that can only run on one machine;
  3. a skip that matches no declared residue is a gate failure. "tmux not
     installed", "python3 required", "sqlite3 not on PATH" are broken runners,
     not residue, and the gate says so instead of printing green.

Only the summary lines, failures, and the residue ledger reach stdout; a
passing test's logging does not.

Usage:
    scripts/gate-test.py [package ...]        default: ./...
"""

import json
import re
import subprocess
import sys

GO_TEST = ["go", "test", "-json", "-race", "-count=1", "-shuffle=on"]

# Skip reasons this repo declares as residue, named so the ledger can say
# which oracle owns them. A reason that matches none of these is a failure.
RESIDUE = [
    ("live gate (make live)", re.compile(r"CLAUDIA_\w*LIVE|no live gate set|XAI_API_KEY")),
    ("subprocess helper", re.compile(r"helper — only runs as")),
    ("host-bound", re.compile(r"cannot test miss path|neither ChatGPT\.app nor Codex\.app")),
]

# The line go test prints under `--- SKIP:` carries the reason:
#     codex_session_test.go:257: CLAUDIA_CODEX_LIVE not set (...)
SKIP_REASON = re.compile(r"^\s+\S+\.go:\d+: (.*)$")
SHUFFLE_SEED = re.compile(r"-test\.shuffle (\d+)")
PACKAGE_VERDICT = re.compile(r"^(ok|\?|FAIL)\s")


def main(argv):
    packages = argv[1:] or ["./..."]
    proc = subprocess.Popen(GO_TEST + packages, stdout=subprocess.PIPE, text=True)

    test_output = {}   # (package, test) -> [lines]
    pkg_output = {}    # package -> [lines]
    seeds = {}         # package -> shuffle seed
    failed = []        # (package, test, [lines])
    skipped = []       # (package, test, reason)
    passed = 0

    for raw in proc.stdout:
        try:
            ev = json.loads(raw)
        except ValueError:
            sys.stdout.write(raw)
            continue
        action = ev.get("Action")
        pkg = ev.get("Package", "")
        test = ev.get("Test")
        if action == "output":
            line = ev.get("Output", "")
            if test:
                test_output.setdefault((pkg, test), []).append(line)
            else:
                pkg_output.setdefault(pkg, []).append(line)
                m = SHUFFLE_SEED.search(line)
                if m:
                    seeds[pkg] = m.group(1)
            continue
        if test:
            lines = test_output.pop((pkg, test), [])
            if action == "pass":
                passed += 1
            elif action == "fail":
                failed.append((pkg, test, lines))
            elif action == "skip":
                reason = ""
                for line in lines:
                    m = SKIP_REASON.match(line)
                    if m:
                        reason = m.group(1)
                skipped.append((pkg, test, reason))
            continue
        # Package-level verdict.
        if action in ("pass", "skip"):
            for line in pkg_output.pop(pkg, []):
                if PACKAGE_VERDICT.match(line):
                    sys.stdout.write(line)
        elif action == "fail":
            for line in pkg_output.pop(pkg, []):
                sys.stdout.write(line)
            if pkg in seeds:
                print(f"    replay this order: go test -race -count=1 -shuffle={seeds[pkg]} {pkg}")

    status = proc.wait()

    for pkg, test, lines in failed:
        print(f"\n--- FAIL {pkg} {test}")
        sys.stdout.write("".join(lines))

    undeclared = []
    counts = {}
    if skipped:
        print("\nresidue (skipped, not run):")
    for pkg, test, reason in sorted(skipped):
        family = next((name for name, pat in RESIDUE if pat.search(reason)), None)
        if family is None:
            undeclared.append((pkg, test, reason))
            continue
        counts[family] = counts.get(family, 0) + 1
        print(f"  {family:<22} {pkg}.{test}: {reason}")
    for pkg, test, reason in undeclared:
        print(f"  ✗ UNDECLARED          {pkg}.{test}: {reason!r}")

    ledger = ", ".join(f"{n} {name}" for name, n in sorted(counts.items()))
    verdict = "✓" if status == 0 and not undeclared else "✗"
    print(f"{verdict} tests: {passed} passed, {len(failed)} failed, {len(skipped)} skipped"
          + (f" ({ledger})" if ledger else ""))
    if undeclared:
        print(f"✗ {len(undeclared)} skip(s) match no declared residue — a runner missing a "
              "hermetic dependency is a broken gate, not a pass. Install the dependency or, "
              "if the skip is a new class of residue, declare it in scripts/gate-test.py.")
    return 1 if status != 0 or undeclared else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
