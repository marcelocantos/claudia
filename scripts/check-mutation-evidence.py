#!/usr/bin/env python3
"""Re-derive the mutation evidence a target's acceptance quotes (🎯T32).

A target's acceptance says "reintroducing X makes test T RED". That sentence is
quoted once, in a commit message, and then never run again. Any later commit
that changes T's INPUTS can retire the evidence without turning anything red —
no assertion weakened, no suite failing, nothing for CI to notice. 🎯T30's
8c5e04a did exactly that to 🎯T28: it added a `landed` argument to
ensureSubmitted and passed landed=true at 🎯T28's call sites, which was correct
for the branch it modelled, and which made 🎯T28's over-broadness mutation stop
biting TestT28DailyDeliveredFramesReportSuccess.

This script turns those sentences back into an experiment. For each entry in
mutation-evidence.json it:

  1. runs the named tests UNMUTATED and requires every one of them GREEN — a
     test that is already red, skipped, or absent is not evidence, and would
     otherwise read as "the mutation killed it";
  2. applies the mutation as an anchored source edit and requires each test to
     land on its declared side: `red` tests must go RED (that is the evidence),
     `green` tests must stay GREEN (that is how a commit body's "fixture Y is
     unaffected, so the two cover distinct mechanisms" claim stays true).

Everything happens in a throwaway `git worktree` of the revision under test,
never in the working tree, which is shared with other agents' uncommitted edits.

--prove-teeth is the second half, and the reason this is a loop rather than an
artifact. A check that only re-runs mutations can itself be toothless: it must
be shown to go RED when the decay it exists to catch is applied. An entry may
declare a `decay` — an edit to the TEST's inputs modelled on a real commit —
and --prove-teeth asserts both halves of what makes that decay invisible:

    decay alone            every named test stays GREEN   (CI sees nothing)
    decay + mutation       a `red` test SURVIVES          (so THIS CHECK goes
                                                           red, which is the
                                                           only alarm there is)

If the declared decay stops disarming the evidence, --prove-teeth goes red and
says so: the worked instance has gone stale and must be re-derived by hand.

Note for whoever reads the gate line: this harness succeeds partly BY MAKING
TESTS FAIL, so the raw `go test` output of every run goes to a log file whose
path is printed, and stdout carries only this script's own verdicts. Echoing
that output on stdout instead makes a gate runner read a mutant's "--- FAIL" as
this script's own failure and report a green run as SUSPECT.

Usage:
    scripts/check-mutation-evidence.py [--prove-teeth] [--rev REV] [--target T]
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

EVIDENCE = "mutation-evidence.json"

# `go test -v` result lines, indented one level per subtest depth:
#     --- PASS: TestFoo (0.00s)
#         --- FAIL: TestFoo/bar.txt (0.00s)
RESULT = "^[ \t]*--- (PASS|FAIL|SKIP): {name}(?: |$)"

# `go test` reports a package that would not compile as "[build failed]" on the
# FAIL line and prints the compiler diagnostics above it.
BUILD_FAILED = re.compile(r"^(FAIL\t\S+ \[build failed\]|# )", re.M)


class Red(Exception):
    """A verdict this script exists to report. Message goes to stdout."""


def load_evidence(path):
    """Load the evidence file, refusing duplicate keys.

    Many agents share this clone, and json.load keeps the LAST of two identical
    keys without a word — a second `decay` block written by another hand would
    discard the first silently, which is the exact failure mode this whole file
    exists to end.
    """
    def no_dupes(pairs):
        out = {}
        for k, v in pairs:
            if k in out:
                sys.exit(f"{EVIDENCE}: duplicate key {k!r} in one object — JSON "
                         "keeps the last and drops the first without a word. "
                         "Merge them by hand.")
            out[k] = v
        return out

    with open(path) as f:
        return json.load(f, object_pairs_hook=no_dupes)


def go_run(test):
    """A -run pattern that matches exactly the named test path.

    `go test` splits the pattern on "/" and matches each element against the
    corresponding name element, unanchored — so a bare "TestFoo" also selects
    "TestFooBar". Anchor every element, and escape it: subtest names here are
    fixture filenames, and an unescaped "." matches any character.
    """
    return "/".join("^" + re.escape(part) + "$" for part in test.split("/"))


def run_one(work, package, test, log, label):
    """Run one named test in the worktree. -> 'green' | 'red' | reason string."""
    # Bytes, not text: the fixtures are raw `tmux capture-pane` frames, and a
    # failing assertion echoes one. A frame cut mid-rune is not valid UTF-8, and
    # decoding strictly turns a mutant this check is meant to REPORT into a
    # traceback out of subprocess.
    r = subprocess.run(
        ["go", "test", package, "-count=1", "-v", "-run", go_run(test)],
        cwd=work, capture_output=True,
    )
    out = (r.stdout + r.stderr).decode("utf-8", "replace")
    log.write(f"===== [{label}] {package} {test} (exit={r.returncode})\n{out}\n")
    log.flush()
    m = re.search(RESULT.format(name=re.escape(test)), out, re.M)
    if m:
        return {"PASS": "green", "FAIL": "red", "SKIP": "skipped"}[m.group(1)]
    if BUILD_FAILED.search(out):
        return "the package did not compile"
    if r.returncode == 0:
        return "no test of that name ran"
    return f"did not report a result (exit={r.returncode})"


def apply_edits(work, edits, files):
    """Apply anchored edits to the worktree. `files` accumulates the pristine
    contents of everything touched, so restore() can put it all back."""
    staged = {}
    for e in edits:
        name = e["file"]
        if name not in files:
            with open(os.path.join(work, name)) as f:
                files[name] = f.read()
        src = staged.get(name, files[name])
        n = src.count(e["find"])
        if n != 1:
            raise Red(
                f"anchor matched {n} times in {name}, want exactly 1: "
                f"{e['find'].strip()[:70]!r}\n"
                "        The code this mutation edits has moved. The mutation must "
                "be re-derived by hand — never silently skipped."
            )
        staged[name] = src.replace(e["find"], e["replace"])
    for name, src in staged.items():
        with open(os.path.join(work, name), "w") as f:
            f.write(src)


def restore(work, files):
    """Put every touched file back, and prove it: a crashed run once left a
    mutant on disk and the next run took it as its baseline."""
    for name, src in files.items():
        path = os.path.join(work, name)
        with open(path, "w") as f:
            f.write(src)
        with open(path) as f:
            if hashlib.sha1(f.read().encode()).hexdigest() != \
               hashlib.sha1(src.encode()).hexdigest():
                raise Red(f"could not restore {name} in the worktree")


def check_entry(work, entry, log, files, edits, label):
    """Run every named test under `edits`. -> [(name, expect, got)]."""
    restore(work, files)
    apply_edits(work, edits, files)
    return [(t["name"], t["expect"],
             run_one(work, entry["package"], t["name"], log, label))
            for t in entry["tests"]]


def baseline(work, entry, log, files):
    """Every named test must be GREEN unmutated, whatever side it is declared
    on. A red, skipped or absent test is not evidence of anything."""
    bad = []
    for t in entry["tests"]:
        got = run_one(work, entry["package"], t["name"], log, "baseline")
        if got != "green":
            bad.append((t["name"], got))
    if bad:
        lines = "\n".join(f"        {n}: {g} unmutated" for n, g in bad)
        raise Red("these tests are not GREEN before the mutation is applied, so "
                  "nothing they do under it is evidence:\n" + lines)


def verify(work, entry, log, files):
    """The evidence itself: mutation applied, each test on its declared side."""
    got = check_entry(work, entry, log, files, entry["edits"], "mutation")
    wrong = [(n, e, g) for n, e, g in got if g != e]
    if not wrong:
        red = [t["name"] for t in entry["tests"] if t["expect"] == "red"]
        print(f"    GREEN: mutation bites — RED: {', '.join(red)}")
        return
    lines = []
    for name, expect, got in wrong:
        if expect == "red":
            lines.append(f"        {name}: SURVIVED the mutation (got {got}, "
                         "want RED) — this evidence has been disarmed")
        else:
            lines.append(f"        {name}: got {got}, want GREEN — the mutation "
                         "now bites a test declared unaffected by it")
    raise Red("the quoted evidence no longer holds:\n" + "\n".join(lines))


def prove_teeth(work, entry, log, files):
    """The check must go RED when the decay it exists to catch is applied."""
    decay = entry.get("decay")
    if not decay:
        print("    (no decay declared — nothing to prove here)")
        return
    print(f"    decay: {decay['what']}")

    quiet = check_entry(work, entry, log, files, decay["edits"], "decay")
    if any(g != "green" for _, _, g in quiet):
        lines = "\n".join(f"        {n}: {g}" for n, _, g in quiet if g != "green")
        raise Red("the decay does not go unnoticed — some test is not GREEN under "
                  "it alone, so CI would already catch this shape:\n" + lines +
                  "\n        Re-derive the decay: it must model a change that "
                  "leaves every suite green.")
    print("    decay alone: every named test still GREEN — invisible to CI, as it was")

    got = check_entry(work, entry, log, files, decay["edits"] + entry["edits"],
                      "decay+mutation")
    disarmed = [(n, g) for n, e, g in got if e == "red" and g == "green"]
    if not disarmed:
        raise Red("the declared decay no longer disarms this evidence: under decay "
                  "+ mutation every `red` test still went RED, so this check would "
                  "stay green and prove nothing.\n        The worked instance is "
                  "stale — re-derive it against the code as it now stands.")
    for name, _ in disarmed:
        print(f"    decay + mutation: {name} SURVIVED — so this check goes RED")
    print("    GREEN: the check has teeth — it catches the decay, "
          "which no suite does")


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--rev", default="HEAD", help="revision to check (default HEAD)")
    ap.add_argument("--target", help="check only this target's entries")
    ap.add_argument("--prove-teeth", action="store_true",
                    help="assert each declared decay makes this check go red")
    args = ap.parse_args()

    repo = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    entries = load_evidence(os.path.join(repo, EVIDENCE))["entries"]
    if args.target:
        entries = [e for e in entries if e["target"] == args.target]
        if not entries:
            sys.exit(f"no entries for target {args.target}")

    sha = subprocess.run(["git", "rev-parse", args.rev], cwd=repo, check=True,
                         capture_output=True, text=True).stdout.strip()
    mode = "proving the check's teeth" if args.prove_teeth else \
        "re-deriving quoted mutation evidence"
    print(f"{mode} at {args.rev} ({sha[:12]}), {len(entries)} entries")

    work = tempfile.mkdtemp(prefix="mutation-evidence-")
    logfd, logpath = tempfile.mkstemp(prefix="mutation-evidence-", suffix=".log")
    log = os.fdopen(logfd, "w")
    print(f"raw go test output of every run: {logpath}")
    reds = []
    try:
        subprocess.run(["git", "worktree", "add", "--detach", work, sha],
                       cwd=repo, check=True, capture_output=True)
        for entry in entries:
            print(f"\n🎯{entry['target']} / {entry['mutation']}")
            print(f"    {entry['what']}")
            print(f"    quoted by {entry['quoted_by']}, tests in {entry['package']}")
            files = {}
            try:
                baseline(work, entry, log, files)
                if args.prove_teeth:
                    prove_teeth(work, entry, log, files)
                else:
                    verify(work, entry, log, files)
            except Red as e:
                reds.append(f"🎯{entry['target']}/{entry['mutation']}")
                print(f"    RED: {e}")
            finally:
                try:
                    restore(work, files)
                except Red as e:
                    reds.append(f"🎯{entry['target']}/{entry['mutation']} (restore)")
                    print(f"    RED: {e}")
    finally:
        log.close()
        subprocess.run(["git", "worktree", "remove", "--force", work],
                       cwd=repo, capture_output=True)
        shutil.rmtree(work, ignore_errors=True)

    print()
    if reds:
        print(f"RED: {len(reds)}/{len(entries)} entries — {', '.join(reds)}")
        return 1
    print(f"GREEN: {len(entries)}/{len(entries)} entries")
    return 0


if __name__ == "__main__":
    sys.exit(main())
