"""Reads `go test -bench` output on stdin and prints, for every benchmark and metric, the median and the range.

    make bench | tee run/bench.txt | python3 scripts/bench_medians.py

README "Results" quotes medians from this. Standard library only.
"""

import re
import statistics
import sys
from collections import defaultdict

line = re.compile(r"^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+(.*)$")
values = defaultdict(lambda: defaultdict(list))  # benchmark -> unit -> values
order = []
for text in sys.stdin:
    m = line.match(text.strip())
    if not m:
        continue
    name, rest = m.groups()
    if name not in values:
        order.append(name)
    fields = rest.split()
    for value, unit in zip(fields[::2], fields[1::2]):
        values[name][unit].append(float(value))

for name in order:
    parts = []
    for unit, vs in values[name].items():
        parts.append(f"{unit} {statistics.median(vs):g} [{min(vs):g}, {max(vs):g}]")
    n = len(next(iter(values[name].values())))
    print(f"{name} (n={n}): " + "; ".join(parts))
