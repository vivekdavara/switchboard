"""Prints the switchboard_* counters and gauges that differ between two scrapes of /metrics (histograms' buckets left
out; their _count and _sum stay). Usage: python3 scripts/metrics_delta.py BEFORE AFTER"""

import sys


def read(path: str) -> dict[str, float]:
    samples = {}
    with open(path) as f:
        for line in f:
            if line.startswith("switchboard_") and "_bucket{" not in line:
                name, value = line.rsplit(" ", 1)
                samples[name] = float(value)
    return samples


def main(before_path: str, after_path: str) -> None:
    before, after = read(before_path), read(after_path)
    for name in sorted(after):
        delta = after[name] - before.get(name, 0)
        if delta != 0 or (name.startswith("switchboard_breaker_state") and after[name] == 1):
            shown = f"{delta:+.6g}" if not name.startswith("switchboard_breaker_state") else f"{after[name]:.0f}"
            print(f"{name} {shown}")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
