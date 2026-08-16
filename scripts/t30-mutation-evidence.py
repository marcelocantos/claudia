#!/usr/bin/env python3
"""Mutation evidence for T30: the submit fix is load-bearing, and it is not
over-broad.

T30's fifth acceptance clause asks for two things to be shown by experiment
rather than asserted in prose: that reverting the submit fix makes a named test
RED, and that one over-broadness mutation shows the sub-threshold sends which
succeeded before T30 are not broken by the new path. A test that is green
against both the fixed and the broken code proves nothing, and only mutation
distinguishes the two.

Each mutant is an anchored edit to internal/tmuxagent/send.go, applied in a
throwaway worktree of the revision under test — never in the working tree,
which is shared with other agents' uncommitted edits — followed by the one test
named as its killer. A mutant that SURVIVES is the finding: it means nothing in
the suite is watching that line.

  M1a     sawContent := landed -> false          the landed handoff is dropped:
                                                 ensureSubmitted starts blind
                                                 again, as it did before T30
  M1b     the vacated-composer return nil ->     the fix's decision is undone
          the pre-T30 re-press                   while its plumbing stays
  M1a+M3  both defects reverted at once          the exact state the live repro
                                                 was captured in
  M2      landed := false -> true                over-broadness on the evidence:
                                                 claim every payload landed
  M3      spinnerLine dropped from               the detector gap T30 found:
          MatchTurnInProgress                    a running turn whose activity
                                                 line we do not recognise
  M4      pasteBlockThreshold 400 -> 0           over-broadness on the routing:
                                                 push the small sends that
                                                 always worked onto the paste
                                                 branch

Exits non-zero if any mutant survives, so it can be run under a gate runner.
Note for whoever reads that gate: this harness SUCCEEDS by making tests fail, so
its stdout carries its own verdict — the assertion each mutant tripped — and the
raw `go test` output of the RED runs goes to a log file whose path is printed.
Echoing that output on stdout instead makes a gate runner read the mutants'
"--- FAIL" as this script's own failure, and report a green run as SUSPECT.

Usage: scripts/t30-mutation-evidence.py [revision]   (default HEAD)
"""

import hashlib
import os
import re
import shutil
import subprocess
import sys
import tempfile

SEND = "internal/tmuxagent/send.go"

# The line `go test` prints for a failed assertion: "    file_test.go:218: msg".
# That message is what a mutant tripped, and it is the verdict this script
# reports; everything else about the RED run goes to the log.
ASSERTION = re.compile(r"^\s+(\w[\w.]*_test\.go:\d+): (.*)$")


def tripped(output):
    """The assertion the killed mutant tripped, safe to print on stdout."""
    for line in output.splitlines():
        m = ASSERTION.match(line)
        if not m:
            continue
        text = f"{m.group(1)}: {m.group(2)}"
        # A gate runner scanning our stdout would read a failure token in the
        # quoted assertion as this script's own failure. No assertion here
        # carries one, and if one ever does the log has it verbatim.
        return "(assertion contains a failure token; see log)" if "FAIL" in text \
            else text[:160]
    return "(no assertion line; see log)"

VACATED_FIXED = """			// The payload was in this box and is not any more, and the
			// only key we sent was Enter: it was submitted (\U0001f3afT30). This
			// is evidence about OUR payload, not about chrome that might
			// belong to another turn, so it is safe where the chrome-first
			// rule \U0001f3afT28 removed was not. Pressing Enter again here is what
			// burned the whole bound and turned a delivered brief into
			// "turn not submitted".
			return nil
"""
VACATED_PRE_T30 = """			// Content was seen then cleared without working chrome —
			// keep pressing Enter a few more times.
			if press+1 < maxSubmitPresses {
				_ = d.sendEnter()
			}
"""
BLIND = ("\tsawContent := landed\n", "\tsawContent := false\n")
NO_SPINNER = ("\treturn turnInProgressHint.Match(f) || spinnerLine.Match(f)\n",
              "\treturn turnInProgressHint.Match(f)\n")

# mutant -> (edits, the one test that must go RED)
MUTANTS = [
    ("M1a", [BLIND], "TestT30VacatedComposerIsSubmitEvidence"),
    ("M1b", [(VACATED_FIXED, VACATED_PRE_T30)], "TestT30VacatedComposerIsSubmitEvidence"),
    ("M1a+M3", [BLIND, NO_SPINNER], "TestT30LiveReproSendSucceeds"),
    ("M2", [("\tlanded := false\n", "\tlanded := true\n")],
     "TestT30EmptyComposerWithoutLandedEvidenceStillFails"),
    ("M3", [NO_SPINNER], "TestT28DailyDeliveredFramesAreNotPasteChips"),
    ("M4", [("const pasteBlockThreshold = 400\n", "const pasteBlockThreshold = 0\n")],
     "TestT30SubThresholdSendUnaffected"),
]


def main():
    repo = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    rev = sys.argv[1] if len(sys.argv) > 1 else "HEAD"
    sha = subprocess.run(["git", "rev-parse", rev], cwd=repo, check=True,
                         capture_output=True, text=True).stdout.strip()
    print(f"mutating {SEND} at {rev} ({sha[:12]})")

    work = tempfile.mkdtemp(prefix="t30-mutation-")
    logfd, logpath = tempfile.mkstemp(prefix="t30-mutation-", suffix=".log")
    log = os.fdopen(logfd, "w")
    print(f"raw go test output of every mutant run: {logpath}")
    survivors = []
    try:
        subprocess.run(["git", "worktree", "add", "--detach", work, sha],
                       cwd=repo, check=True, capture_output=True)
        path = os.path.join(work, SEND)
        with open(path) as f:
            base = f.read()
        # The baseline is the committed file, and it is re-read from git rather
        # than trusted from disk: a crashed run once left a mutant behind, and
        # the next run took it as its baseline without noticing.
        base_sha1 = hashlib.sha1(base.encode()).hexdigest()

        for name, edits, test in MUTANTS:
            src = base
            for old, new in edits:
                if src.count(old) != 1:
                    sys.exit(f"FAIL: {name} anchor matched {src.count(old)} times, "
                             f"want exactly 1: {old.strip()[:60]!r}")
                src = src.replace(old, new)
            with open(path, "w") as f:
                f.write(src)
            try:
                r = subprocess.run(["go", "test", "./internal/tmuxagent/", "-count=1",
                                    "-run", test], cwd=work, capture_output=True)
            finally:
                with open(path, "w") as f:
                    f.write(base)
            with open(path) as f:
                restored = hashlib.sha1(f.read().encode()).hexdigest()
            if restored != base_sha1:
                sys.exit(f"FAIL: {name} did not restore {SEND}")
            killed = r.returncode != 0
            if not killed:
                survivors.append(f"{name} ({test})")
            out = (r.stdout + r.stderr).decode("utf-8", "replace")
            log.write(f"===== {name} -> {test} (exit={r.returncode})\n{out}\n")
            log.flush()
            print(f"\n{name} -> {test}: "
                  f"{'KILLED (test RED)' if killed else 'SURVIVED — NOTHING WATCHES THIS'}"
                  f"  exit={r.returncode}")
            print(f"    tripped: {tripped(out) if killed else 'nothing'}")
    finally:
        log.close()
        subprocess.run(["git", "worktree", "remove", "--force", work],
                       cwd=repo, capture_output=True)
        shutil.rmtree(work, ignore_errors=True)

    print(f"\nRESULT: {len(MUTANTS) - len(survivors)}/{len(MUTANTS)} killed", end="")
    print("" if not survivors else f", SURVIVORS: {', '.join(survivors)}")
    return 1 if survivors else 0


if __name__ == "__main__":
    sys.exit(main())
