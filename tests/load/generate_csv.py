#!/usr/bin/env python3
"""Deterministic synthetic CSV generator for load tests.

Produces rows with a mix of strings, ints, and booleans so preprocess +
mapping exercises multiple built-in transforms.

Usage:
    python generate_csv.py --rows 10000 --out /tmp/smoke.csv
    python generate_csv.py --rows 10000000 --out /tmp/huge.csv --shards 40
"""
from __future__ import annotations

import argparse
import csv
import os
import random
import string
import sys
from pathlib import Path


COUNTRIES = ["usa", "canada", "uk", "germany", "france", "india", "japan", "brazil"]


def gen_email(i: int) -> str:
    handle = "".join(random.choices(string.ascii_lowercase, k=8))
    return f"{handle}+{i}@example.com"


def write_shard(path: Path, start: int, end: int, header: bool) -> None:
    with path.open("w", newline="") as fp:
        w = csv.writer(fp)
        if header:
            w.writerow(["id", "email", "country", "amount", "active"])
        for i in range(start, end):
            w.writerow([
                i,
                gen_email(i),
                random.choice(COUNTRIES),
                round(random.uniform(1, 10_000), 2),
                "true" if i % 3 != 0 else "false",
            ])


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--rows", type=int, required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--shards", type=int, default=1, help="Split into N files")
    ap.add_argument("--seed", type=int, default=42)
    args = ap.parse_args()
    random.seed(args.seed)

    out: Path = args.out
    out.parent.mkdir(parents=True, exist_ok=True)

    if args.shards == 1:
        write_shard(out, 0, args.rows, header=True)
        print(f"wrote {args.rows} rows to {out}")
        return 0

    stem = out.with_suffix("").name
    parent = out.parent
    per = args.rows // args.shards
    for s in range(args.shards):
        start = s * per
        end = args.rows if s == args.shards - 1 else start + per
        shard_path = parent / f"{stem}_part_{s:03d}.csv"
        write_shard(shard_path, start, end, header=True)
        print(f"  shard {s:03d}: {end - start:,} rows -> {shard_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
