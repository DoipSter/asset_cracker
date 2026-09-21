"""Asset Cracker

A phone-shaped desktop widget for Kalshi's 15-minute crypto prediction markets. One page
per coin (Bitcoin and Ethereum), switched by the buttons at the top, each with a live price,
a chart, the round's "price to beat" and a bell to silence notifications. A side panel on
each edge shows that coin's paper-trading strategies, bet log and leaderboard.

Run it:
    python asset_cracker.py

No extra packages needed (Python 3.8+ on Windows 10/11). Prices come from Coinbase's public
API and market data from Kalshi's, so no account or API key is required. Every trade is
simulated: nothing here places a real order.
"""

import base64
import collections
import ctypes
import http.client
import json
import math
import os
import queue
import re
import socket
import ssl
import subprocess
import sys
import threading
import time
import tkinter as tk
import urllib.request
from ctypes import wintypes
from datetime import datetime, timedelta, timezone
from tkinter import font as tkfont

from kalshi_trader import (ANTI_STRATEGIES, START_BALANCE, STRATEGIES, KalshiTrader,
                           parse_amount, strategies)

APP_NAME = "Asset Cracker"
APP_ID = "AssetCracker.App"  # how Windows knows our notifications belong to us

API = "https://api.exchange.coinbase.com/products"
TICKER_EVERY_MS = 500  # Coinbase allows ~10 requests/sec per IP, so this is well inside it
CANDLES_EVERY_MS = 60_000
ALERT_PCT = 1.0  # notify when the price moves this much since the last alert

# name -> (point size in seconds, how many points to show)
RANGES = {"1M": (1, 60), "15M": (60, 15), "1H": (60, 60), "24H": (300, 288), "7D": (3600, 168)}
# 1M is drawn from the live feed rather than Coinbase's candles: the smallest candle they
# serve is a minute, which would be a single point. The feed is already kept per second for
# the window chart, so a minute of it is free.
LIVE_RANGES = {"1M"}
DEFAULT_RANGE = "24H"

# Dark theme
ICON_BG = "#141B2D"  # the navy behind the bell in the app icon
BEZEL = "#2A3145"  # the phone's frame
ISLAND = "#000000"
BG = "#0B0F1A"  # the screen
CARD = "#151B2B"
BUTTON = "#232A3D"
HILITE = "#2C3752"  # a lighter band behind the current window's orders in the log
AMBER = "#F2B84B"  # the soft "a bet was just placed" glow on the Strategies tab
FLASH_SECONDS = 9.0  # how long that glow takes to fade away (one slow pulse, no blinking)


def mix_color(base, top, amount):
    """`base` colour with `amount` (0..1) of `top` blended in, as a hex string."""
    a = max(0.0, min(1.0, amount))
    b = [int(base[i:i + 2], 16) for i in (1, 3, 5)]
    t = [int(top[i:i + 2], 16) for i in (1, 3, 5)]
    return "#" + "".join(f"{round(x + (y - x) * a):02x}" for x, y in zip(b, t))
TEXT = "#F2F5FA"
MUTED = "#8A93A8"
GRID = "#222A3C"
BELL = "#E24B4A"
UP = "#2ECC85"
UP_TINT = "#123A2B"
DOWN = "#FF5C5A"
DOWN_TINT = "#3F1B1E"
TRANSPARENT = "#010203"  # painted outside the rounded shell, then made see-through

W, H = 360, 720  # phone size in logical pixels
TAB = 16  # the side button sticks out this far past the phone's right edge
SIDE = 360  # the side panel is a square this big


def _quad(p0, p1, p2, steps=12):
    """Points along a quadratic Bezier curve (excluding the start point)."""
    pts = []
    for i in range(1, steps + 1):
        t = i / steps
        x = (1 - t) ** 2 * p0[0] + 2 * (1 - t) * t * p1[0] + t ** 2 * p2[0]
        y = (1 - t) ** 2 * p0[1] + 2 * (1 - t) * t * p1[1] + t ** 2 * p2[1]
        pts.append((x, y))
    return pts


# The bell, in the same units as Sean's Noti: x from the center, y from the baseline.
BELL_BODY = (
    [(-9, -43)]
    + _quad((-9, -43), (-9, -61), (0, -61))
    + _quad((0, -61), (9, -61), (9, -43))
    + [(12, -40), (-12, -40)]
)
CLAPPER = (0, -36, 3)  # center x, center y, radius
BELL_CENTER_Y = -47  # vertical middle of bell + clapper


# --------------------------------------------------------------------------
# Windows notifications
# --------------------------------------------------------------------------

_TOAST_SCRIPT = r"""
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$raw = [xml] $template.GetXml()
($raw.toast.visual.binding.text | where { $_.id -eq '1' }).AppendChild($raw.CreateTextNode($env:NOTI_TITLE)) | Out-Null
($raw.toast.visual.binding.text | where { $_.id -eq '2' }).AppendChild($raw.CreateTextNode($env:NOTI_MESSAGE)) | Out-Null
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml($raw.OuterXml)
$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($env:NOTI_APP_ID).Show($toast)
"""

# Used only if we can't register our own app name with Windows.
_POWERSHELL_APP_ID = r"{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe"


def register_app_id():
    """Give our notifications a proper name instead of "Windows PowerShell".
    Writes to HKEY_CURRENT_USER, so no admin needed. Returns the app id to use."""
    try:
        import winreg

        key = winreg.CreateKey(
            winreg.HKEY_CURRENT_USER, rf"Software\Classes\AppUserModelId\{APP_ID}"
        )
        winreg.SetValueEx(key, "DisplayName", 0, winreg.REG_SZ, APP_NAME)
        winreg.CloseKey(key)
        return APP_ID
    except Exception:
        return _POWERSHELL_APP_ID


def send_toast(app_id, title, message):
    """Send a native Windows toast notification. Raises on failure."""
    if sys.platform != "win32":
        raise RuntimeError("Windows notifications only work on Windows.")

    encoded = base64.b64encode(_TOAST_SCRIPT.encode("utf-16-le")).decode("ascii")
    env = dict(os.environ, NOTI_APP_ID=app_id, NOTI_TITLE=title, NOTI_MESSAGE=message)
    result = subprocess.run(
        ["powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded],
        env=env,
        capture_output=True,
        text=True,
        timeout=20,
        creationflags=0x08000000,  # CREATE_NO_WINDOW
    )
    if result.returncode != 0:
        raise RuntimeError((result.stderr or "PowerShell failed").strip().splitlines()[0])


# --------------------------------------------------------------------------
# Prices and settings
# --------------------------------------------------------------------------


def _get_json(path, product="BTC-USD"):
    req = urllib.request.Request(f"{API}/{product}{path}", headers={"User-Agent": "seans-btc/1.0"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)


WS_HOST = "ws-feed.exchange.coinbase.com"


def _ws_recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("socket closed")
        buf += chunk
    return buf


def _ws_send(sock, payload, opcode=1):
    """Send one masked WebSocket frame (clients must mask, per RFC 6455)."""
    n = len(payload)
    if n < 126:
        header = bytes([0x80 | opcode, 0x80 | n])
    elif n < 65536:
        header = bytes([0x80 | opcode, 0x80 | 126]) + n.to_bytes(2, "big")
    else:
        header = bytes([0x80 | opcode, 0x80 | 127]) + n.to_bytes(8, "big")
    mask = os.urandom(4)
    sock.sendall(header + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))


def _ws_read(sock):
    """Read one complete message, answering pings along the way."""
    message = b""
    while True:
        b1, b2 = _ws_recv_exact(sock, 2)
        opcode, n = b1 & 0x0F, b2 & 0x7F
        if n == 126:
            n = int.from_bytes(_ws_recv_exact(sock, 2), "big")
        elif n == 127:
            n = int.from_bytes(_ws_recv_exact(sock, 8), "big")
        if b2 & 0x80:  # servers don't mask, but tolerate it
            _ws_recv_exact(sock, 4)
        payload = _ws_recv_exact(sock, n)
        if opcode == 8:
            raise ConnectionError("server closed the feed")
        if opcode == 9:
            _ws_send(sock, payload, 0xA)  # pong
        elif opcode in (0, 1, 2):
            message += payload
            if b1 & 0x80:  # final fragment
                return message


def _ws_connect():
    raw = socket.create_connection((WS_HOST, 443), timeout=10)
    sock = ssl.create_default_context().wrap_socket(raw, server_hostname=WS_HOST)
    key = base64.b64encode(os.urandom(16)).decode()
    sock.sendall(
        (f"GET / HTTP/1.1\r\nHost: {WS_HOST}\r\nUpgrade: websocket\r\n"
         f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\n"
         f"Sec-WebSocket-Version: 13\r\nUser-Agent: seans-btc/1.0\r\n\r\n").encode()
    )
    head = b""
    while not head.endswith(b"\r\n\r\n"):  # byte by byte so no frame data is swallowed
        head += _ws_recv_exact(sock, 1)
    if b" 101 " not in head.split(b"\r\n", 1)[0]:
        raise ConnectionError("websocket upgrade refused")
    return sock


def stream_prices(on_price, on_offline, product="BTC-USD"):
    """Live trade prices over Coinbase's WebSocket feed: every trade, as it happens.
    Runs on a background thread; calls on_price(price, exchange_time) or on_offline().
    If the feed keeps failing (say, a network that blocks it), falls back to polling."""
    failures = 0
    while failures < 3:
        try:
            sock = _ws_connect()
            sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            sock.settimeout(15)  # these coins trade constantly, so silence means a dead link
            # "matches" is the raw trade stream; "ticker" is the same data but arrives
            # with up to ~150 ms of extra, uneven delay.
            _ws_send(sock, json.dumps(
                {"type": "subscribe", "product_ids": [product], "channels": ["matches"]}
            ).encode())
            while True:
                msg = json.loads(_ws_read(sock))
                if msg.get("type") in ("match", "last_match") and "price" in msg:
                    failures = 0
                    stamp = datetime.fromisoformat(msg["time"].replace("Z", "+00:00"))
                    on_price(float(msg["price"]), stamp.timestamp())
        except Exception:
            failures += 1
            on_offline()
            time.sleep(2)
    _poll_prices(on_price, on_offline, product)


KALSHI_HOST = "api.elections.kalshi.com"
KALSHI_MARKETS = "/trade-api/v2/markets"


def _iso_ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def _market_dict(m):
    """The bits of a Kalshi market the app cares about, as plain numbers."""
    def dollars(key, default=0.0):
        try:
            return float(m[key])
        except (KeyError, TypeError, ValueError):
            return default

    strike = m.get("floor_strike")
    return {
        "ticker": m["ticker"],
        "strike": float(strike) if strike is not None else None,
        "close": _iso_ts(m["close_time"]),
        "yes_bid": dollars("yes_bid_dollars"), "yes_ask": dollars("yes_ask_dollars"),
        "no_bid": dollars("no_bid_dollars"), "no_ask": dollars("no_ask_dollars"),
        # buying Yes takes the yes asks; buying No takes the yes bids
        "yes_ask_size": dollars("yes_ask_size_fp", 1e9),
        "no_ask_size": dollars("yes_bid_size_fp", 1e9),
    }


def _book_quotes(book):
    """Live prices from Kalshi's order book, which lists the bids on each side. Buying
    Yes is met by a No bid (so the Yes ask is 1 - the best No bid), and vice versa."""
    def best(levels):
        parsed = [(float(p), float(q)) for p, q in levels if float(q) > 0]  # skip empty levels
        return max(parsed) if parsed else (0.0, 0.0)

    yes_bid, yes_size = best(book.get("yes_dollars", []))
    no_bid, no_size = best(book.get("no_dollars", []))
    return {
        "yes_bid": yes_bid, "no_bid": no_bid,
        "yes_ask": round(1 - no_bid, 4) if no_bid else 0.0,
        "no_ask": round(1 - yes_bid, 4) if yes_bid else 0.0,
        "yes_ask_size": no_size, "no_ask_size": yes_size,
    }


def _kalshi_get(conn, path):
    conn.request("GET", path, headers={"User-Agent": "seans-btc/1.0"})
    resp = conn.getresponse()
    body = resp.read()  # always drain, or the connection can't be reused
    if resp.status != 200:
        raise RuntimeError(f"Kalshi HTTP {resp.status}")
    return json.loads(body)


_TICKER = re.compile(r"(.+)-(\d\d)([A-Z]{3})(\d\d)(\d\d)(\d\d)-(\d\d)$")


def _next_ticker(ticker):
    """The id of the round that follows this one: e.g. KXBTC15M-26SEP200145-45 is followed by
    KXBTC15M-26SEP200200-00 (the date-time part is the round's closing time, 15 minutes on)."""
    m = _TICKER.match(ticker)
    if not m:
        return None
    series, yy, mon, dd, hh, mm, _ = m.groups()
    try:
        dt = datetime.strptime(f"{yy}{mon}{dd}{hh}{mm}", "%y%b%d%H%M") + timedelta(minutes=15)
    except ValueError:
        return None
    return f"{series}-{dt:%y}{dt.strftime('%b').upper()}{dt:%d%H%M}-{dt:%M}"


def _find_market(conn, series, t):
    """The open round that closes soonest, found via Kalshi's (slowly refreshed) list."""
    data = _kalshi_get(conn, f"{KALSHI_MARKETS}?series_ticker={series}&status=open&limit=10")
    live = [m for m in map(_market_dict, data["markets"])
            if m["strike"] is not None and m["close"] > t]
    return min(live, key=lambda m: m["close"]) if live else None


def stream_kalshi(on_market, on_settled, pending, now, series="KXBTC15M"):
    """Once a second: report the open 15-minute market of `series` and its live prices, and
    check whether any bets that are waiting have settled. Runs on a background thread.
    `pending()` lists (ticker, close time) of open bets; `now()` is the true time."""
    conn = None
    current = None
    just_closed = None  # the round we were watching, until we learn how it settled
    while True:
        started = time.monotonic()
        try:
            if conn is None:
                conn = http.client.HTTPSConnection(KALSHI_HOST, timeout=8)
            t = now()
            # Kalshi's market list (and even the single-market view) is cached for several
            # seconds, so its prices can be badly stale. Use the list only to find which
            # market is open and its strike (those change every 15 minutes), and build
            # the live prices once a second from the order book, which is real time.
            if current is None:
                current = _find_market(conn, series, t)
            elif t >= current["close"]:
                # A round just ended. Kalshi's list takes ~30 seconds to show the next one,
                # but its id is predictable and its own page is live, so ask for it directly
                # (its price to beat appears about 5 seconds after the old round closes).
                if just_closed is None:
                    just_closed = (current["ticker"], current["close"])
                nxt = _next_ticker(current["ticker"])
                found = None
                if nxt:
                    try:
                        m = _market_dict(_kalshi_get(conn, f"{KALSHI_MARKETS}/{nxt}")["market"])
                        if m["strike"] is not None and m["close"] > t:
                            found = m
                    except RuntimeError:
                        pass  # not published yet; try again next second
                if found:
                    current = found
                elif t > current["close"] + 60:  # the prediction isn't working: use the list
                    current = _find_market(conn, series, t)
            if current is not None and current["close"] > t:
                book = _kalshi_get(conn, f"{KALSHI_MARKETS}/{current['ticker']}/orderbook")["orderbook_fp"]
                on_market(dict(current, **_book_quotes(book)))
            # Rounds we hold bets on, plus the one that just closed even if nobody bet it:
            # its settled value is a free measurement of the index, so it is worth asking for.
            watch = dict(pending())
            if just_closed:
                watch.setdefault(*just_closed)
            for ticker, close in watch.items():
                if t > close + 1:  # Kalshi reports results about 5 seconds after close
                    m = _kalshi_get(conn, f"{KALSHI_MARKETS}/{ticker}")["market"]
                    if m.get("result") in ("yes", "no"):
                        on_settled(ticker, m["result"], m.get("expiration_value"), close)
                        if just_closed and ticker == just_closed[0]:
                            just_closed = None
            if just_closed and t > just_closed[1] + 150:
                just_closed = None  # no result after this long: stop asking
        except Exception:
            if conn is not None:
                conn.close()
            conn = None
            time.sleep(2)  # back off before retrying
        time.sleep(max(0.0, 1.0 - (time.monotonic() - started)))


def fetch_recent_offsets(series, product, count=10):
    """How far Kalshi's index has been sitting above our exchange price lately, measured from
    rounds that settled before we started. Each settled value is the index averaged over that
    round's final minute, so compare it with our exchange's own average for the same minute.
    Returns [(close time, offset), ...]."""
    conn = http.client.HTTPSConnection(KALSHI_HOST, timeout=10)
    try:
        data = _kalshi_get(conn, f"{KALSHI_MARKETS}?series_ticker={series}"
                                 f"&status=settled&limit={count}")
    finally:
        conn.close()
    rounds = []
    for m in data["markets"]:
        value = parse_amount(m.get("expiration_value"))
        if value:
            rounds.append((_iso_ts(m["close_time"]), value))  # else: still finalising
    if not rounds:
        return []
    iso = lambda v: datetime.fromtimestamp(v, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    lo = min(c for c, _ in rounds) - 180
    hi = max(c for c, _ in rounds) + 120
    # one candle per minute: [start, low, high, open, close, volume]
    candles = {int(r[0]): r for r in _get_json(
        f"/candles?granularity=60&start={iso(lo)}&end={iso(hi)}", product)}
    out = []
    for close, index_avg in rounds:
        candle = candles.get(int(close) - 60)  # the candle covering that final minute
        if candle and index_avg > 0:
            ours = (candle[3] + candle[4]) / 2  # open and close average out to about its mean
            if ours > 0:
                out.append((close, index_avg / ours - 1))
    return out


def _poll_prices(on_price, on_offline, product="BTC-USD"):
    """Fallback: poll the ticker forever, on one reused connection."""
    host, path = "api.exchange.coinbase.com", f"/products/{product}/ticker"
    conn = None
    while True:
        started = time.monotonic()
        try:
            if conn is None:
                conn = http.client.HTTPSConnection(host, timeout=5)
            conn.request("GET", path, headers={"User-Agent": "seans-btc/1.0"})
            resp = conn.getresponse()
            body = resp.read()  # always drain, or the connection can't be reused
            if resp.status != 200:
                raise RuntimeError(f"HTTP {resp.status}")
            on_price(float(json.loads(body)["price"]), None)
        except Exception:
            if conn is not None:
                conn.close()
            conn = None
            on_offline()
            time.sleep(2)  # back off before retrying
        time.sleep(max(0, TICKER_EVERY_MS / 1000 - (time.monotonic() - started)))


def fetch_history(range_key, product="BTC-USD"):
    """Returns (closes oldest-first, high, low) for the chosen range."""
    granularity, count = RANGES[range_key]
    rows = _get_json(f"/candles?granularity={granularity}", product)
    rows.sort(key=lambda r: r[0])  # each row: time, low, high, open, close, volume
    rows = rows[-count:]
    return [r[4] for r in rows], max(r[2] for r in rows), min(r[1] for r in rows)


def data_dir():
    """Where the paper-trading files live: next to the app (or the script)."""
    if getattr(sys, "frozen", False):
        return os.path.dirname(sys.executable)
    return os.path.dirname(os.path.abspath(__file__))


SETTINGS_PATH = os.path.join(os.environ.get("APPDATA", "."), "AssetCracker", "settings.json")


def _read_settings():
    try:
        with open(SETTINGS_PATH) as f:
            return json.load(f)
    except Exception:
        return {}


def load_muted(coin="BTC"):
    muted = _read_settings().get("muted", False)
    if isinstance(muted, dict):  # {"BTC": bool, "ETH": bool}
        return bool(muted.get(coin, False))
    return bool(muted) if coin == "BTC" else False  # an older file held just one flag: BTC's


def save_muted(coin, value):
    try:
        settings = _read_settings()
        muted = settings.get("muted", {})
        if not isinstance(muted, dict):
            muted = {"BTC": bool(muted)}
        muted[coin] = value
        os.makedirs(os.path.dirname(SETTINGS_PATH), exist_ok=True)
        with open(SETTINGS_PATH, "w") as f:
            json.dump({**settings, "muted": muted}, f)
    except Exception:
        pass  # not being able to remember the setting isn't worth an error


# The coins we track. Each gets its own phone window, side panel, accounts and files.
# The index gap and volatility were measured from a week of Kalshi settlements (Sep 2026).
# Up to five coins, all funded from one balance per strategy. Index offset and volatility
# are per coin; BTC and ETH are measured (see research/), the rest start from BTC's numbers
# and self-calibrate from their own settlements within the hour. `decimals` is how many
# Kalshi quotes that coin's strikes to, which is the precision a price has to be shown
# at for a 15-minute move to be visible at all: DOGE moves in the sixth decimal.
ASSETS = {
    "BTC": dict(coin="BTC", name="Bitcoin", product="BTC-USD", series="KXBTC15M",
                icon="btc.ico", index_offset_pct=0.000057,
                index_sd_pct=0.000144, default_sigma=8e-5, min_pad_pct=0.000187, decimals=2),
    "ETH": dict(coin="ETH", name="Ethereum", product="ETH-USD", series="KXETH15M",
                icon="eth.ico", index_offset_pct=0.0000713,
                index_sd_pct=0.0002156, default_sigma=9.4e-5, min_pad_pct=0.000187, decimals=2),
    "SOL": dict(coin="SOL", name="Solana", product="SOL-USD", series="KXSOL15M",
                icon="eth.ico", index_offset_pct=0.000057,
                index_sd_pct=0.000216, default_sigma=1.1e-4, min_pad_pct=0.00025, decimals=4),
    "XRP": dict(coin="XRP", name="XRP", product="XRP-USD", series="KXXRP15M",
                icon="eth.ico", index_offset_pct=0.000057,
                index_sd_pct=0.000216, default_sigma=1.1e-4, min_pad_pct=0.00025, decimals=4),
    "DOGE": dict(coin="DOGE", name="Dogecoin", product="DOGE-USD", series="KXDOGE15M",
                 icon="eth.ico", index_offset_pct=0.000057,
                 index_sd_pct=0.000216, default_sigma=1.2e-4, min_pad_pct=0.00025, decimals=6),
}


# --------------------------------------------------------------------------
# The app
# --------------------------------------------------------------------------


class Drawing:
    """Canvas helpers shared by the phone and the side panel. Coordinates are in
    logical pixels; `self.k` scales them to the display."""

    def px(self, n):
        return int(round(n * self.k))

    def font(self, size, weight="Semibold"):
        return (f"Segoe UI {weight}".strip(), -self.px(size))

    ox = 0  # logical x offset of everything drawn (a phone with a left-hand tab is shifted right)

    def pts(self, coords):
        return [v * self.k + (self.ox * self.k if i % 2 == 0 else 0)
                for i, v in enumerate(coords)]

    def rrect(self, x1, y1, x2, y2, r, **kw):
        """A rounded rectangle."""
        c = [x1 + r, y1, x2 - r, y1, x2, y1, x2, y1 + r, x2, y2 - r, x2, y2,
             x2 - r, y2, x1 + r, y2, x1, y2, x1, y2 - r, x1, y1 + r, x1, y1]
        return self.canvas.create_polygon(self.pts(c), smooth=True, outline="", **kw)

    def circle(self, cx, cy, r, **kw):
        return self.canvas.create_oval(*self.pts([cx - r, cy - r, cx + r, cy + r]), **kw)

    def text(self, x, y, s, size, fill, weight="Semibold", anchor="center", **kw):
        return self.canvas.create_text(
            (x + self.ox) * self.k, y * self.k, text=s, font=self.font(size, weight),
            fill=fill, anchor=anchor, **kw
        )

    def _button(self, tag, on_click):
        self.canvas.tag_bind(tag, "<Button-1>", lambda e: on_click())
        self.canvas.tag_bind(tag, "<Enter>", lambda e: self.canvas.configure(cursor="hand2"))
        self.canvas.tag_bind(tag, "<Leave>", lambda e: self.canvas.configure(cursor=""))


class Hub:
    """The whole app: one phone with a page per coin (stacked in the same spot, one visible
    at a time) and one side panel per coin (Ethereum's on the left, Bitcoin's on the right).
    Every coin keeps streaming and trading in the background, whichever page is showing."""

    def __init__(self, root, active="BTC"):
        self.root = root
        self.active = active
        self.monitors = {}
        # One panel per world, each following whichever coin page is showing.
        self.panels = {False: None, True: None}
        # One trader for the whole app: each strategy has a single balance that every coin
        # draws on, so a bet on SOL spends the same money as a bet on BTC.
        self.trader = KalshiTrader(data_dir(), ASSETS)

    def monitor(self):
        """The page on screen right now."""
        return self.monitors[self.active]

    def switch(self, coin):
        if coin == self.active or coin not in self.monitors:
            return
        old, new = self.monitors[self.active], self.monitors[coin]
        new.geometry(f"+{old.winfo_x()}+{old.winfo_y()}")  # land exactly where the old page was
        new._style_for_taskbar()
        new.deiconify()
        new.lift()
        old.withdraw()
        self.active = coin
        self.trader.select_coin(coin)  # the panel follows the page you are looking at
        self.redraw_tabs()
        for panel in self.panels.values():
            if panel:
                panel.follow()
                if panel.is_open:
                    panel.refresh()

    def follow(self, moved):
        """The phone was dragged: carry the hidden pages and the open panels along."""
        for m in self.monitors.values():
            if m is not moved:
                m.geometry(f"+{moved.winfo_x()}+{moved.winfo_y()}")
        for panel in self.panels.values():
            if panel:
                panel.follow()

    def toggle_panel(self, anti=False):
        if self.panels[anti] is None:
            self.panels[anti] = SidePanel(self, anti=anti)
        self.panels[anti].toggle()

    def open_panels(self):
        return [p for p in self.panels.values() if p and p.is_open]
        self.redraw_tabs()

    def is_open(self, anti=False):
        p = self.panels[anti]
        return p is not None and p.is_open

    def redraw_tabs(self):
        for m in self.monitors.values():
            m._draw_tab()

    def quit(self):
        self.trader.save(force=True)  # never lose the latest balances
        self.root.destroy()


class Monitor(Drawing, tk.Toplevel):
    """One page of the phone: everything for one coin (feeds, chart, trading accounts).
    All pages have the same size and sit in the same spot; the Hub shows one at a time."""

    def __init__(self, root, asset, hub):
        super().__init__(root)
        self.hub = hub
        self.asset = asset
        self.coin = asset["coin"]
        self.ox = TAB  # room for a tab on each edge: Ethereum's on the left, Bitcoin's on the right
        self.k = self.winfo_fpixels("1i") / 96  # display scaling (1.0 = 100%)
        self.app_id = register_app_id() if sys.platform == "win32" else APP_ID

        self.muted = load_muted(self.coin)
        self.range_key = DEFAULT_RANGE
        self.price = None
        self.ptb = None  # the 15-minute "price to beat"
        self.ptb_close = 0  # when that 15-minute window ends (unix time)
        self.history = []  # closes for the current range
        self.window_pts = collections.deque(maxlen=2400)  # (second, price), ~40 minutes
        self.high = self.low = None
        self.online = False
        self.last_alert_price = None
        self.results = queue.Queue()
        self._history_busy = False
        self._history_timer = None
        self._drag = None
        self.last_trade_ts = None  # exchange timestamp of the price on screen
        self._clock_offsets = collections.deque(maxlen=200)  # exchange time - PC time
        self._chart_dirty = False
        self._chart_drawn_at = 0.0
        self._panel_drawn_at = 0.0
        self._taskbar_styled = False
        self.flash = {}  # strategy name -> when it last placed a bet (for the glow)

        self.title(f"{APP_NAME} · {self.coin}")
        self._set_icon()
        self.overrideredirect(True)  # no title bar: the phone shell is the window
        self.configure(bg=TRANSPARENT)
        self.attributes("-transparentcolor", TRANSPARENT)
        w, h = self.px(W + 2 * TAB), self.px(H)
        # Every page opens in the middle of the screen, leaving room for a panel each side.
        x = (self.winfo_screenwidth() - w) // 2
        y = max(0, (self.winfo_screenheight() - h) // 3)
        self.geometry(f"{w}x{h}+{max(0, x)}+{y}")

        self.canvas = tk.Canvas(self, width=w, height=h, bg=TRANSPARENT, highlightthickness=0)
        self.canvas.pack()
        self.canvas.bind("<ButtonPress-1>", self._drag_start)
        self.canvas.bind("<B1-Motion>", self._drag_move)

        self._draw_static()
        self._draw_tab()
        self._draw_bell_button()
        self._draw_range_pills()
        self._draw_price()
        self._draw_ptb()
        self._draw_chart()
        self._draw_status()

        if self.hub.active != self.coin:
            self.withdraw()  # only one page shows at a time; this one waits behind the scenes
        else:
            self.after(50, self._show_in_taskbar)
        self.after(100, self._pump)
        self.after(1000, self._tick)
        self._start_ticker()
        self._poll_history()
        self._start_kalshi()
        self._seed_trader()
        self._seed_offset()

    # ---- helpers ----------------------------------------------------------

    def now(self):
        """The true time. A PC clock can be seconds off, so correct it using the
        exchange's timestamps; the largest offset is the least-delayed sample."""
        offsets = list(self._clock_offsets)
        return time.time() + (max(offsets) if offsets else 0)

    def _set_icon(self):
        try:  # bundled by PyInstaller into sys._MEIPASS; next to the script otherwise
            base = getattr(sys, "_MEIPASS", os.path.dirname(os.path.abspath(__file__)))
            self.iconbitmap(os.path.join(base, self.asset["icon"]))
        except tk.TclError:
            pass

    @property
    def trader(self):
        """The one trader every page shares."""
        return self.hub.trader

    @property
    def state(self):
        """This coin's prices, volatility and index calibration."""
        return self.hub.trader.coins[self.coin]

    @property
    def panel(self):
        """The single side panel, once it has been opened."""
        return self.hub.panel

    def _style_for_taskbar(self):
        """Borderless windows are hidden from the taskbar unless we ask nicely."""
        if self._taskbar_styled:
            return
        try:
            user32 = ctypes.windll.user32
            user32.GetParent.argtypes = [wintypes.HWND]
            user32.GetParent.restype = wintypes.HWND
            user32.GetWindowLongW.argtypes = [wintypes.HWND, ctypes.c_int]
            user32.SetWindowLongW.argtypes = [wintypes.HWND, ctypes.c_int, ctypes.c_long]
            hwnd = user32.GetParent(self.winfo_id())
            style = user32.GetWindowLongW(hwnd, -20)  # GWL_EXSTYLE
            style = (style & ~0x80) | 0x40000  # drop TOOLWINDOW, add APPWINDOW
            user32.SetWindowLongW(hwnd, -20, style)
            self._taskbar_styled = True
        except Exception:
            pass

    def _show_in_taskbar(self):
        self._style_for_taskbar()
        self.withdraw()  # the style only takes effect after a hide and show
        self.after(10, self.deiconify)

    # ---- dragging (the window has no title bar) ---------------------------

    def _drag_start(self, e):
        current = self.canvas.find_withtag("current")
        if not current or "btn" in self.canvas.gettags(current[0]):
            self._drag = None
            return
        self._drag = (e.x_root - self.winfo_x(), e.y_root - self.winfo_y())

    def _drag_move(self, e):
        if self._drag:
            self.geometry(f"+{e.x_root - self._drag[0]}+{e.y_root - self._drag[1]}")
            self.hub.follow(self)  # the other page and any open panels travel with it

    def quit_app(self):
        self.hub.quit()

    # ---- drawing ----------------------------------------------------------

    def _draw_static(self):
        c = self.canvas
        self.rrect(0, 0, W, H, 54, fill=BEZEL)  # the frame
        self.rrect(6, 6, W - 6, H - 6, 48, fill=BG)  # the screen
        self.rrect(W / 2 - 52, 20, W / 2 + 52, 46, 13, fill=ISLAND)  # the island
        self.rrect(W / 2 - 62, H - 28, W / 2 + 62, H - 22, 3, fill=MUTED)  # home bar

        # The page switch: one pill per coin. Tickers rather than names, because five
        # names will not fit across a phone.
        # The close button sits at x=42 and the bell at x=W-46, so the pills get the strip
        # between them rather than the full width.
        coins = list(ASSETS)
        left, right, gap = 62, W - 70, 3
        wide = (right - left - gap * (len(coins) - 1)) / len(coins)
        for i, coin in enumerate(coins):
            x1 = left + i * (wide + gap)
            active = coin == self.coin
            tags = ("btn", f"page_{coin}")
            self.rrect(x1, 71, x1 + wide, 97, 12, fill=TEXT if active else BUTTON, tags=tags)
            self.text(x1 + wide / 2, 84, coin, 10, BG if active else MUTED, tags=tags)
            self._button(f"page_{coin}", lambda c=coin: self.hub.switch(c))

        # close button, top left
        self.circle(42, 84, 15, fill=BUTTON, width=0, tags=("btn", "close"))
        for a, b in (((-5, -5), (5, 5)), ((-5, 5), (5, -5))):
            c.create_line(*self.pts([42 + a[0], 84 + a[1], 42 + b[0], 84 + b[1]]),
                          fill=MUTED, width=self.px(2), capstyle="round",
                          tags=("btn", "close"))
        self._button("close", self.quit_app)

        # "BTC index  · live" caption (the settlement index, as the prediction market uses)
        self.text(W / 2 - 4, 122, f"{self.coin} index", 12, MUTED, anchor="e")
        self.live_dot = self.circle(W / 2 + 8, 122, 4, fill=MUTED, width=0)
        self.live_text = self.text(W / 2 + 16, 122, "connecting", 12, MUTED, anchor="w")

        # the chart card
        self.rrect(24, 294, W - 24, 582, 28, fill=CARD)

    def _draw_bell_button(self):
        c = self.canvas
        c.delete("bell")
        cx, cy, sc = W - 46, 84, 0.85
        self.circle(cx, cy, 20, fill=CARD, width=0, tags=("btn", "bell"))
        color = MUTED if self.muted else BELL
        body = []
        for x, y in BELL_BODY:
            body += [cx + x * sc, cy + (y - BELL_CENTER_Y) * sc]
        c.create_polygon(self.pts(body), fill=color, outline="", tags=("btn", "bell"))
        x, y, r = CLAPPER
        self.circle(cx + x * sc, cy + (y - BELL_CENTER_Y) * sc, r * sc, fill=color,
                    width=0, tags=("btn", "bell"))
        if self.muted:  # a slash through it
            c.create_line(*self.pts([cx - 11, cy - 11, cx + 11, cy + 11]), fill=CARD,
                          width=self.px(6), capstyle="round", tags=("btn", "bell"))
            c.create_line(*self.pts([cx - 11, cy - 11, cx + 11, cy + 11]), fill=TEXT,
                          width=self.px(2.5), capstyle="round", tags=("btn", "bell"))
        self._button("bell", self.toggle_mute)

    def _draw_tab(self):
        """The side buttons. Right opens the strategies, left opens their anti-world twins;
        both follow whichever coin's page you are looking at. The left one is tinted in the
        DOWN colour so the two worlds are never confused at a glance."""
        c = self.canvas
        c.delete("tab")
        top, bottom = 196, 296
        mid = (top + bottom) / 2
        for anti in (False, True):
            opened = self.hub.is_open(anti)
            name = "tab_anti" if anti else "tab"
            tags = ("btn", "tab", name)
            live = DOWN if anti else UP
            if anti:
                self.rrect(-TAB, top, 4, bottom, 7, fill=live if opened else BEZEL, tags=tags)
                x, d = -6, -1  # the chevron points away from the phone to open
            else:
                self.rrect(W - 4, top, W + TAB, bottom, 7, fill=live if opened else BEZEL,
                           tags=tags)
                x, d = W + 6, 1
            if opened:
                d = -d
            c.create_line(*self.pts([x - 2 * d, mid - 7, x + 2 * d, mid, x - 2 * d, mid + 7]),
                          fill=BG if opened else TEXT, width=self.px(2), capstyle="round",
                          joinstyle="round", tags=tags)
            self._button(name, lambda a=anti: self.hub.toggle_panel(a))

    def _draw_range_pills(self):
        self.canvas.delete("pills")
        # sized so the whole row still fits the phone's width as ranges are added
        w, gap, top, bottom = 56, 6, 596, 632
        x = (W - (w * len(RANGES) + gap * (len(RANGES) - 1))) / 2
        for key in RANGES:
            active = key == self.range_key
            tags = ("btn", "pills", f"range_{key}")
            self.rrect(x, top, x + w, bottom, 18, fill=TEXT if active else CARD, tags=tags)
            self.text(x + w / 2, (top + bottom) / 2, key, 13,
                      BG if active else TEXT, tags=tags)
            self._button(f"range_{key}", lambda k=key: self.set_range(k))
            x += w + gap

    def _draw_price(self):
        """The headline price is the index the prediction market runs on, not our exchange's
        last trade, so it lines up with what Coinbase Predictions shows. The raw exchange
        price is still what the model works from; only the display is lifted."""
        c = self.canvas
        c.delete("price")
        if self.price is None:
            self.text(W / 2, 168, "$ ———", 44, MUTED, tags="price")
            return
        shown = self.price * (1 + self.state.offset_pct)
        dec = self.asset["decimals"]
        self.text(W / 2, 168, f"${shown:,.{dec}f}", 44, TEXT, tags="price")

        # 1M has no candle history behind it, so its change comes from the live points
        if self.range_key in LIVE_RANGES:
            window = [p for t, p in self.window_pts if t >= self.now() - 60]
            base = window[0] if len(window) >= 2 else None
        else:
            base = self.history[0] if len(self.history) >= 2 else None
        if base:
            change = (self.price - base) / base * 100
            good = change >= 0
            # a minute's move is a hundredth of a percent or so, and would round to 0.00
            places = 3 if self.range_key in LIVE_RANGES else 2
            label = f"{'▲' if good else '▼'} {abs(change):.{places}f}%  ·  {self.range_key}"
            width = tkfont.Font(family="Segoe UI Semibold", size=-self.px(13)).measure(label)
            half = width / self.k / 2 + 16
            self.rrect(W / 2 - half, 200, W / 2 + half, 228, 14,
                       fill=UP_TINT if good else DOWN_TINT, tags="price")
            self.text(W / 2, 214, label, 13, UP if good else DOWN, tags="price")

    def _draw_ptb(self):
        """The 15-minute 'price to beat' row, with a countdown to the window's end."""
        c = self.canvas
        c.delete("ptb")
        top, bottom = 240, 286
        mid = top + 15
        self.rrect(24, top, W - 24, bottom, 18, fill=CARD, tags="ptb")
        if self.ptb is None:
            self.text(W / 2, (top + bottom) / 2, "Price to beat  —", 12, MUTED, tags="ptb")
            return
        # Kalshi settles on an index that runs a little above Coinbase's price, so compare
        # like with like: our estimate of that index against the price to beat.
        offset, samples = self.state.offset_status()
        est = self.price * (1 + offset) if self.price is not None else None
        state = MUTED if est is None else (UP if est >= self.ptb else DOWN)
        self.circle(42, mid, 4, fill=state, width=0, tags="ptb")
        self.text(54, mid, "Price to beat", 11, MUTED, weight="", anchor="w", tags="ptb")
        self.text(W / 2 + 34, mid, f"${self.ptb:,.{self.asset['decimals']}f}", 14, TEXT,
                  tags="ptb")
        left = int(self.ptb_close - self.now())
        clock = f"{left // 60}:{left % 60:02d}" if left > 0 else "settling"
        self.text(W - 40, mid, clock, 12, MUTED, anchor="e", tags="ptb")
        if est is not None:
            # The headline price is already the index, so only the distance to the target is
            # worth repeating. Say so in words when the gap is still assumed rather than
            # measured -- a bare tick mark is too cryptic to decode at this size.
            diff = est - self.ptb
            label = (f"{'above' if diff >= 0 else 'below'} target by "
                     f"${abs(diff):,.{self.asset['decimals']}f}")
            if samples < 3:
                label += "  ·  index gap still estimated"
            self.text(W / 2, top + 33, label, 10, state, weight="", tags="ptb")

    def _draw_window_chart(self):
        """The 15M view: the live Kalshi window from open to close, with the price to
        beat and a marker for every bet the selected strategy placed in it."""
        c = self.canvas
        left, right, top, bottom = 46, W - 46, 322, 518
        if self.ptb is None or not self.window_pts:
            self.text(W / 2, 420, "Loading window…", 14, MUTED, weight="", tags="chart")
            return
        close_t = self.ptb_close
        open_t = close_t - 900
        pts = [(t, p) for t, p in self.window_pts if open_t <= t <= close_t]
        bets = [lot for lot in self.trader.account().log
                if lot["close"] == close_t and lot.get("coin") == self.coin]
        if not pts:
            self.text(W / 2, 420, "Window just opened…", 14, MUTED, weight="", tags="chart")
            return

        # Draw on the scale of Kalshi's index (Coinbase's price nudged up by its usual gap),
        # since that's what the price to beat and the settlement use.
        lift = 1 + self.state.offset_pct
        dec = self.asset["decimals"]
        levels = [p * lift for _, p in pts] + [self.ptb]
        for lot in bets:
            levels += [lot["btc_price"] * lift]  # (the key says btc but holds either coin)
            levels += [lot["exit_btc"] * lift] if lot.get("exit_btc") else []
        lo, hi = min(levels), max(levels)
        pad = max((hi - lo) * 0.14, self.ptb * self.asset["min_pad_pct"])
        lo, hi = lo - pad, hi + pad

        def X(t):
            return left + (right - left) * (t - open_t) / 900

        def Yi(p):  # p is already on the index's scale
            return bottom - (bottom - top) * (p - lo) / (hi - lo)

        def Y(p):  # p is a Coinbase price
            return Yi(p * lift)

        good = pts[-1][1] * lift >= self.ptb
        line, tint = (UP, UP_TINT) if good else (DOWN, DOWN_TINT)
        for i in range(4):  # faint gridlines
            y = top + (bottom - top) * i / 3
            c.create_line(*self.pts([left, y, right, y]), fill=GRID, width=self.px(1), tags="chart")

        step = max(1, len(pts) // 280)  # a thinner line is plenty and much cheaper to draw
        shown = pts[::step] + ([pts[-1]] if (len(pts) - 1) % step else [])
        coords = [v for t, p in shown for v in (X(t), Y(p))]
        if len(shown) >= 2:
            c.create_polygon(self.pts(coords + [coords[-2], bottom, coords[0], bottom]),
                             fill=tint, outline="", tags="chart")
            c.create_line(*self.pts(coords), fill=line, width=self.px(2.2), capstyle="round",
                          joinstyle="round", tags="chart")

        y = Yi(self.ptb)  # the price to beat
        c.create_line(*self.pts([left, y, right, y]), fill=TEXT, width=self.px(1.5),
                      dash=(self.px(5), self.px(4)), tags="chart")
        self.text(right, y - 8,
                  f"Price to beat  ${self.ptb:,.{self.asset['decimals']}f}", 10, MUTED,
                  weight="",
                  anchor="e", tags="chart")
        ex, ey = X(pts[-1][0]), Y(pts[-1][1])
        self.circle(ex, ey, 6.5, fill=CARD, width=0, tags="chart")
        self.circle(ex, ey, 4, fill=line, width=0, tags="chart")

        labels = []  # boxes already taken by bet labels, so neighbours don't overlap
        for lot in bets:  # where each bet went in
            x, yb = X(lot["t"]), Y(lot["btc_price"])
            col = UP if lot["side"] == "UP" else DOWN
            c.create_line(*self.pts([x, yb, x, bottom]), fill=col, width=self.px(1),
                          dash=(self.px(2), self.px(3)), tags="chart")
            d = -1 if lot["side"] == "UP" else 1  # up-triangle for UP, down for DOWN
            c.create_polygon(self.pts([x, yb + d * 8, x - 6.5, yb - d * 4, x + 6.5, yb - d * 4]),
                             fill=col, outline=CARD, width=self.px(1.5), tags="chart")
            ax, lx = ("e", x - 9) if x > right - 80 else ("w", x + 9)
            x0, x1 = (lx, lx + 96) if ax == "w" else (lx - 96, lx)
            for dy in (-28, 28, -54, 54, -80, 80):  # first spot that's free
                ly = min(max(yb + dy, top + 10), bottom - 10)
                if not any(x0 < bx1 and bx0 < x1 and ly - 13 < by1 and by0 < ly + 13
                           for bx0, by0, bx1, by1 in labels):
                    break
            labels.append((x0, ly - 13, x1, ly + 13))
            conf = f" · {lot['model_prob'] * 100:.0f}%" if lot.get("model_prob") else ""
            self.text(lx, ly - 6, f"{lot['side']} {lot['contracts']}×{lot['price'] * 100:.0f}¢{conf}",
                      10, col, anchor=ax, tags="chart")
            self.text(lx, ly + 6, f"{self.coin} ${lot['btc_price']:,.{dec}f}", 10, MUTED, weight="",
                      anchor=ax, tags="chart")
            if lot.get("status") == "sold" and lot.get("exit_btc"):  # an early sale
                sx, sy = X(lot["exit_t"]), Y(lot["exit_btc"])
                self.circle(sx, sy, 5, fill=BG, outline=col, width=self.px(2), tags="chart")
                self.text(sx, sy + 13, f"sold {lot['pnl']:+.2f}", 9, col, weight="", tags="chart")

        for t, anchor, x in ((open_t, "w", left), (close_t, "e", right)):
            stamp = datetime.fromtimestamp(t).strftime("%I:%M %p").lstrip("0")
            self.text(x, 530, stamp, 10, MUTED, weight="", anchor=anchor, tags="chart")
        ps = [p * lift for _, p in pts]
        self.text(46, 552, f"Low  ${min(ps):,.{dec}f}", 11, MUTED, anchor="w", tags="chart")
        self.text(W - 46, 552, f"High  ${max(ps):,.{dec}f}", 11, MUTED, anchor="e", tags="chart")

    def _draw_minute_chart(self):
        """The 1M view: the last sixty seconds, one point per second, straight off the feed.

        At this zoom the price barely moves, so the scale has to fit the data rather than
        the price to beat -- a target far away would flatten a whole minute into a
        straight line. The target is still drawn when it happens to fall inside the band.
        """
        c = self.canvas
        left, right, top, bottom = 46, W - 46, 322, 518
        start = self.now() - 60
        pts = [(t, p) for t, p in self.window_pts if t >= start]
        if len(pts) < 2:
            self.text(W / 2, 420, "Listening…", 14, MUTED, weight="", tags="chart")
            return

        lift = 1 + self.state.offset_pct
        dec = self.asset["decimals"]
        levels = [p * lift for _, p in pts]
        lo, hi = min(levels), max(levels)
        # a minute can be genuinely flat, so fall back to the coin's own minimum padding
        pad = max((hi - lo) * 0.18, hi * self.asset["min_pad_pct"] * 0.5)
        lo, hi = lo - pad, hi + pad

        def X(t):
            return left + (right - left) * (t - start) / 60

        def Yi(p):
            return bottom - (bottom - top) * (p - lo) / (hi - lo)

        good = levels[-1] >= levels[0]
        line, tint = (UP, UP_TINT) if good else (DOWN, DOWN_TINT)
        for i in range(4):  # faint gridlines
            y = top + (bottom - top) * i / 3
            c.create_line(*self.pts([left, y, right, y]), fill=GRID, width=self.px(1),
                          tags="chart")

        coords = [v for (t, _), p in zip(pts, levels) for v in (X(t), Yi(p))]
        c.create_polygon(self.pts(coords + [coords[-2], bottom, coords[0], bottom]),
                         fill=tint, outline="", tags="chart")
        c.create_line(*self.pts(coords), fill=line, width=self.px(2.5),
                      capstyle="round", joinstyle="round", tags="chart")

        # One dot per second. At this zoom they are individually visible, so a gap in the
        # dots is a gap in the feed -- which is worth being able to see at a glance.
        for x, y in zip(coords[::2], coords[1::2]):
            self.circle(x, y, 1.8, fill=line, width=0, tags="chart")
        self.circle(coords[-2], coords[-1], 7, fill=CARD, width=0, tags="chart")
        self.circle(coords[-2], coords[-1], 4.5, fill=line, width=0, tags="chart")

        if self.ptb is not None and lo <= self.ptb <= hi:  # only when it is actually in view
            y = Yi(self.ptb)
            c.create_line(*self.pts([left, y, right, y]), fill=TEXT, width=self.px(1.5),
                          dash=(self.px(5), self.px(4)), tags="chart")
            self.text(right, y - 8, f"Price to beat  ${self.ptb:,.{dec}f}", 10, MUTED,
                      weight="", anchor="e", tags="chart")

        self.text(46, 552, f"Low  ${min(levels):,.{dec}f}", 11, MUTED, anchor="w",
                  tags="chart")
        self.text(W - 46, 552, f"High  ${max(levels):,.{dec}f}", 11, MUTED, anchor="e",
                  tags="chart")
        # 60 would be a full minute with no missed seconds; fewer means a quiet or laggy feed
        self.text(W / 2, 552, f"{len(pts)}/60 ticks", 11, MUTED, weight="", tags="chart")

    def _draw_chart(self):
        c = self.canvas
        c.delete("chart")
        if self.range_key == "15M":
            self._draw_window_chart()
            return
        if self.range_key == "1M":
            self._draw_minute_chart()
            return
        left, right, top, bottom = 46, W - 46, 322, 518
        pts = list(self.history)
        if len(pts) < 2:
            msg = "Loading chart…" if self.online or self.price is None else "No data"
            self.text(W / 2, 420, msg, 14, MUTED, weight="", tags="chart")
            return

        lo, hi = min(pts), max(pts)
        pad = (hi - lo) * 0.1 or 1
        lo, hi = lo - pad, hi + pad
        good = pts[-1] >= pts[0]
        line, tint = (UP, UP_TINT) if good else (DOWN, DOWN_TINT)

        for i in range(4):  # faint gridlines
            y = top + (bottom - top) * i / 3
            c.create_line(*self.pts([left, y, right, y]), fill=GRID,
                          width=self.px(1), tags="chart")

        coords = []
        for i, p in enumerate(pts):
            coords += [left + (right - left) * i / (len(pts) - 1),
                       bottom - (bottom - top) * (p - lo) / (hi - lo)]
        c.create_polygon(self.pts(coords + [right, bottom, left, bottom]), fill=tint,
                         outline="", tags="chart")
        c.create_line(*self.pts(coords), fill=line, width=self.px(2.5),
                      capstyle="round", joinstyle="round", tags="chart")
        ex, ey = coords[-2], coords[-1]
        self.circle(ex, ey, 7, fill=CARD, width=0, tags="chart")
        self.circle(ex, ey, 4.5, fill=line, width=0, tags="chart")

        if self.low is not None:  # on the index's scale, to match the headline price
            dec, lift = self.asset["decimals"], 1 + self.state.offset_pct
            self.text(46, 552, f"Low  ${self.low * lift:,.{dec}f}", 11, MUTED, anchor="w",
                      tags="chart")
            self.text(W - 46, 552, f"High  ${self.high * lift:,.{dec}f}", 11, MUTED, anchor="e",
                      tags="chart")

    def _draw_status(self):
        self.canvas.delete("status")
        if self.last_trade_ts:  # the trade's own time, to compare against another app
            d = datetime.fromtimestamp(self.last_trade_ts)
            stamp = f"{d:%I:%M:%S}.{d.microsecond // 1000:03d} {d:%p}".lstrip("0")
        else:
            stamp = "—"
        self.text(W / 2, 654, f"Last trade {stamp}", 11, MUTED, weight="", tags="status")
        alerts = ("Alerts muted" if self.muted
                  else f"Alerts on  ·  notify on {ALERT_PCT:g}% moves")
        self.text(W / 2, 676, alerts, 11, MUTED if self.muted else TEXT, tags="status")

    def _set_live(self, online):
        self.online = online
        color = UP if online else MUTED
        self.canvas.itemconfigure(self.live_dot, fill=color)
        self.canvas.itemconfigure(
            self.live_text, text="live" if online else "offline", fill=color
        )

    # ---- actions ----------------------------------------------------------

    def toggle_mute(self):
        self.muted = not self.muted
        save_muted(self.coin, self.muted)
        if not self.muted:  # start fresh so we don't fire on an old baseline
            self.last_alert_price = self.price
        self._draw_bell_button()
        self._draw_status()

    def set_range(self, key):
        if key == self.range_key:
            return
        self.range_key = key
        self.history, self.high, self.low = [], None, None
        self._draw_range_pills()
        self._draw_price()
        self._draw_chart()
        self._poll_history()

    def _maybe_alert(self, price):
        if self.last_alert_price is None:
            self.last_alert_price = price
            return
        change = (price - self.last_alert_price) / self.last_alert_price * 100
        if self.muted or abs(change) < ALERT_PCT:
            return
        self.last_alert_price = price
        title = f"{self.asset['name']} is {'up' if change > 0 else 'down'} {abs(change):.1f}%"

        def worker():
            try:
                send_toast(self.app_id, title,
                           f"Now ${price:,.{self.asset['decimals']}f}")
            except Exception:
                pass  # a missed toast shouldn't disturb the display

        threading.Thread(target=worker, daemon=True).start()

    # ---- fetching (on background threads, results come back via a queue) --

    def _start_ticker(self):
        def on_price(price, exchange_ts):
            if exchange_ts:  # sampled here, at arrival, so it isn't skewed by the UI queue
                self._clock_offsets.append(exchange_ts - time.time())
            self.results.put(("price", price, exchange_ts))

        threading.Thread(
            target=stream_prices,
            args=(on_price, lambda: self.results.put(("offline",)), self.asset["product"]),
            daemon=True,
        ).start()

    def _poll_history(self):
        if self.range_key in LIVE_RANGES:
            return  # nothing to fetch: this range is drawn from the feed we already have
        if self._history_busy:
            return
        self._history_busy = True
        key = self.range_key

        def worker():
            try:
                self.results.put(("history", key, *fetch_history(key, self.asset["product"])))
            except Exception:
                self.results.put(("history_failed",))

        threading.Thread(target=worker, daemon=True).start()

    def _seed_trader(self):
        def worker():
            try:
                rows = _get_json("/candles?granularity=60", self.asset["product"])
                rows.sort(key=lambda r: r[0])
                # each candle's close happens at its start time + 60 s
                self.results.put(("seed", [(r[0] + 60, r[4]) for r in rows[-50:]]))
            except Exception:
                pass  # the bot just starts from a typical volatility instead

        threading.Thread(target=worker, daemon=True).start()

    def _start_kalshi(self):
        threading.Thread(
            target=stream_kalshi,
            args=(lambda m: self.results.put(("market", m)),
                  lambda t, r, v, c: self.results.put(("settled", t, r, v, c)),
                  lambda: self.trader.pending_tickers(self.coin), self.now, self.asset["series"]),
            daemon=True,
        ).start()

    def _seed_offset(self):
        """Learn the current index gap from rounds that settled before we launched."""
        def worker():
            try:
                found = fetch_recent_offsets(self.asset["series"], self.asset["product"])
                if found:
                    self.results.put(("offsets", found))
            except Exception:
                pass  # we'll learn it from the next settlements instead

        threading.Thread(target=worker, daemon=True).start()

    def _redraw_for_offset(self):
        """The index gap changed, so everything shown on the index's scale moves with it."""
        self._draw_price()
        self._draw_ptb()
        self._chart_dirty = True

    def _tick(self):
        """Once a second: keep the countdown moving."""
        self._draw_ptb()
        self.after(1000, self._tick)

    def _pump(self):
        latest_price = None
        try:
            while True:
                msg = self.results.get_nowait()
                if msg[0] == "price":
                    latest_price = msg  # trades can arrive in bursts; draw only the newest
                else:
                    self._handle(msg)
        except queue.Empty:
            pass
        if latest_price:
            self._handle(latest_price)
        # The chart is the expensive part; redraw it at most ~10x a second so it never
        # holds up the price.
        if self._chart_dirty and time.monotonic() - self._chart_drawn_at > 0.1:
            self._chart_dirty = False
            self._chart_drawn_at = time.monotonic()
            self._draw_chart()
        open_panels = self.hub.open_panels()
        if open_panels and time.monotonic() - self._panel_drawn_at > 0.25:
            self._panel_drawn_at = time.monotonic()
            for panel in open_panels:
                panel.refresh()
        self.after(8, self._pump)

    def flash_level(self, name):
        """0..1: how strongly a strategy's row should glow. It eases in over half a second,
        then fades out slowly, so a new bet is noticeable without being jarring."""
        started = self.flash.get(name)
        if started is None:
            return 0.0
        age = time.monotonic() - started
        if age >= FLASH_SECONDS:
            return 0.0
        return min(1.0, age / 0.5) * (1 - age / FLASH_SECONDS)

    def _on_bankrupt(self, e):
        """A strategy ran out and has been staked again. Unlike a bet, this is reported
        whichever strategy is being tracked -- it is about the experiment, not about one
        position -- though the bell still silences the toast. The postmortem is on disk
        either way, which is what makes it findable after the fact.
        """
        title = f"{e['strategy']} ran out of money"
        body = (f"Ended with ${e['cash']:.2f} after {e['rounds']} rounds. "
                f"Staked again at ${START_BALANCE:,.0f} (life {e['life'] + 1}). "
                f"Findings: {os.path.basename(e['report'])}")
        print(f"[bankrupt] {title} - {body}", flush=True)  # also on stdout, for a log
        if self.muted:
            return

        def worker():
            try:
                send_toast(self.app_id, title, body)
            except Exception:
                pass  # a missed toast shouldn't disturb the display

        threading.Thread(target=worker, daemon=True).start()

    def _on_trade(self, e):
        """A bet was placed or settled: refresh the panel and (unless muted) notify."""
        if e["kind"] == "bet":
            self.flash[e["strategy"]] = time.monotonic()
        for panel in self.hub.open_panels():
            panel.refresh()
        self._chart_dirty = True  # a new marker may belong on the chart
        if e["kind"] == "bankrupt":
            self._on_bankrupt(e)
            return
        # Five strategies trade at once; only the one you're tracking gets notifications,
        # and the bell silences those too.
        # Both worlds have a tracked strategy, and both are worth hearing about; the bell
        # still silences them.
        tracked = (self.trader.tracked(False), self.trader.tracked(True))
        if self.muted or e["strategy"] not in tracked:
            return
        who = f"{self.coin} {e['strategy']}"
        if e["kind"] == "bet":
            title = f"{who}: bet {e['side']}, {e['contracts']} × {e['price'] * 100:.0f}¢ ({e['multiplier']:.2f}x)"
            body = (f"{e['model_prob'] * 100:.0f}% confident  ·  ${e['cost']:.2f} incl. fee  ·  "
                    f"price to beat ${e['strike']:,.{self.asset['decimals']}f}")
        elif e["kind"] == "sold":
            title = f"{who}: sold early {'+' if e['pnl'] >= 0 else '−'}${abs(e['pnl']):.2f}"
            body = f"{e['side']} × {e['contracts']} at {e['exit_price'] * 100:.0f}¢"
        else:
            title = f"{who}: {'won +' if e['won'] else 'lost −'}${abs(e['pnl']):.2f}"
            final = parse_amount(e.get("final_value")) or 0.0
            body = (f"{e['side']} bet {'paid out' if e['won'] else 'missed'}  ·  "
                    f"final ${final:,.{self.asset['decimals']}f}")

        def worker():
            try:
                send_toast(self.app_id, title, body)
            except Exception:
                pass

        threading.Thread(target=worker, daemon=True).start()

    def _handle(self, msg):
        kind = msg[0]
        if kind == "price":
            changed = msg[1] != self.price
            self.price = msg[1]
            self.last_trade_ts = msg[2] or time.time()
            if not self.online:
                self._set_live(True)
            self._maybe_alert(self.price)
            if changed:  # only redraw when the price actually moved
                if self.history:
                    self.history[-1] = self.price  # keep the chart's end point live
                    if self.high is not None:
                        self.high, self.low = max(self.high, self.price), min(self.low, self.price)
                self._draw_price()
                self._draw_ptb()
                self._chart_dirty = True
            self._draw_status()
            ts = msg[2] or self.now()
            self.trader.observe(self.coin, self.price, ts)  # feeds volatility
            sec = int(ts)  # one point per second for the 15-minute window chart
            if self.window_pts and self.window_pts[-1][0] == sec:
                self.window_pts[-1] = (sec, self.price)
            elif not self.window_pts or sec > self.window_pts[-1][0]:
                self.window_pts.append((sec, self.price))
            if self.range_key in ("15M", "1M"):  # both are drawn from the live points
                self._chart_dirty = True
        elif kind == "seed":
            candles = msg[1]  # [(time, close), ...] one per minute, oldest first
            done = [p for t, p in candles if t <= self.now()]  # drop the minute still in progress
            self.trader.seed_vol(self.coin, done)
            first = self.window_pts[0][0] if self.window_pts else float("inf")
            self.window_pts.extendleft(reversed([pt for pt in candles if pt[0] < first]))
            self._chart_dirty = True
        elif kind == "market":
            m = msg[1]
            changed = (m["strike"], m["close"]) != (self.ptb, self.ptb_close)
            self.ptb, self.ptb_close = m["strike"], m["close"]
            if changed:
                self._draw_ptb()
                if self.range_key == "15M":
                    self._draw_chart()
            if self.price:
                for event in self.trader.step(self.coin, m, self.price, self.now()):
                    self._on_trade(event)
            self.trader.save()  # throttled: keeps the balance file current
        elif kind == "settled":
            _, ticker, result, final, close = msg
            # The settled value is the index averaged over that round's final minute: a free,
            # exact measurement of how far it sits above our exchange price.
            self.trader.note_settlement(self.coin, close, final)
            for event in self.trader.on_settled(ticker, result, final, self.now(), self.price):
                self._on_trade(event)
            self._redraw_for_offset()  # a fresh measurement shifts every price we show
        elif kind == "offsets":
            self.trader.seed_offsets(self.coin, msg[1])
            self._redraw_for_offset()
        elif kind == "offline":
            if self.online:
                self._set_live(False)
        elif kind == "history":
            self._history_busy = False
            _, key, closes, high, low = msg
            if key != self.range_key:  # user switched range while this was loading
                self._poll_history()
                return
            self.history, self.high, self.low = closes, high, low
            if self.price is not None:
                self.history[-1] = self.price
            self._draw_price()
            self._draw_chart()
            if self._history_timer:  # one refresh timer only, even after switching ranges
                self.after_cancel(self._history_timer)
            self._history_timer = self.after(CANDLES_EVERY_MS, self._poll_history)
        elif kind == "history_failed":
            self._history_busy = False
            self._draw_chart()
            self.after(10_000, self._poll_history)


class SidePanel(Drawing, tk.Toplevel):
    """A square window that slides out from a side button on the phone. It has three tabs:
    the tracked strategy's account, a log of its bets, and a leaderboard.

    There are two, one per world. The right-hand panel shows the six strategies; the left
    shows their anti-world twins, which believe the opposite of whatever the model says.
    Same class, same tabs -- `anti` picks which set of accounts it reads and which edge it
    grows from."""

    STEPS = 9
    ROW_H = 24  # a row in the bet log
    LOG_ROWS = 8

    def __init__(self, hub, anti=False):
        super().__init__(hub.root)  # its own window, parented to the invisible root
        self.hub = hub
        self.anti = anti
        self.k = hub.monitor().k
        self.is_open = False
        self.tab = "account"
        self.log_offset = 0  # how many rows the log is scrolled back
        self._row_names = []  # strategy shown in each leaderboard row, for taps
        self._anim = 0  # bumped on every open/close so a stale animation stops itself
        self._reset_armed = False
        self._reset_timer = None

        self.withdraw()
        self.overrideredirect(True)
        self.configure(bg=TRANSPARENT)
        self.attributes("-transparentcolor", TRANSPARENT)
        s = self.px(SIDE)
        self.canvas = tk.Canvas(self, width=s, height=s, bg=TRANSPARENT, highlightthickness=0)
        # Anchor toward the phone so the panel appears to slide out from it.
        self.canvas.pack(anchor="nw")
        self.canvas.bind("<MouseWheel>", lambda e: self._scroll(-1 if e.delta > 0 else 1))
        self.rrect(0, 0, SIDE, SIDE, 46, fill=BEZEL)
        self.rrect(6, 6, SIDE - 6, SIDE - 6, 40, fill=BG)
        for tab in ("account", "log", "strategies"):
            self._button(f"tab_{tab}", lambda t=tab: self._set_tab(t))
        for i in range(len(strategies(anti))):
            self._button(f"strat_{i}", lambda i=i: self._pick(i))
        self._button("badge", self._cycle)
        self._button("pause", self._toggle_pause)
        self._button("reset", self._reset)
        self._button("log_up", lambda: self._scroll(-1))
        self._button("log_down", lambda: self._scroll(1))

    @property
    def app(self):
        """The page currently on screen. The panel always shows that coin."""
        return self.hub.monitor()

    # ---- placement and sliding --------------------------------------------

    def _origin(self, width=None):
        """Top-left of the panel when it is `width` wide, glued to the phone's edge.

        The anti panel grows leftward, so its left edge moves as it opens while its right
        edge stays pinned to the phone -- otherwise it would slide out from under itself.
        """
        a = self.app  # position against the page that's on screen
        full = self.px(SIDE)
        width = full if width is None else width
        y = a.winfo_y() + a.px(110)
        if self.anti:
            right = a.winfo_x() - a.px(6)
            return max(0, right - width), y
        x = a.winfo_x() + a.px(W + 2 * TAB) + a.px(6)
        return min(x, self.winfo_screenwidth() - full), y

    def follow(self):
        if self.is_open:
            x, y = self._origin()
            self.geometry(f"+{x}+{y}")

    def toggle(self):
        self.is_open = not self.is_open
        self._anim += 1
        if self.is_open:
            self.refresh()
            self.deiconify()
            self.lift()
            self._slide(self._anim, 0, 1)
        else:
            self._slide(self._anim, self.STEPS, -1)

    def _slide(self, token, step, direction):
        if token != self._anim:
            return
        t = step / self.STEPS
        width = max(1, int(self.px(SIDE) * (1 - (1 - t) ** 3)))  # ease-out
        x, y = self._origin(width)
        self.geometry(f"{width}x{self.px(SIDE)}+{x}+{y}")
        nxt = step + direction
        if 0 <= nxt <= self.STEPS:
            self.after(14, self._slide, token, nxt, direction)
        elif direction < 0:
            self.withdraw()

    # ---- content ----------------------------------------------------------

    def refresh(self):
        self.canvas.delete("dyn")
        s = self.hub.trader.snapshot(name=self.hub.trader.tracked(self.anti),
                                     coin=self.app.coin)
        self._header(s)
        {"account": self._account, "log": self._log, "strategies": self._strategies}[
            self.tab](s)

    def _header(self, s):
        tag = "dyn"
        title = (f"Anti-world {self.app.coin}" if self.anti
                 else f"Kalshi {self.app.coin} 15-min")
        self.text(28, 32, title, 14, DOWN if self.anti else TEXT, anchor="w", tags=tag)
        chip = f"{s['name']}  ▾"
        w = tkfont.Font(family="Segoe UI Semibold", size=-self.px(11)).measure(chip) / self.k + 24
        self.rrect(SIDE - 28 - w, 20, SIDE - 28, 44, 12, fill=BUTTON, tags=(tag, "btn", "badge"))
        self.text(SIDE - 28 - w / 2, 32, chip, 11, TEXT, tags=(tag, "btn", "badge"))

        labels = {"account": "Account", "log": "Log", "strategies": "Strategies"}
        x = 28
        for key, label in labels.items():
            active = key == self.tab
            tags = (tag, "btn", f"tab_{key}")
            self.rrect(x, 54, x + 96, 80, 13, fill=TEXT if active else BUTTON, tags=tags)
            self.text(x + 48, 67, label, 11, BG if active else TEXT, tags=tags)
            if key == "strategies" and not active:  # a bet was placed while you're elsewhere
                glow = max((self.app.flash_level(n) for n in self.app.flash), default=0.0)
                if glow > 0:
                    # brighter than the row glow: it's a small dot on a dark button
                    self.circle(x + 84, 67, 4, fill=mix_color(BUTTON, AMBER, glow * 1.4 + 0.15),
                                width=0, tags=tags)
            x += 104

    def _account(self, s):
        c, tag = self.canvas, "dyn"
        self.text(28, 122, f"${s['equity']:,.2f}", 30, TEXT, anchor="w", tags=tag)
        good = s["pnl"] >= 0
        label = f"{'+' if good else '−'}${abs(s['pnl']):,.2f}  ·  {s['pnl_pct']:+.2f}%"
        width = tkfont.Font(family="Segoe UI Semibold", size=-self.px(12)).measure(label)
        right = 28 + width / self.k + 24
        self.rrect(28, 144, right, 168, 12, fill=UP_TINT if good else DOWN_TINT, tags=tag)
        self.text((28 + right) / 2, 156, label, 12, UP if good else DOWN, tags=tag)

        if s["paused"]:
            status, color = "Paused", MUTED
        elif s["open"]:
            status, color = f"{len(s['open'])} open", UP
        else:
            status, color = "Watching", TEXT
        self.text(SIDE - 28, 156, status, 11, color, anchor="e", tags=tag)

        # Kalshi's real prices, what this strategy thinks, and its open bets
        self.rrect(20, 178, SIDE - 20, 278, 20, fill=CARD, tags=tag)
        m, view = s["market"], s["view"]
        if m and m["yes_ask"] > 0 and m["no_ask"] > 0:
            odds = (f"UP {m['yes_ask'] * 100:.0f}¢ ({1 / m['yes_ask']:.2f}x)  ·  "
                    f"DOWN {m['no_ask'] * 100:.0f}¢ ({1 / m['no_ask']:.2f}x)")
        else:
            odds = "Loading…"
        if view:
            chance = f"model {view['p_model'] * 100:.0f}%  ·  market {view['mid'] * 100:.0f}%"
            sig = view["signal"]
            if sig["bet"]:  # it's about to place a bet: show how sure it is
                signal = f"{sig['side']} · {sig['conf']:.0f}% confident · will bet"
                signal_color = UP if sig["side"] == "UP" else DOWN
            else:
                signal, signal_color = f"{sig['side']} {sig['conf']:.0f}%  ·  {sig['why']}", MUTED
        else:
            chance, signal, signal_color = "—", "—", MUTED
        open_lots = s["open"]
        if len(open_lots) == 1:
            p = open_lots[0]
            bet = f"{p['side']} × {p['contracts']} @ {p['price'] * 100:.0f}¢ ({p['multiplier']:.2f}x)"
            bet_color = UP if p["side"] == "UP" else DOWN
        elif open_lots:
            up = sum(p["contracts"] for p in open_lots if p["side"] == "UP")
            down = sum(p["contracts"] for p in open_lots if p["side"] == "DOWN")
            bet, bet_color = f"{len(open_lots)} bets  ·  {up} UP, {down} DOWN", TEXT
        else:
            bet, bet_color = "None", MUTED
        rows = [("Odds", odds, TEXT), ("UP chance", chance, TEXT),
                ("Signal", signal, signal_color), ("Bets", bet, bet_color)]
        for i, (name, value, col) in enumerate(rows):
            y = 191 + i * 24
            self.text(36, y, name, 12, MUTED, weight="", anchor="w", tags=tag)
            self.text(SIDE - 36, y, value, 11, col, anchor="e", tags=tag)
            if i < 3:
                c.create_line(*self.pts([36, y + 12, SIDE - 36, y + 12]), fill=GRID,
                              width=self.px(1), tags=tag)

        self.text(SIDE / 2, 291, f"Rounds monitored {s['rounds_monitored']}  ·  "
                  f"participated {s['rounds_joined']}", 11, TEXT, weight="", tags=tag)
        self.text(SIDE / 2, 306, f"{s['bets']} bets  ·  {s['wins']} wins  ·  {s['losses']} "
                  f"losses  ·  cash ${s['cash']:,.2f}", 10, MUTED, weight="", tags=tag)

        for name, x1, x2, fill, txt, fg in (
            ("pause", 28, 176, BUTTON, "Resume all" if s["paused"] else "Pause all", TEXT),
            ("reset", 184, SIDE - 28, DOWN_TINT if self._reset_armed else BUTTON,
             "Tap again to reset" if self._reset_armed else f"Reset all ${START_BALANCE:,.0f}",
             DOWN if self._reset_armed else TEXT),
        ):
            self.rrect(x1, 318, x2, 346, 14, fill=fill, tags=(tag, "btn", name))
            self.text((x1 + x2) / 2, 332, txt, 12, fg, tags=(tag, "btn", name))

    def _log(self, s):
        c, tag = self.canvas, "dyn"
        lots = list(reversed(s["log"]))  # newest first
        top = 96
        for x, label, size, anchor in ((34, "TIME", 9, "w"), (74, "COIN", 9, "w"),
                                       (108, "BET", 9, "w"), (142, "CONTRACTS", 8, "w"),
                                       (190, "MULT", 8, "w"), (258, "COST", 9, "e"),
                                       (SIDE - 34, "RESULT", 9, "e")):
            self.text(x, top, label, size, MUTED, anchor=anchor, tags=tag)
        if not lots:
            self.text(SIDE / 2, 200, "No bets yet", 14, MUTED, weight="", tags=tag)
            self.text(SIDE / 2, 224, "They'll show up here as they're placed", 11, MUTED,
                      weight="", tags=tag)
            return
        self.log_offset = max(0, min(self.log_offset, len(lots) - self.LOG_ROWS))
        page = lots[self.log_offset:self.log_offset + self.LOG_ROWS]
        # Anything still running is worth flagging, whichever coin it is on -- every coin's
        # round closes on the same quarter hour, so this catches the whole live slate.
        now = self.app.now()
        for i, lot in enumerate(page):
            y = top + 22 + i * self.ROW_H
            if lot["close"] > now:  # this round has not settled yet
                self.rrect(24, y - 11, SIDE - 24, y + 11, 8, fill=HILITE, tags=tag)
            c.create_line(*self.pts([28, y - 12, SIDE - 28, y - 12]), fill=GRID,
                          width=self.px(1), tags=tag)
            when = datetime.fromisoformat(lot["time"]).strftime("%I:%M%p").lstrip("0")
            self.text(34, y, when[:-1].lower(), 9, MUTED, weight="", anchor="w", tags=tag)
            self.text(74, y, lot.get("coin") or "?", 10, TEXT, anchor="w", tags=tag)
            col = UP if lot["side"] == "UP" else DOWN
            self.text(108, y, lot["side"], 10, col, anchor="w", tags=tag)
            self.text(142, y, f"{lot['contracts']}×{lot['price'] * 100:.0f}¢", 10,
                      TEXT, weight="", anchor="w", tags=tag)
            # a cheap contract pays 70x or more, so drop the decimals once it is big
            mult = lot["multiplier"]
            self.text(190, y, f"{mult:.0f}x" if mult >= 10 else f"{mult:.2f}x", 9, MUTED,
                      weight="", anchor="w", tags=tag)
            self.text(258, y, f"${lot['cost']:.2f}", 10, TEXT, weight="", anchor="e", tags=tag)
            status = lot["status"]
            if status == "open":
                res, rcol = "open", MUTED
            else:
                pnl = lot["pnl"]
                sign = "+" if pnl > 0 else "−"
                res = f"{'sold ' if status == 'sold' else ''}{sign}${abs(pnl):.2f}"
                rcol = UP if pnl > 0 else DOWN
            self.text(SIDE - 34, y, res, 10, rcol, anchor="e", tags=tag)
        last = min(len(lots), self.log_offset + self.LOG_ROWS)
        net = sum(lot.get("pnl", 0) for lot in lots)
        self.text(34, 338, f"{self.log_offset + 1}–{last} of {len(lots)}  ·  net "
                  f"{'+' if net >= 0 else '−'}${abs(net):.2f}", 10, MUTED, weight="",
                  anchor="w", tags=tag)
        self.rrect(176, 333, 188, 343, 3, fill=HILITE, tags=tag)  # legend for the band
        self.text(194, 338, "live round", 10, MUTED, weight="", anchor="w", tags=tag)
        for name, cx, glyph in (("log_up", SIDE - 62, "▲"), ("log_down", SIDE - 34, "▼")):
            self.circle(cx, 338, 11, fill=BUTTON, width=0, tags=(tag, "btn", name))
            self.text(cx, 338, glyph, 8, TEXT, weight="", tags=(tag, "btn", name))

    def _strategies(self, s):
        tag = "dyn"
        standings = self.app.trader.standings(anti=self.anti)
        self._row_names = [r["name"] for r in standings]
        any_bets = any(r["bets"] for r in standings)
        pitch = min(47, 234 // max(1, len(standings)))  # rows shrink to fit however many there are
        height = pitch - 4
        for i, r in enumerate(standings):
            y = 96 + i * pitch
            chosen = r["name"] == self.app.trader.tracked(self.anti)
            tags = (tag, "btn", f"strat_{i}")
            glow = self.app.flash_level(r["name"])  # a soft amber warmth after a new bet
            base = BUTTON if chosen else CARD
            row = mix_color(base, AMBER, 0.28 * glow)
            top_line, bottom_line = y + height * 0.32, y + height * 0.74
            self.rrect(20, y, SIDE - 20, y + height, 14, fill=row, tags=tags)
            if glow > 0:
                self.circle(27, top_line, 3.5, fill=mix_color(row, AMBER, glow), width=0, tags=tags)
            star = "★ " if (i == 0 and any_bets and r["pnl_pct"] > 0) else ""
            self.text(34, top_line, f"{star}{r['name']}", 12, TEXT, anchor="w", tags=tags)
            self.text(SIDE - 100, top_line, f"${r['equity']:,.2f}", 12, TEXT, anchor="e", tags=tags)
            good = r["pnl_pct"] >= 0
            self.text(SIDE - 34, top_line, f"{'+' if good else '−'}{abs(r['pnl_pct']):.1f}%", 12,
                      UP if good else DOWN, anchor="e", tags=tags)
            record = f"{r['bets']} bets · {r['joined']}/{s['rounds_monitored']} rounds"
            self.text(34, bottom_line, r["blurb"], 10, MUTED, weight="", anchor="w", tags=tags)
            self.text(SIDE - 34, bottom_line, record, 10, MUTED, weight="", anchor="e", tags=tags)
        self.text(SIDE / 2, 338, f"Rounds monitored {s['rounds_monitored']}  ·  tap a "
                  "strategy to track it", 10, MUTED, weight="", tags=tag)

    # ---- actions ----------------------------------------------------------

    def _set_tab(self, tab):
        self.tab = tab
        self.refresh()

    def _scroll(self, rows):
        if self.tab == "log":
            self.log_offset = max(0, self.log_offset + rows * 3)
            self.refresh()

    def _pick(self, i):
        if i < len(self._row_names):
            self.app.trader.select(self._row_names[i])
            self.app._chart_dirty = True
            self.log_offset = 0
            self.refresh()

    def _cycle(self):
        names = [p["name"] for p in strategies(self.anti)]
        trader = self.app.trader
        here = trader.tracked(self.anti)
        trader.select(names[(names.index(here) + 1) % len(names)])
        self.app._chart_dirty = True
        self.log_offset = 0
        self.refresh()

    def _toggle_pause(self):
        trader = self.app.trader
        trader.set_paused(not trader.paused)
        self.refresh()

    def _reset(self):
        if not self._reset_armed:  # resetting deletes the history, so ask twice
            self._reset_armed = True
            self._reset_timer = self.after(3000, self._disarm)
        else:
            self._disarm()
            self.app.trader.reset()
            self.app._chart_dirty = True
        self.refresh()

    def _disarm(self):
        self._reset_armed = False
        if self._reset_timer:
            self.after_cancel(self._reset_timer)
            self._reset_timer = None
        self.refresh()


def main():
    if sys.platform == "win32":
        try:  # keep text crisp on high-DPI screens
            ctypes.windll.shcore.SetProcessDpiAwareness(1)
        except Exception:
            pass
    root = tk.Tk()
    root.withdraw()  # an invisible parent; the pages and panels are its Toplevel windows
    hub = Hub(root, active="BTC")
    for coin, asset in ASSETS.items():
        hub.monitors[coin] = Monitor(root, asset, hub)
    hub.redraw_tabs()
    root.mainloop()


if __name__ == "__main__":
    main()
