// Host-supplied path rules (jevons 🎯T546 / claudia 🎯T209). Refuse mutating
// tools on matching paths before sandbox write checks.

export type ProtectedPathRule = { pattern: string; reason: string };

const GLOB_META = new Set([".", "+", "^", "$", "{", "}", "(", ")", "|", "[", "]", "\\"]);

export function basenamePath(p: string): string {
  const s = p.replace(/\\/g, "/");
  const i = s.lastIndexOf("/");
  return i >= 0 ? s.slice(i + 1) : s;
}

function globToRegExp(pattern: string): RegExp {
  const pat = pattern.replace(/\\/g, "/");
  if (pat.startsWith("**/") && !pat.slice(3).includes("/") && !pat.slice(3).includes("*")) {
    const suffix = pat.slice(3);
    return new RegExp("(^|/)" + suffix.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "$", "i");
  }
  let re = "^";
  for (let i = 0; i < pat.length; i++) {
    if (pat.startsWith("**", i)) {
      re += ".*";
      i++;
      continue;
    }
    const ch = pat[i]!;
    if (ch === "*") re += "[^/]*";
    else if (GLOB_META.has(ch)) re += "\\" + ch;
    else re += ch;
  }
  return new RegExp(re + "$", "i");
}

// Match repo-relative paths against glob patterns (e.g. double-star bullseye.yaml).
export function matchesProtectedPattern(rel: string, pattern: string): boolean {
  const norm = rel.replace(/\\/g, "/").replace(/^\.\/+/, "");
  const re = globToRegExp(pattern);
  return re.test(norm) || re.test(basenamePath(norm));
}

export function protectedPathRefusal(rel: string, rules: ProtectedPathRule[]): string | undefined {
  for (const r of rules) {
    if (matchesProtectedPattern(rel, r.pattern)) return r.reason;
  }
  return undefined;
}
