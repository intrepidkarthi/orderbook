#!/usr/bin/env python3
"""Regenerate the README's hero GIF and the social-preview card from the real console.

    python3 .github/readme/capture.py            # both
    python3 .github/readme/capture.py gif        # .github/readme/demo.gif
    python3 .github/readme/capture.py card       # web/og.png

Needs Google Chrome, Go and ffmpeg on PATH; Python's standard library only. The
engine is built to WebAssembly from this checkout into a temporary copy of web/,
served on localhost, and driven over the DevTools protocol under virtual time --
every frame is exactly one 100 ms tick of the page, however long the screenshot
takes, so the same seed records the same film.

The capture hides the page chrome that explains the console to a first-time
visitor (nav, first-run guide, fine print), fixes the grid's height so the frame
does not grow as the tape fills, and tightens the ladder rows so all twenty levels
fit. Every number on screen is the engine's.
"""
import base64, http.server, json, os, shutil, subprocess, sys, tempfile, threading, time

sys.dont_write_bytecode = True
from cdp import launch

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(os.path.dirname(HERE))
SEED = 7
WIDTH = 1100          # CSS px; the grid collapses to one column under 1020
WARMUP_TICKS = 320    # fills the sparklines' history and gives lambda its sample
FRAMES = 170          # 17 s at 10 ticks/s; the GIF keeps every other one
GIF_WIDTH = 1600      # 2x the README's ~800 px column

CSS = """
.nav, .guide, .fineprint { display: none !important; }
.console { padding: 16px !important; }
.con-grid { height: 620px; }
.con-grid > * { min-height: 0; }
.con-grid > .tiles:first-child { display: flex; flex-direction: column; }
.con-grid > .tiles > .panel { flex-shrink: 0; }
.con-grid > .tiles:first-child > .panel:last-child { flex: 1; min-height: 0; flex-shrink: 1 !important; }
.con-grid > .tiles:last-child > .panel:last-child { display: none; }
.tape { max-height: none !important; overflow: hidden !important;
  -webkit-mask-image: linear-gradient(to bottom, #000 80%, transparent); mask-image: linear-gradient(to bottom, #000 80%, transparent); }
.con-grid > section.panel { display: flex; flex-direction: column; }
#ladder { flex: 1; min-height: 0; display: flex; flex-direction: column; justify-content: center; overflow: hidden; padding: 4px 0 !important; }
#ladder > * { flex-shrink: 0; }
#ladder .lrow { margin: 1px 0 !important; padding: 2px 14px !important; }
#ladder .lmid { padding: 5px 14px !important; margin: 2px 0 !important; }
.alist { max-height: 96px !important; overflow: hidden !important; }
"""

# Tick -> what "you" do. The market runs on its own in between.
ACTIONS = {
    # Rest a bid three ticks under the touch: it appears in the book marked with a dot.
    30: """(() => { const s = JSON.parse(obSnapshot(10)); const p = (+s.bids[0].price - 0.03).toFixed(2);
          document.getElementById('t-side').value = 'BUY'; document.getElementById('t-price').value = p;
          document.getElementById('t-qty').value = '2.0';
          document.getElementById('trade-form').requestSubmit(); })()""",
    # Take liquidity: your own prints land on the tape and flash the level.
    60: "document.getElementById('c-buy').click()",
    # Layered size placed and pulled: the shipping SpoofDetector names the account.
    90: "document.getElementById('c-spoof').click()",
}


def build_site(dst):
    web = os.path.join(ROOT, "web")
    for name in os.listdir(web):
        if name.endswith((".html", ".css", ".js")):
            shutil.copy(os.path.join(web, name), dst)
    env = dict(os.environ, GOOS="js", GOARCH="wasm")
    subprocess.run(["go", "build", "-o", os.path.join(dst, "obook.wasm"), "./cmd/obwasm"], cwd=ROOT, env=env, check=True)
    goroot = subprocess.run(["go", "env", "GOROOT"], capture_output=True, text=True, check=True).stdout.strip()
    for rel in ("lib/wasm/wasm_exec.js", "misc/wasm/wasm_exec.js"):
        if os.path.exists(os.path.join(goroot, rel)):
            shutil.copy(os.path.join(goroot, rel), dst)
            break
    shutil.copy(os.path.join(HERE, "og-card.html"), dst)


def serve(root):
    class Quiet(http.server.SimpleHTTPRequestHandler):
        def __init__(self, *a, **kw):
            super().__init__(*a, directory=root, **kw)

        def log_message(self, *a):
            pass

    handler = Quiet
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv, f"http://127.0.0.1:{srv.server_address[1]}"


def screenshot(c, path, clip):
    shot = c.call("Page.captureScreenshot", format="png", clip=clip)
    with open(path, "wb") as f:
        f.write(base64.b64decode(shot["data"]))


def rect(c, js):
    x, y, w, h = json.loads(c.eval(f"JSON.stringify((() => {{ {js} }})())"))
    return dict(x=x, y=y, width=w, height=h, scale=1)


def record(base, work, port):
    frames = os.path.join(work, "frames")
    os.makedirs(frames)
    proc, c = launch(os.path.join(work, "profile"), port=port, width=WIDTH, height=1000)
    try:
        c.call("Emulation.setDeviceMetricsOverride", width=WIDTH, height=1000, deviceScaleFactor=2, mobile=False)
        c.call("Emulation.setEmulatedMedia", features=[{"name": "prefers-color-scheme", "value": "dark"}])
        c.call("Page.enable")
        c.call("Page.navigate", url=f"{base}/console.html?seed={SEED}")
        for _ in range(400):
            if c.eval("!!document.getElementById('ui') && !document.getElementById('ui').hidden"):
                break
            time.sleep(0.05)
        else:
            raise RuntimeError("the console did not boot")
        c.eval(f"(() => {{ const s = document.createElement('style'); s.textContent = {json.dumps(CSS)}; document.head.appendChild(s); }})()")
        c.call("Emulation.setVirtualTimePolicy", policy="pause")
        for _ in range(WARMUP_TICKS):
            c.advance(100)

        pad = 16
        r = rect(c, "const a = document.querySelector('.mkt-bar').getBoundingClientRect();"
                    "const b = document.querySelector('.surv').getBoundingClientRect();"
                    "return [a.x, a.y, a.width, b.bottom - a.y];")
        film = dict(x=max(0, r["x"] - pad), y=max(0, r["y"] - pad), width=r["width"] + 2 * pad, height=r["height"] + 2 * pad, scale=1)
        for i in range(FRAMES):
            if i in ACTIONS:
                c.eval(ACTIONS[i])
            screenshot(c, os.path.join(frames, f"{i:04d}.png"), film)
            c.advance(100)

        fit = json.loads(c.eval("JSON.stringify([...document.querySelectorAll('.con-grid > *, #ladder')].map(e => e.scrollHeight - e.clientHeight))"))
        if any(fit):
            raise RuntimeError(f"a panel overflows the frame by {fit} px; raise .con-grid's height")
        if not c.eval("document.querySelectorAll('#alist .arow').length"):
            raise RuntimeError("the spoof raised no alert; the film would end on nothing")

        ladder = rect(c, "const e = document.getElementById('ladder').closest('.panel').getBoundingClientRect();"
                         "return [e.x, e.y, e.width, e.height];")
        screenshot(c, os.path.join(work, "ladder.png"), ladder)
    finally:
        proc.kill()
    return frames


def make_gif(frames):
    out = os.path.join(HERE, "demo.gif")
    vf = (f"fps=5,scale={GIF_WIDTH}:-1:flags=lanczos,split[a][b];"
          "[a]palettegen=max_colors=256:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle")
    subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-framerate", "10", "-i", os.path.join(frames, "%04d.png"),
                    "-vf", vf, "-final_delay", "250", out], check=True)
    print(f"{os.path.relpath(out, ROOT)}  {os.path.getsize(out) / 1e6:.1f} MB")


def make_card(site, base, work, port):
    shutil.copy(os.path.join(work, "ladder.png"), site)
    proc, c = launch(os.path.join(work, "profile-card"), port=port, width=1280, height=640)
    try:
        c.call("Emulation.setDeviceMetricsOverride", width=1280, height=640, deviceScaleFactor=1, mobile=False)
        c.call("Page.enable")
        c.call("Page.navigate", url=f"{base}/og-card.html")
        for _ in range(100):
            if c.eval("document.readyState === 'complete' && document.querySelector('img').complete"):
                break
            time.sleep(0.05)
        out = os.path.join(ROOT, "web", "og.png")
        screenshot(c, out, dict(x=0, y=0, width=1280, height=640, scale=1))
    finally:
        proc.kill()
    print(f"{os.path.relpath(out, ROOT)}  {os.path.getsize(out) / 1e3:.0f} KB")


def main():
    what = sys.argv[1:] or ["gif", "card"]
    with tempfile.TemporaryDirectory() as work:
        site = os.path.join(work, "site")
        os.makedirs(site)
        build_site(site)
        srv, base = serve(site)
        try:
            frames = record(base, work, 9333)
            if "gif" in what:
                make_gif(frames)
            if "card" in what:
                make_card(site, base, work, 9334)
        finally:
            srv.shutdown()


if __name__ == "__main__":
    main()
