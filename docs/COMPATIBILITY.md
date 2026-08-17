# Compatibility — what will not move under you

Author: Karthikeyan NG · Last updated: 2026-08-12

---

## Why this exists

Over two days in August 2026 this repository shipped six releases containing two
breaking wire-protocol bumps, five new methods on an exported interface, a router
that began refusing input it used to accept, new fields on an exported struct, and a
metric-label change that breaks a scraper. Every one of those was defensible. None of
them announced itself as a compatibility event, because the tests were updated in the
same commit as the code — which is exactly what makes a break invisible from inside.

Anyone who integrated on the Monday was broken by the Wednesday. That is the single
biggest thing standing between this library and someone running it, and it is not a
missing feature: it is the absence of a promise.

This page is the promise, and `internal/apicheck` is the part that has teeth.

## What is covered

**Everything exported from `pkg/...`.** Fifteen packages, 1,113 exported
declarations as of this writing. Their names, signatures, struct fields and
interface methods are recorded in
`internal/apicheck/testdata/surface.txt`, and a test fails when any of it changes.

**What is not covered, and never will be:**

- **`internal/...`** — including the wire protocol. It lives under `internal/` so
  that "unsupported, may change" is enforced by the compiler rather than promised in
  a doc. `pkg/orderentry` is the supported surface for order-entry types.
- **`cmd/...`** — the reference binaries. Flags may change. `cmd/obgw` is a
  reference implementation, not a product.
- **`examples/...`** — read them, copy them, do not import them.
- **Wire-format versions.** See below; they have their own rule.

## The rule

Within a major version, code that compiles against `pkg/...` keeps compiling.

Concretely, on covered packages, these are breaking and require a major bump:

- Removing or renaming anything exported.
- Changing a function or method signature.
- Removing a struct field, or changing its type.
- **Adding a method to an exported interface.** This reads as an addition and is a
  break: every implementer outside this repository stops compiling. `CommandLog`
  gained five methods in v0.21.0 and this is the case that taught the lesson — and it
  gained a sixteenth, `AppendSetPhase`, in the unreleased phase-journal work, where
  the lesson was applied rather than re-learned. That break was taken **knowingly and
  in preference to the compatible alternative**: an optional `PhaseLog` interface
  would have broken nobody and would have let an implementer drop phase records by
  omission, which is the durability hole the change existed to close. Recorded here
  because it is the first time this rule has been consulted and then deliberately
  paid: see [`JOURNAL-COMPLETENESS.md`](JOURNAL-COMPLETENESS.md) §4.2.
- Tightening what a function accepts. `Shards` began refusing a second symbol
  without a manifest in v0.22.0 — no signature changed and existing callers broke.

These are not breaking, and land in a minor release:

- Adding a new exported function, type, or struct field.
- Adding a method to an exported *struct* type.
- Behaviour changes that fix a stated contract, with the changelog entry saying so.

## Pre-1.0, honestly

This library is pre-1.0 and semantic versioning lets a pre-1.0 project break anything
at any time. That licence is exactly what produced the two days above, so it is
hereby declined:

**From v0.26.0 onward, a breaking change to a covered package requires a minor
version bump, an entry under a `Changed` heading naming what breaks, and an updated
`surface.txt` in the same commit.** No silent breaks, no "it was pre-1.0."

That is weaker than a 1.0 promise and stronger than what came before. **1.0 is not
being claimed yet, and the reason is honest rather than procedural**: the multi-symbol
surface is days old, the wire moved twice this week, and nobody outside this
repository has integrated against any of it. Freezing an API nobody has used is how
you freeze a mistake permanently. 1.0 becomes reasonable once at least one external
integration exists and has survived a release — see
[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md) on independent review, which is
the same gap wearing a different hat.

## The wire protocol

`internal/wire` is not importable and carries no API promise, but it is a *network*
format, so operators need a different guarantee than programmers do.

The version byte leads every payload. A decoder refuses a version it does not know
rather than misreading it, which is the guarantee that matters: **a client built for
an older version fails loudly, never silently.** There is no multi-version
negotiation and one is not planned — the gateway speaks exactly one version, and an
upgrade is a coordinated one.

Current: **v4**. History is in [PROTOCOL.md](PROTOCOL.md), which records what each
bump bought and why it could not be avoided.

## Metrics

Metric names and label sets are part of the operational contract even though they are
not Go API, because a dashboard breaks as surely as a build does. They are not
covered by `surface.txt` and changes are called out in the changelog. The price
gauges gained a `symbol` label in v0.24.0 and that broke bare-name scrapes; it is the
kind of change that now needs saying out loud.

## Deprecation

A covered symbol that is going away is marked `Deprecated:` in its doc comment, kept
working for at least one minor release, and removed no earlier than the release after
that. `matching.EventBookDelta` is the existing example: declared, never emitted,
and deliberately not deleted, because removing a constant from the middle of an iota
block silently renumbers every value after it.

## How the check works

```sh
go test ./internal/apicheck/                    # fails if the surface moved
APICHECK_UPDATE=1 go test ./internal/apicheck/  # regenerate, then read the diff
```

The regeneration step is the point. The check cannot stop a breaking change and does
not try — it makes one impossible to ship without a human looking at a diff that says
`REMOVED or CHANGED — this breaks code that compiles today`. It is the same device
`internal/wire` uses for the protocol, for the same reason: a promise nobody can
check is a promise that erodes one defensible commit at a time.
