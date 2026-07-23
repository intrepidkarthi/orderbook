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

## Ideas looking for an owner

- An `int64` fixed-point "ticks" fast path behind the existing interface.
- A websocket (vs REST-poll) live L2 feed in `cmd/l2capture`.
- More demo scenes (Kyle's λ, delta/CVD, a surveillance visualizer).
- Additional strategies in `pkg/strategy`.

See open issues, or start a discussion first for larger changes.
