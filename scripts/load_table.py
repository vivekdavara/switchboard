"""Prints the medians of scripts/load.sh's runs as a Markdown table: one row per case, each column the median of its
runs (and the runs' range for throughput). Usage: python3 scripts/load_table.py run/load/results.jsonl"""

import json
import statistics
import sys
from collections import defaultdict


def main(path: str) -> None:
    runs = defaultdict(list)
    with open(path) as f:
        for line in f:
            d = json.loads(line)
            runs[d["case"]].append(d)
    print("| case | runs | requests/s (median, range) | p50 | p90 | p99 | p99.9 | max | not 200 |")
    print("|---|---|---|---|---|---|---|---|---|")
    for case, rs in runs.items():
        med = lambda k: statistics.median(r[k] for r in rs)  # noqa: E731
        rates = [r["per_second"] for r in rs]
        bad = sum(r["requests"] - r["statuses"].get("200", 0) for r in rs)
        ms = lambda k: f"{med(k) / 1000:.3f} ms"  # noqa: E731
        print(f"| {case} | {len(rs)} | {med('per_second'):,.0f} ({min(rates):,.0f} to {max(rates):,.0f}) | "
              f"{ms('p50_us')} | {ms('p90_us')} | {ms('p99_us')} | {ms('p999_us')} | {ms('max_us')} | {bad} |")


if __name__ == "__main__":
    main(sys.argv[1])
