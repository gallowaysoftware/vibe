#!/usr/bin/env python3
"""Minimal Marionette client: drive a real headless Firefox over TCP 2828.

Answers the DOM half of C12 L1 — which controls the page actually renders
under a guest token — with a real browser engine rather than by reading the
HTML source. Run it directly:

    python3 marionette.py http://127.0.0.1:9721/ui/fleet "$TOKEN" out.json

once with the guest token and once with the operator token, and diff.

Every field here names the element it interrogates. A page-wide innerText
regex is NOT a control check: `warm` appears in the footer's warm-target
summary whether or not #warmrow is rendered, so a field called
`warm_row_visible` backed by /warm/i reports the wrong answer as soon as a
warm target exists. `page_text_mentions_warm` is kept, under a name that
says what it is.
"""
import json, socket, subprocess, sys, time, os, shutil

PROFILE = os.environ.get("FLEETLAB_DIR", "/tmp/fleetlab") + "/ffprofile"
PORT = 2828


class M:
    def __init__(self, port=PORT):
        self.msgid = 0
        for _ in range(120):
            try:
                self.s = socket.create_connection(("127.0.0.1", port), timeout=5)
                break
            except OSError:
                time.sleep(0.5)
        else:
            raise SystemExit("marionette never listened")
        self.s.settimeout(60)
        self.buf = b""
        self._read()  # the server's hello frame

    def _read(self):
        while b":" not in self.buf:
            self.buf += self.s.recv(65536)
        n, _, rest = self.buf.partition(b":")
        n = int(n)
        while len(rest) < n:
            rest += self.s.recv(65536)
        self.buf = rest[n:]
        return json.loads(rest[:n])

    def cmd(self, name, params=None):
        self.msgid += 1
        payload = json.dumps([0, self.msgid, name, params or {}]).encode()
        self.s.sendall(str(len(payload)).encode() + b":" + payload)
        msg = self._read()
        if msg[2]:
            raise RuntimeError(f"{name}: {msg[2]}")
        return msg[3]

    def js(self, script, args=None):
        return self.cmd("WebDriver:ExecuteScript",
                        {"script": script, "args": args or [], "newSandbox": False})["value"]


def main():
    url, token, out = sys.argv[1], sys.argv[2], sys.argv[3]
    import socket as _s
    running = False
    try:
        _s.create_connection(("127.0.0.1", PORT), timeout=1).close(); running = True
    except OSError:
        pass
    proc = None
    if not running:
        shutil.rmtree(PROFILE, ignore_errors=True)
        os.makedirs(PROFILE, exist_ok=True)
        proc = subprocess.Popen(
            ["firefox", "--headless", "--marionette", "--no-remote", "--profile", PROFILE,
             "about:blank"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        m = M()
        m.cmd("WebDriver:NewSession", {"capabilities": {}})
        m.cmd("WebDriver:Navigate", {"url": url})
        m.js("window.localStorage.setItem('vibeFleetToken', arguments[0]);", [token])
        m.cmd("WebDriver:Navigate", {"url": url})
        time.sleep(6)
        report = m.js("""
          const vis = el => !!el && !el.hidden && getComputedStyle(el).display !== 'none'
                            && el.offsetParent !== null;
          const btns = [...document.querySelectorAll('button')]
              .filter(b => vis(b)).map(b => b.textContent.trim()).filter(t => t);
          return JSON.stringify({
            title: document.title,
            guest_chip_visible: vis(document.getElementById('guest-chip')),
            guest_chip_text: (document.getElementById('guest-chip')||{}).textContent || null,
            token_gate_visible: vis(document.getElementById('token-gate')),
            visible_buttons: btns,
            cell_rows: document.querySelectorAll('#cells tr, table tbody tr').length,
            body_has_cell_names: ['alpha','bravo','charlie','front']
                .filter(n => document.body.innerText.includes(n)),
            savings_tab_visible: [...document.querySelectorAll('a,button')]
                .some(e => vis(e) && /savings/i.test(e.textContent)),
            savings_nav_visible: vis(document.getElementById('nav-savings')),
            warm_row_visible: vis(document.getElementById('warmrow')),
            warm_row_exists_in_dom: !!document.getElementById('warmrow'),
            // NOT the warm-row check: the footer's "warm targets: …" summary
            // matches this whenever a warm target is configured, guest or not.
            page_text_mentions_warm: /warm/i.test(document.body.innerText),
            body_len: document.body.innerText.length,
          });
        """)
        with open(out, "w") as f:
            f.write(report)
        print(report)
    finally:
        if proc:
            proc.terminate()


if __name__ == "__main__":
    main()
