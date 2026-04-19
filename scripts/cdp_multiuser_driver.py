#!/usr/bin/env python3
"""Two-user cloudplay diagnostic driver — drives the dedicated CDP Chrome at
127.0.0.1:9222 across the `games-dev.*` (Claude) and `games-dev2.*` (Claude2)
synthetic-identity tabs.

Prereqs (see /debug-cloudplay-multiuser for the full brief):
- Traefik middleware on queen exposes both subdomains with injected
  X-Auth-Request-User headers (claude-agent / claude-agent-2).
- Dedicated Chrome running at 127.0.0.1:9222 with both tabs open on
  https://games-dev.milvenan.technology/ and https://games-dev2.milvenan.technology/.
- Server has the [0xDE 0xAD 0xBE 0xEF port] synthetic-rumble hook in
  pkg/worker/coordinatorhandlers.go (committed in fe97f08).

Subcommands:
  status              snapshot of both tabs (playing? slot? connected?)
  reload              reload both tabs (picks up fresh JS)
  start-host GAME     click GAME in host tab (substring match, e.g. "NFL Blitz")
  join-joiner GAME SLOT   set joiner slider to SLOT, click GAME
  rumble HOST PORT    inject synthetic rumble on PORT via HOST tab
                      (HOST is 'host' or 'joiner')
  stats               dump data-channel messagesReceived / bytesReceived
  rumble-test         full sweep: port 0 and 1, before/after stats deltas
  eval TAB EXPR       raw JS eval in 'host' or 'joiner' tab

Stats-based observation is the reliable signal: each synthetic rumble
packet arrives on the target's data channel as 5 bytes. Instrumenting JS
onmessage/dispatchEvent on RTCDataChannel does NOT catch native message
dispatch in Chrome — don't waste time wrapping the prototype.
"""
import json, sys, time, urllib.request, socket, base64, os, websocket
from urllib.parse import urlparse

CDP = 'http://127.0.0.1:9222'
HOST = 'games-dev.milvenan.technology'
JOINER = 'games-dev2.milvenan.technology'
TABS = {'host': HOST, 'joiner': JOINER}

def _webrtc_import(tab):
    """Find the app's webrtc singleton by reading the deployed ?v= stamp
    from a <script> src (version.sh stamps all imports to the same sha) and
    importing the exact URL the app used. ES modules cache by URL, so
    matching the stamp gets us the same instance the app is using."""
    stamp = tab.eval(
        "Array.from(document.querySelectorAll('script[src*=\"?v=\"]'))"
        ".map(s => (s.src.match(/\\?v=([a-f0-9]+)/)||[])[1]).find(Boolean) || ''"
    )[1] or ''
    q = f"?v={stamp}" if stamp else ""
    return f"/js/network/webrtc.js{q}"


def list_tabs():
    return json.loads(urllib.request.urlopen(CDP + '/json').read())


def find_tab(host):
    for t in list_tabs():
        if host in t.get('url', ''):
            return t
    raise SystemExit(f'No tab for {host}. Open it in CDP Chrome first.')


class Tab:
    """Minimal CDP client that bypasses Chrome's Origin-header rejection."""

    def __init__(self, host):
        self.host = host
        meta = find_tab(host)
        u = urlparse(meta['webSocketDebuggerUrl'])
        s = socket.create_connection((u.hostname, u.port), timeout=10)
        key = base64.b64encode(os.urandom(16)).decode()
        req = (f'GET {u.path} HTTP/1.1\r\nHost: {u.hostname}:{u.port}\r\n'
               f'Upgrade: websocket\r\nConnection: Upgrade\r\n'
               f'Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n')
        s.sendall(req.encode())
        buf = b''
        while b'\r\n\r\n' not in buf:
            buf += s.recv(4096)
        if b'101' not in buf.split(b'\r\n', 1)[0]:
            raise SystemExit(f'CDP handshake failed: {buf[:200]}')
        self.ws = websocket.WebSocket()
        self.ws.sock = s
        self.ws.connected = True
        self.id = 0

    def send(self, method, params=None):
        self.id += 1
        self.ws.send(json.dumps({'id': self.id, 'method': method, 'params': params or {}}))
        while True:
            msg = json.loads(self.ws.recv())
            if msg.get('id') == self.id:
                return msg

    def eval(self, expr, await_promise=False):
        r = self.send('Runtime.evaluate', {
            'expression': expr,
            'returnByValue': True,
            'awaitPromise': await_promise,
        })
        res = r.get('result', {}).get('result', {})
        if res.get('subtype') == 'error':
            return ('ERROR', res.get('description', ''))
        return ('OK', res.get('value'))

    def reload(self):
        self.send('Page.enable')
        self.send('Page.reload', {'ignoreCache': True})


def dc_stats(tab):
    """Data-channel message counters — the reliable observation signal."""
    url = _webrtc_import(tab)
    return tab.eval(f"""(async () => {{
        const mod = await import('{url}');
        const stats = await mod.webrtc.stats();
        if (!stats) return null;
        let dc = null;
        stats.forEach(s => {{ if (s.type === 'data-channel') dc = s; }});
        return dc && {{received: dc.messagesReceived, bytes: dc.bytesReceived, sent: dc.messagesSent}};
    }})()""", await_promise=True)


def inject_rumble(tab, port):
    """Send the magic 5-byte synthetic rumble packet over the input data channel."""
    url = _webrtc_import(tab)
    return tab.eval(f"""(async () => {{
        const mod = await import('{url}');
        mod.webrtc.input(new Uint8Array([0xDE,0xAD,0xBE,0xEF, {int(port)}]));
    }})()""", await_promise=True)


def tab_status(tab):
    return tab.eval("""({
        url: location.href,
        playing: document.querySelector('#stream')?.currentTime || 0,
        ready: document.querySelector('#stream')?.readyState || 0,
        slot: document.getElementById('playeridx')?.value,
        selectedGame: document.querySelector('.game-select__result.is-selected')?.textContent?.slice(0,60),
    })""")


def click_game(tab, pattern):
    # Search-first UI: results only render once the user types. Stuff
    # the pattern into the input, fire an 'input' event so the fuzzy
    # filter runs synchronously, then click the first matching result.
    return tab.eval(f"""(() => {{
        const input = document.getElementById('game-select-input');
        if (!input) return 'no-input';
        input.value = {json.dumps(pattern)};
        input.dispatchEvent(new Event('input', {{bubbles:true}}));
        const items = Array.from(document.querySelectorAll('.game-select__result'));
        const re = new RegExp({json.dumps(pattern)}, 'i');
        const target = items.find(e => re.test(e.textContent||''));
        if (!target) return 'notfound';
        target.click();
        return 'clicked ' + (target.textContent||'').slice(0,60);
    }})()""")


def set_slot(tab, slot):
    return tab.eval(f"""(() => {{
        const s = document.getElementById('playeridx');
        s.value = {int(slot)};
        s.dispatchEvent(new Event('input', {{bubbles:true}}));
        s.dispatchEvent(new Event('change', {{bubbles:true}}));
        return s.value;
    }})()""")


def cmd_status():
    for name, host in TABS.items():
        t = Tab(host)
        print(f'{name:6} {host}:  {tab_status(t)[1]}')
        print(f'         dc_stats: {dc_stats(t)[1]}')


def cmd_reload():
    for host in TABS.values():
        Tab(host).reload()
        print(f'reloaded {host}')


def cmd_start_host(pattern):
    t = Tab(HOST)
    print('host:', click_game(t, pattern))


def cmd_join_joiner(pattern, slot):
    t = Tab(JOINER)
    print('joiner slot ->', set_slot(t, slot))
    print('joiner click:', click_game(t, pattern))


def cmd_rumble(which, port):
    tab = Tab(TABS[which])
    inject_rumble(tab, port)
    print(f'sent port={port} via {which}')


def cmd_stats():
    for name, host in TABS.items():
        t = Tab(host)
        print(f'{name:6} dc_stats: {dc_stats(t)[1]}')


def cmd_rumble_test():
    """Full sweep: record stats deltas for port 0 and port 1 from host tab."""
    tabs = {name: Tab(host) for name, host in TABS.items()}

    def snap():
        return {n: dc_stats(t)[1] for n, t in tabs.items()}

    def delta(a, b):
        return {n: (None if a[n] is None or b[n] is None else
                    {'dmsgs': b[n]['received'] - a[n]['received'],
                     'dbytes': b[n]['bytes'] - a[n]['bytes']})
                for n in a}

    for port in (0, 1):
        before = snap()
        for _ in range(3):
            inject_rumble(tabs['host'], port)
            time.sleep(0.3)
        time.sleep(0.5)
        after = snap()
        print(f'port={port} delta after 3 sends: {delta(before, after)}')


def cmd_eval(which, expr):
    print(Tab(TABS[which]).eval(expr))


if __name__ == '__main__':
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(0)
    cmd = sys.argv[1]
    args = sys.argv[2:]
    fns = {
        'status': cmd_status,
        'reload': cmd_reload,
        'start-host': lambda: cmd_start_host(args[0]),
        'join-joiner': lambda: cmd_join_joiner(args[0], args[1]),
        'rumble': lambda: cmd_rumble(args[0], args[1]),
        'stats': cmd_stats,
        'rumble-test': cmd_rumble_test,
        'eval': lambda: cmd_eval(args[0], args[1]),
    }
    fn = fns.get(cmd)
    if not fn:
        print(f'unknown cmd: {cmd}')
        print(__doc__)
        sys.exit(1)
    fn()
