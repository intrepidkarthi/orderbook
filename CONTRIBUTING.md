# Contributing

Contributions are welcome — bug fixes, new order types, signals, strategies,
demo scenes, or research write-ups.

## Ground rules

- **Money is never a float.** Prices are `int64` ticks and quantities `int64`
  lots; a per-symbol `Instrument` converts decimals only at the API boundary.
- **The core stays lean.** `pkg/{types,orderbook,matching}` must not import the
  research/sim/strategy/web layers (strict downward layering — see
  [docs/SPEC.md §3](docs/SPEC.md#3-architecture)).
- **Determinism matters.** No wall-clock or RNG in the matching path; anything
  that must be reproducible is injected. This is what keeps replay and backtests
  honest.
- **Everything is tested.** Add tests with your change; keep the suite green.

## Local workflow

```sh
make check     # tidy + vet + test + race
make test      # unit tests
make race      # race detector
make bench     # benchmarks
make demo      # run the CLI demo
```

Or directly: `go test -race ./...`, `go vet ./...`.

## Sending a change

1. Branch from `main`.
2. Keep commits small and focused; write a clear message.
3. Make sure `make check` passes and `gofmt` is clean.
4. Open a PR describing the change and how you verified it.

## Before a large PR — scope & quality bar

For anything beyond a small, obvious fix, **open an issue (or comment on an
existing one) first** to agree on the scope and approach. It saves you from
building the wrong thing, and it keeps review load sane. A few expectations, so
we can say yes:

- **Implement the real thing, not a stub.** A change should actually do what it
  claims. A "FIX adapter" parses and serialises real FIX wire (`tag=value`,
  SOH-delimited) — not a `map[int]string`; a detector detects the real pattern.
  Plausible-looking placeholders that merely compile will be closed.
- **Tests must prove the behaviour.** "It builds and `go test` passes" is not
  enough on its own — tests need to exercise the feature (positive *and* negative
  cases) and preserve determinism.
- **Follow the ground rules above** (int64, downward layering, no clock/RNG in
  the match path); keep it `gofmt`-clean, focused, and documented if it changes
  public API.
- **AI-assisted is welcome — understanding it is required.** Use whatever tools
  you like, but you own what you submit; unreviewed auto-generated PRs that don't
  meet the spec (or that you can't explain in review) will be closed.

New here? The [good first issues](https://github.com/intrepidkarthi/orderbook/labels/good%20first%20issue)
are scoped and labelled for exactly this.

## Ideas looking for an owner

- Protocol codecs — a FIX / OUCH / SBE order-entry adapter (see the open issues).
- A metrics exporter (`expvar` / Prometheus) off the `EventSink` stream.
- A websocket (vs REST-poll) live L2 feed in `cmd/l2capture`.
- More demo scenes (Kyle's λ, delta/CVD, a surveillance visualizer) and
  strategies in `pkg/strategy`.

Browse the [open issues](https://github.com/intrepidkarthi/orderbook/issues)
(`good first issue` / `help wanted`), or start a discussion first for larger
changes.
