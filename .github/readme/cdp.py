"""Minimal Chrome DevTools Protocol client, standard library only."""
import base64, json, os, socket, struct, subprocess, time, urllib.request

CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"


class CDP:
    def __init__(self, ws_url):
        host_port, path = ws_url[len("ws://"):].split("/", 1)
        host, port = host_port.split(":")
        self.sock = socket.create_connection((host, int(port)))
        key = base64.b64encode(os.urandom(16)).decode()
        req = (f"GET /{path} HTTP/1.1\r\nHost: {host_port}\r\nUpgrade: websocket\r\n"
               f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        self.sock.sendall(req.encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += self.sock.recv(4096)
        assert b" 101 " in buf.split(b"\r\n")[0], buf
        self.rest = buf.split(b"\r\n\r\n", 1)[1]
        self.next_id = 0
        self.events = []

    def _recv_exact(self, n):
        while len(self.rest) < n:
            chunk = self.sock.recv(1 << 20)
            if not chunk:
                raise EOFError
            self.rest += chunk
        out, self.rest = self.rest[:n], self.rest[n:]
        return out

    def _send(self, text):
        data = text.encode()
        head = bytes([0x81])
        n = len(data)
        if n < 126:
            head += bytes([0x80 | n])
        elif n < 65536:
            head += bytes([0x80 | 126]) + struct.pack(">H", n)
        else:
            head += bytes([0x80 | 127]) + struct.pack(">Q", n)
        mask = os.urandom(4)
        self.sock.sendall(head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))

    def _recv_msg(self):
        payload = b""
        while True:
            b0, b1 = self._recv_exact(2)
            n = b1 & 0x7F
            if n == 126:
                n = struct.unpack(">H", self._recv_exact(2))[0]
            elif n == 127:
                n = struct.unpack(">Q", self._recv_exact(8))[0]
            payload += self._recv_exact(n)
            if b0 & 0x80:
                return json.loads(payload)

    def call(self, method, **params):
        self.next_id += 1
        mid = self.next_id
        self._send(json.dumps({"id": mid, "method": method, "params": params}))
        while True:
            msg = self._recv_msg()
            if "method" in msg:
                self.events.append(msg)
                continue
            if msg.get("id") == mid:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg.get("result", {})

    def wait_event(self, method):
        for i, e in enumerate(self.events):
            if e["method"] == method:
                return self.events.pop(i)
        while True:
            msg = self._recv_msg()
            if msg.get("method") == method:
                return msg
            if "method" in msg:
                self.events.append(msg)

    def advance(self, ms):
        self.call("Emulation.setVirtualTimePolicy", policy="advance", budget=ms)
        self.wait_event("Emulation.virtualTimeBudgetExpired")
        self.events.clear()

    def eval(self, expr):
        r = self.call("Runtime.evaluate", expression=expr, returnByValue=True, awaitPromise=True)
        if "exceptionDetails" in r:
            raise RuntimeError(r["exceptionDetails"])
        return r["result"].get("value")


def launch(profile, port=9333, width=1280, height=900):
    proc = subprocess.Popen([CHROME, "--headless=new", f"--remote-debugging-port={port}",
                             f"--user-data-dir={profile}", f"--window-size={width},{height}",
                             "--hide-scrollbars", "--no-first-run", "--no-default-browser-check",
                             "about:blank"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(100):
        try:
            tabs = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/json"))
            page = next(t for t in tabs if t["type"] == "page")
            return proc, CDP(page["webSocketDebuggerUrl"])
        except Exception:
            time.sleep(0.1)
    proc.kill()
    raise RuntimeError("chrome did not start")
