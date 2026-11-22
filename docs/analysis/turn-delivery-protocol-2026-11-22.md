# Turn and delivery protocol across death, compaction, drops and restarts

Deep analysis, 2026-11-22. Branch `filler/analysis-2026-11-22`, cut from
`filler/fix-hunt-findings-2` at `c33e60e` (both reviewed hunt-fix groups,
not yet on `master`). Read-only with respect to product code: nothing
under the module was changed except this report and the files under
[`docs/analysis/turn-delivery-protocol-2026-11-22/`](turn-delivery-protocol-2026-11-22/).

Question: is the turn and delivery protocol correct across process death,
consumer death, compaction, drops and restarts? Three properties are
checked with TLC:

1. every event is delivered to a live consumer exactly once or reported
   lost;
2. no prompt is accepted during compaction and then dropped;
3. no child outlives its owner.

Then the acknowledgement design of 🎯T1404 is modelled.

Status: **in progress**. Sections marked *pending* are not written yet.

## 1. What the system is, from the code

*pending*

## 2. Invariants

*pending*

## 3. Models and what TLC found

*pending*

## 4. Confirmed defects

*pending*

## 5. 🎯T1404: the acknowledgement design

*pending*

## 6. Design findings that are not defects

*pending*

## 7. Unconfirmed suspicions

*pending*

## 8. Relation to the 2026-11-21 Codex partials

*pending*

## 9. Coverage

*pending*
