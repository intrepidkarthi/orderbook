# Exchange in a Box — One Command, a Running Venue

Status: **implemented, smoke-tested on CI; step 2.4 of [`ADOPTION-PLAN.md`](ADOPTION-PLAN.md)** ·
Author: Karthikeyan NG · 2026-10-07

## 1. What and why

`docker compose up` starts a small venue with a market already trading, and a page to
watch it in. Several projects in [`LANDSCAPE.md`](LANDSCAPE.md) §7 draw people with
exactly this: a whole exchange they can run before they read any code. Every piece here
already exists except the quoter.

## 2. What runs

| Service | Command | What it does |
|---|---|---|
| `gateway` | `obgw` | The venue: order entry on 9000, market data on 9001, admin `/metrics`, `/healthz` and `/readyz` on 9100. Write-ahead log and snapshot in a named volume, so a restart recovers the book. |
| `marketmaker` | `obquote` (new) | Keeps a ladder of quotes on both sides, refreshed every second around a wandering mid, and leans it against its inventory. |
| `flow` | `obsoak` | Two accounts sending a gentle mix of resting and marketable orders, so the quotes trade. |
| `dashboard` | `obdash` | The operator page of [`CONSOLE-SPEC.md`](CONSOLE-SPEC.md) phase 2, on <http://127.0.0.1:8090>. It reads only what the venue publishes: market data and `/metrics`. |

**The browser console is not in the box.** The page `cmd/obwasm` builds runs its own
engine in WebAssembly and cannot attach to a venue. It is served from the project site.
The dashboard is the console that watches this venue.

**Only the dashboard is published to the host, on 127.0.0.1.** The gateway's ports
stay on the compose network. Opening them is one `ports:` line, and the gateway sends
passwords in plaintext without `-tls-cert`, which it says on startup.

## 3. `cmd/obquote`

```
obquote -addr gateway:9000 -account mm:PASSWORD -symbol BTC-USD -levels 5 -refresh 1s
```

- **Each refresh** cancels the previous ladder and enters a new one: `-levels` bids and
  asks, `-half-spread` ticks from a mid that moves at most `-walk` ticks, `-step`
  apart.
- **The ladder leans** by `-skew` ticks per lot of inventory, so one-sided flow pushes
  the quotes away from it.
- **It logs one line every ten refreshes**: mid, ladder, inventory, fills and rejects.
- **Tested** by a pure ladder test, and by running the real `obgw`, quoting into it, and
  reading the venue's own `orderbook_resting_orders`. Exactly one ladder must be
  resting, so every refresh cancelled the last. Skipping the cancels, flipping the
  ladder, and quoting one side each fail it.

## 4. The image

- **One `Dockerfile`, two stages.** A Go builder compiles `obgw`, `obquote`, `obsoak`
  and `obdash`, with `CGO_ENABLED=0`. A small runtime stage carries the four binaries
  and a shell `wget` for health checks. Each service picks its binary with `command:`.
- **No image is built or run on a maintainer's machine.** Base images are third-party
  code. CI builds and runs the box, and the README tells users how to run it on theirs.

## 5. Accounts

`deploy/accounts` holds three demo accounts, `mm`, `flow0` and `flow1`, in obgw's
`user:sha256:<hex>` form. The matching passwords are in `compose.yaml`, where the
clients need them. They are demo credentials for a venue bound to a private network.
The file says so in its first line.

## 6. Smoke test, on CI

`.github/workflows/box.yml` runs on pushes that touch the box's files, and on dispatch.
It brings the box up and checks:

1. **The gateway is ready**: `/readyz` returns 200 within 60 s.
2. **The book is quoted**: `orderbook_resting_orders` is at least `2 × levels`.
3. **The market trades**: the trade counter rises over 20 s.
4. **The dashboard serves its page and its event stream** on 127.0.0.1:8090.
5. **Recovery**: the gateway is restarted, `/readyz` returns 200 again, and the book is
   not empty.

It prints every service's last log lines whether it passes or fails, and takes the box
down.
