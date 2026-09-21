"""Which glyphs can the app actually draw?

    python font_probe.py            # the symbols in use
    python font_probe.py ◎ ≡ S      # candidates of your own

A codepoint the font lacks renders as a hollow box, which looks broken rather than minimal.
Tk will not say which glyphs it has, but it can be measured: a missing one falls back to a box
of a characteristic width, so comparing each candidate against a private-use codepoint --
guaranteed absent from every font -- separates the real from the boxes.

Run this before adding a coin. U+25CE BULLSEYE was the obvious pick for Solana and is a box
on Windows 11; it would have shipped looking broken.
"""

import sys
import tkinter as tk
import tkinter.font as tkfont

# The symbols the app uses today, verified on Windows 11 / Segoe UI.
IN_USE = {
    "BTC": "₿",   # ₿  bitcoin sign
    "ETH": "Ξ",   # Ξ  greek capital xi, as Ethereum is usually written
    "SOL": "≡",   # ≡  identical to: three bars, the nearest thing to Solana's mark
    "XRP": "✕",   # ✕  multiplication x
    "DOGE": "Ð",  # Ð  latin capital eth, the conventional Dogecoin sign
}

FAMILIES = ("Segoe UI Semibold", "Segoe UI")
PRIVATE_USE = ""  # no font defines this, so its width IS the box width


def main():
    root = tk.Tk()
    root.withdraw()
    try:
        wanted = sys.argv[1:]
        pairs = [(f"arg{i}", ch) for i, ch in enumerate(wanted)] if wanted \
            else sorted(IN_USE.items())
        bad = []
        for family in FAMILIES:
            font = tkfont.Font(family=family, size=-14)
            box = font.measure(PRIVATE_USE)
            print(f"\n{family}  (box width {box})")
            for label, ch in pairs:
                width = font.measure(ch)
                boxed = width == box
                print(f"  {label:6s} U+{ord(ch):04X}  width {width:3d}  "
                      f"{'BOX -- do not use' if boxed else 'renders'}")
                if boxed:
                    bad.append((family, label, ch))
        print()
        if bad:
            for family, label, ch in bad:
                print(f"UNUSABLE: {label} U+{ord(ch):04X} is a box in {family}")
            sys.exit(1)
        print("All of these render.")
    finally:
        root.destroy()


if __name__ == "__main__":
    main()
