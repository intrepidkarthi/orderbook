# web — live WASM demo

The engine (`cmd/obwasm`) compiled to WebAssembly, driving an interactive,
animated order book in the browser. No build tooling, no npm — just static files
plus a `.wasm`. Deployed to GitHub Pages by `.github/workflows/pages.yml`.

## Run locally

Build the two generated artifacts (git-ignored), then serve the folder over HTTP
(WASM can't load from `file://`):

```sh
GOOS=js GOARCH=wasm go build -o web/obook.wasm ./cmd/obwasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" web/wasm_exec.js   # or misc/wasm on older Go
python3 -m http.server -d web 8080
# open http://localhost:8080
```

## Files

- `index.html` / `style.css` — layout and theme (light + dark).
- `app.js` — boots the WASM engine and drives the ladder, imbalance meter, and
  trade tape via the `obReset` / `obSubmit` / `obSnapshot` JS bridge.
- `obook.wasm`, `wasm_exec.js` — generated; not committed.

## Roadmap

This is the zero-build v1 (Scene 1–3: book, order types, matching, plus a live
imbalance meter). The richer multi-scene React/Vite version in
[`../docs/DEMO-SPEC.md`](../docs/DEMO-SPEC.md) builds on the same WASM bridge.
