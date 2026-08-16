#!/usr/bin/env python3
"""Oracle for T29: STABILITY.md enumerates exactly the public surface of the
release tag it names.

The surface is derived from SOURCE — `go doc -all` over a clean worktree of the
tag named in STABILITY.md's "Snapshot as of:" line — never from the document.
That direction matters: a document checked against itself always passes.

  A. every derived top-level item is named in STABILITY.md
  B. every identifier the surface tables claim exists at that tag really does
  C. no malformed table rows
  D. every row of a "| … | Status |" table carries a Stable / Needs review /
     Fluid assessment
  E. every exported struct field is named too — fields are surface, and go
     doc's top-level listing alone would let one disappear silently
  F. every environment variable the public packages read is named. go doc
     cannot see these, so they come from the tag's source
  G. the counts the document states about itself match the derived surface

Exits non-zero on any violation, so it can be run under a gate runner or CI.

Usage: scripts/check-stability-surface.py [repo-root]
"""

import os
import re
import shutil
import subprocess
import sys
import tempfile

# Packages that are part of the module but not part of the public surface.
NON_PUBLIC_DIRS = {"internal", "cmd", "specs", "docs", "scripts", "testdata"}

# The stability vocabulary defined in STABILITY.md's own legend. A status cell
# must open with one of these; a trailing clause after it is fine.
STATUSES = ("Stable", "Needs review", "Fluid")

# Environment-variable rows name a variable, not a Go identifier, so check B
# cannot look them up in the derived surface.
ENV_VAR = re.compile(r"^[A-Z][A-Z0-9_]+$")


def derive_surface(work, pkgs):
    """Parse `go doc -all` for each package into (top-level items, all names).

    Top-level items are types, funcs, methods, consts and vars — including the
    members of grouped `const (…)` / `var (…)` blocks, which is what the
    document counts. Struct fields land in `names` only: they are enumerated in
    the tables but not counted as top-level items.
    """
    items = []                      # (pkg, kind, owner, name)
    fields = []                     # (pkg, type, field)
    names = set()                   # every exported identifier, fields included

    for pkg in pkgs:
        out = subprocess.run(
            ["go", "doc", "-all", pkg],
            cwd=work, check=True, capture_output=True, text=True,
        ).stdout

        in_group = None             # "const" / "var" while inside a ( … ) block
        in_body = None              # struct type name while inside its body
        for line in out.splitlines():
            if in_group or in_body:
                if line.startswith(")") or line.startswith("}"):
                    in_group, in_body = None, None
                    continue
                # `Name, Other Type = value` or a struct field `Name Type`
                head = re.match(r"^\t([A-Z]\w*(?:,\s*[A-Z]\w*)*)\s+\S", line)
                if head:
                    for name in re.split(r",\s*", head.group(1)):
                        names.add(name)
                        if in_group:
                            items.append((pkg, in_group, None, name))
                        else:
                            fields.append((pkg, in_body, name))
                continue

            if line in ("const (", "var ("):
                in_group = line.split()[0]
                continue

            fn = re.match(r"^func (?:\([^)]*?\*?(\w+)\) )?([A-Z]\w*)", line)
            if fn:
                owner, name = fn.groups()
                items.append((pkg, "method" if owner else "func", owner, name))
                names.add(name)
                continue

            ty = re.match(r"^type ([A-Z]\w*)\b(.*)$", line)
            if ty:
                items.append((pkg, "type", None, ty.group(1)))
                names.add(ty.group(1))
                if ty.group(2).rstrip().endswith("{"):
                    in_body = ty.group(1)
                continue

            cv = re.match(r"^(const|var) ([A-Z]\w*)", line)
            if cv:
                items.append((pkg, cv.group(1), None, cv.group(2)))
                names.add(cv.group(2))

    return items, fields, names


def derive_env_vars(work, pkgs):
    """Environment variable names read by the public packages.

    `go doc` cannot see these — they are string literals, usually bound to an
    unexported const — so they are read from the source of the same worktree.
    All-caps underscore literals in Go are env names by convention, and in this
    module the set is exactly that: no other literal matches the shape.
    Test files are excluded; a test-harness switch is not runtime surface.
    """
    lit = re.compile(r'"([A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)"')
    found = set()
    for pkg in pkgs:
        d = os.path.join(work, pkg)
        for name in os.listdir(d):
            if not name.endswith(".go") or name.endswith("_test.go"):
                continue
            found.update(lit.findall(open(os.path.join(d, name)).read()))
    return found


def tables(text):
    """Yield (header_cells, [(lineno, row_cells, raw)]) for each markdown table."""
    header, rows = None, []
    for lineno, line in enumerate(text.splitlines(), 1):
        if line.startswith("|"):
            cells = [c.strip() for c in line.strip().strip("|").split("|")]
            if header is None:
                header = cells
            elif set("".join(cells)) <= set("-: "):
                continue                       # separator row
            else:
                rows.append((lineno, cells, line))
            continue
        if header is not None:
            yield header, rows
            header, rows = None, []
    if header is not None:
        yield header, rows


def main():
    repo = os.path.abspath(sys.argv[1] if len(sys.argv) > 1
                           else os.path.join(os.path.dirname(__file__), ".."))
    text = open(os.path.join(repo, "STABILITY.md")).read()

    m = re.search(r"^Snapshot as of:\s*(v\d+\.\d+\.\d+)", text, re.M)
    if not m:
        sys.exit("FAIL: no 'Snapshot as of: vX.Y.Z' line in STABILITY.md")
    tag = m.group(1)
    print(f"snapshot tag from STABILITY.md: {tag}")

    work = tempfile.mkdtemp(prefix="stability-surface-")
    try:
        subprocess.run(["git", "worktree", "add", "--detach", work, tag],
                       cwd=repo, check=True, capture_output=True)
        pkgs = ["."]
        for name in sorted(os.listdir(work)):
            d = os.path.join(work, name)
            if name.startswith(".") or name in NON_PUBLIC_DIRS:
                continue
            if os.path.isdir(d) and any(f.endswith(".go") for f in os.listdir(d)):
                pkgs.append("./" + name)
        print("public packages:", " ".join(pkgs))
        items, fields, names = derive_surface(work, pkgs)
        env_vars = derive_env_vars(work, pkgs)
    finally:
        subprocess.run(["git", "worktree", "remove", "--force", work],
                       cwd=repo, capture_output=True)
        shutil.rmtree(work, ignore_errors=True)

    # Everything the document says in backticks, split into identifiers.
    ticked = set()
    for t in re.findall(r"`([^`]+)`", text):
        ticked.update(re.findall(r"[A-Za-z_]\w*", t))

    fails = 0

    missing = [f"{p} {k} {o + '.' if o else ''}{n}"
               for p, k, o, n in items if n not in ticked]
    print(f"A. derived top-level items: {len(items)}   "
          f"not named in STABILITY.md: {len(missing)}")
    for x in missing:
        print("   MISSING", x)
    fails += len(missing)

    phantom, unassessed, malformed = [], [], []
    for header, rows in tables(text):
        status_col = header and header[-1] == "Status"
        for lineno, cells, raw in rows:
            if len(cells) != len(header):
                malformed.append((lineno, raw))
                continue
            if not status_col:
                continue
            first = cells[0]
            if first.startswith("~~"):          # explicitly marked removed
                continue
            if not cells[-1].startswith(STATUSES):
                unassessed.append((lineno, first, cells[-1]))
            first = re.sub(r"\s*\(.*\)\s*$", "", first)
            for ident in re.findall(r"`([^`]+)`", first):
                for n in (p.strip() for p in ident.split(",")):
                    if n and not ENV_VAR.match(n) and n not in names:
                        phantom.append((lineno, n))

    print(f"B. table identifiers absent from {tag} source: {len(phantom)}")
    for lineno, n in phantom:
        print(f"   PHANTOM {n} (line {lineno})")
    fails += len(phantom)

    print(f"C. malformed table rows: {len(malformed)}")
    for lineno, raw in malformed:
        print(f"   BADROW line {lineno}: {raw[:70]}")
    fails += len(malformed)

    print(f"D. status cells outside {'/'.join(STATUSES)}: {len(unassessed)}")
    for lineno, item, cell in unassessed:
        print(f"   UNASSESSED {item} -> {cell!r} (line {lineno})")
    fails += len(unassessed)

    missing_fields = [f"{p} {t}.{n}" for p, t, n in fields if n not in ticked]
    print(f"E. exported struct fields: {len(fields)}   "
          f"not named in STABILITY.md: {len(missing_fields)}")
    for x in missing_fields:
        print("   MISSING FIELD", x)
    fails += len(missing_fields)

    missing_env = sorted(e for e in env_vars if e not in ticked)
    print(f"F. env vars read by public packages: {len(env_vars)}   "
          f"not named in STABILITY.md: {len(missing_env)}")
    for e in missing_env:
        print("   MISSING ENV", e)
    fails += len(missing_env)

    # G. the counts the document states about itself.
    per_pkg = {}
    for pkg, _, _, _ in items:
        per_pkg[pkg] = per_pkg.get(pkg, 0) + 1
    claimed = re.search(
        r"^(\d+) top-level items at " + re.escape(tag) + r" — (.+?)— counting",
        text, re.M | re.S)
    with_fields = re.search(r"With fields, (\d+)\.", text)
    if not claimed or not with_fields:
        print("G. FAIL: no 'N top-level items at <tag> — …' / "
              "'With fields, N.' sentence found")
        fails += 1
    else:
        stated_total = int(claimed.group(1))
        stated = {}
        for count, name in re.findall(r"(\d+) in `(claudia(?:/\w+)?)`",
                                      claimed.group(2)):
            stated["." if name == "claudia" else "./" + name.split("/")[1]] = int(count)
        bad = ([] if stated_total == len(items)
               else [f"total {stated_total} != derived {len(items)}"])
        if int(with_fields.group(1)) != len(items) + len(fields):
            bad.append(f"with fields {with_fields.group(1)} != derived "
                       f"{len(items) + len(fields)}")
        for pkg in sorted(set(stated) | set(per_pkg)):
            if stated.get(pkg) != per_pkg.get(pkg):
                bad.append(f"{pkg}: stated {stated.get(pkg)} != derived "
                           f"{per_pkg.get(pkg)}")
        print(f"G. surface counts: {len(bad)} mismatch(es) "
              f"(derived {len(items)}: "
              f"{', '.join(f'{k}={v}' for k, v in sorted(per_pkg.items()))})")
        for b in bad:
            print("   COUNT", b)
        fails += len(bad)

    print("RESULT:", "PASS" if fails == 0 else f"FAIL ({fails} violations)")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
