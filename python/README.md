# obook

Python bindings for the [orderbook](https://github.com/intrepidkarthi/orderbook)
matching engine, over its C ABI, with the standard library only.

```sh
pip install obook        # Linux x86_64 and aarch64 (glibc 2.28+), macOS 12+ on arm64 and x86_64
```

From a checkout, `make python` builds the library into the package and runs the tests.

```python
import obook

with obook.Book() as b:
    b.submit(user=1, side=obook.SELL, price=1010, qty=5)
    r = b.submit(user=2, side=obook.BUY, price=1012, qty=3)
    print(r.status.name, b.events(), b.book())
```

Prices are integer ticks and quantities integer lots. A refused command is a result
with a `Reason`, not an exception. The full interface, and how it is tested, is in
[docs/PYTHON.md](https://github.com/intrepidkarthi/orderbook/blob/main/docs/PYTHON.md).

