# The slow case, day 4's code against v1.0.0

Run right after the release (16:37 to 16:40 on 2026-10-22, on battery at 85%, nothing else building or testing), to
see whether the release run's +0.42 ms at the median against a ~360 ms provider (README "Results (v1.0.0)"; +0.17 ms
on day 4, on AC power) came from the day's code or from the conditions. Four `RUNS=1 scripts/load.sh` runs,
alternated: day 4's measured commit `e4c1912` (in a worktree of its own), then v1.0.0 (`5f795b6`), twice. Each run
measures every case once, so a run's slow-direct and slow-gateway are adjacent.

```bash
git worktree add --detach .scratch/day4 e4c1912
(cd .scratch/day4 && RUNS=1 scripts/load.sh); RUNS=1 scripts/load.sh   # twice; each keeps run/load/results.jsonl
```

`summary.txt` has each run's slow-case medians and gap, and the one-at-a-time overhead (gateway-c1 minus direct-c1):

| Run | Code | slow gap at p50 | gateway-c1 added |
|---|---|---|---|
| 1 | day 4 (`e4c1912`) | +0.138 ms | 56.0 µs |
| 2 | v1.0.0 | -0.026 ms | 55.6 µs |
| 3 | day 4 | +0.069 ms | 56.7 µs |
| 4 | v1.0.0 | +0.074 ms | 56.0 µs |

So the day's code didn't add anything here: in adjacent runs the two versions are within the same tenth of a
millisecond, on a 360 ms baseline. The release run's 0.29 to 0.44 ms (where `RUNS=3` runs a case three times before the
next case, so a direct run and its gateway run are a minute apart) is then more likely the run's conditions than the
code; what in them, I haven't separated.
