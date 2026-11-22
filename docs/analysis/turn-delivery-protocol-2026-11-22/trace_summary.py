# Summarise a TLC counterexample: one line per step with the action name and
# the variables that changed. Usage: python3 -I trace_summary.py <tlc.log>
import re
import sys

text = open(sys.argv[1]).read()
steps = re.split(r'^State (\d+): ', text, flags=re.M)
prev = {}
for i in range(1, len(steps) - 1, 2):
    num, body = steps[i], steps[i + 1]
    header, _, rest = body.partition('\n')
    action = re.match(r'<(\w+)', header)
    values = {}
    for m in re.finditer(r'^/\\ (\w+) = (.*?)(?=^/\\ |\Z)', rest, flags=re.M | re.S):
        values[m.group(1)] = ' '.join(m.group(2).split())
    changed = {k: v for k, v in values.items() if prev.get(k) != v}
    prev = values
    name = action.group(1) if action else header.strip()
    detail = '; '.join(f'{k}={v}' for k, v in changed.items()) if num != '1' else ''
    print(f'{num:>3} {name}: {detail[:600]}')
