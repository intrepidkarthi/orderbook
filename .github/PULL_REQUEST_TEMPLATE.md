<!-- Thanks for contributing! Keep PRs small and focused. -->

## What & why

<!-- What does this change, and what problem does it solve? Link any issue. -->

Closes #

## Checklist

- [ ] `make check` is green (tidy · vet · test · race)
- [ ] Added/updated tests (the suite stays green; determinism preserved — no wall-clock or RNG in the match path)
- [ ] The core (`pkg/{types,orderbook,matching}`) still imports nothing above it (strict downward layering)
- [ ] Docs/CHANGELOG updated if this changes public API or behaviour
- [ ] Hot path stays allocation-free (if you touched `Match`/book internals — check with a benchmark)

## Notes for reviewers

<!-- Anything non-obvious: a design trade-off, a benchmark result, a follow-up. -->
