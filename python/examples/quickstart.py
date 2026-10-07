"""obook in a few steps. Each block can be pasted into a notebook cell.

    make python                       # once, from the repository root
    python3 python/examples/quickstart.py
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

import obook  # noqa: E402

# A book. Prices are integer ticks and quantities integer lots; users are integers.
book = obook.Book(max_orders=10_000)

# Two resting sells at different prices, and one more behind the first in time.
a = book.submit(user=1, side=obook.SELL, price=1010, qty=5)
b = book.submit(user=1, side=obook.SELL, price=1011, qty=5)
c = book.submit(user=3, side=obook.SELL, price=1010, qty=2)
print("resting sells:", a.status.name, b.status.name, c.status.name)

# A buy that crosses: it takes 1010 first, oldest order first, then 1011.
r = book.submit(user=2, side=obook.BUY, price=1011, qty=9)
print("buy:", r.status.name)

# Every trade is an event, named by engine order ids, at the maker's price.
for e in book.events():
    if e.kind == obook.EventKind.TRADE:
        print(f"  trade {e.qty} @ {e.price}  maker {e.maker_id}  taker {e.taker_id}")

# What is left, best first.
for o in book.book():
    print(f"  resting {'sell' if o.side else 'buy'} {o.qty - o.filled} @ {o.price}  (order {o.order_id})")

# Refusals are results, not exceptions.
print("cancel a filled order:", book.cancel(a.order_id, user=1).reason.name)
print("post-only that would cross:",
      book.submit(user=2, side=obook.BUY, price=1011, qty=1, post_only=True).reason.name)

# A list of named tuples is one call away from a DataFrame, if pandas is installed:
#   pandas.DataFrame(book.book())

book.close()
