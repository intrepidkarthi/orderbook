"""The engine through obook, as a cross-engine adapter (docs/CROSS-ENGINE.md §3).

    go run ./cmd/xeng export internal/benchgate/testdata/bench-basic-v1.obt \\
        | python3 python/examples/xeng_replay.py > python.out
    go run ./cmd/xeng table internal/benchgate/testdata/bench-basic-v1.obt \\
        internal/benchgate/testdata/bench-basic-v1.digest python=python.out

The digest it produces must be the committed one: Python sees the same engine.
"""

import os
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

import obook  # noqa: E402


def main() -> None:
    lines = sys.stdin.read().split("\n")
    n = int(lines[0].split()[1])
    cmds = [l.split() for l in lines[1:] if l.strip()]
    if len(cmds) != n:
        sys.exit(f"xeng_replay: {len(cmds)} commands, header says {n}")

    book = obook.Book(max_orders=1_000_000)
    id_of = [0] * n          # position -> engine id
    refused = [0] * n
    trades = []              # (command, price, qty, maker id, taker id, aggressor)
    rejected = obook.Status.REJECTED
    trade = obook.EventKind.TRADE

    start = time.perf_counter_ns()
    for i, f in enumerate(cmds):
        if f[0] == "S":
            r = book.submit(int(f[2]), int(f[3]), int(f[4]), int(f[5]))
            id_of[i] = r.order_id
        else:
            r = book.cancel(id_of[int(f[3])], int(f[2]))
        refused[i] = 1 if r.status == rejected else 0
        for e in book.events():
            if e.kind == trade:
                trades.append((i, e.price, e.qty, e.maker_id, e.taker_id, e.aggressor))
    elapsed = time.perf_counter_ns() - start

    pos_of = {oid: p for p, oid in enumerate(id_of) if oid}
    out = []
    t = 0
    for i in range(n):
        out.append(f"c {i} {refused[i]}")
        while t < len(trades) and trades[t][0] == i:
            _, price, qty, maker, taker, agg = trades[t]
            out.append(f"X {price} {qty} {pos_of[maker]} {pos_of[taker]} {'S' if agg else 'B'}")
            t += 1
    for o in book.book():
        out.append(f"L {pos_of[o.order_id]} {o.side} {o.price} {o.qty} {o.filled}")
    out.append(f"E {book.last_trade_price()} {len(trades)}")
    out.append(f"T {elapsed}")
    sys.stdout.write("\n".join(out) + "\n")
    book.close()


if __name__ == "__main__":
    main()
