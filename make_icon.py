"""Build the app icons: a bell with a coin symbol in it (needs Pillow).
Writes btc.ico (Bitcoin) and eth.ico (Ethereum), plus a preview PNG of each."""

from PIL import Image, ImageDraw, ImageFont

from asset_cracker import BELL, BELL_BODY, BELL_CENTER_Y, CLAPPER, ICON_BG as INK

SIZE = 1024  # draw big, then let Pillow downsample for each icon size
SIZES = [(16, 16), (24, 24), (32, 32), (48, 48), (64, 64), (128, 128), (256, 256)]


def draw_bitcoin(d, gx, gy):
    # No installed font has the glyph, so: a bold "B" plus the two vertical strokes.
    font = ImageFont.truetype("C:/Windows/Fonts/segoeuib.ttf", 330)
    left, top, right, bottom = font.getbbox("B")
    bx, by = gx - (left + right) / 2, gy - (top + bottom) / 2
    d.text((bx, by), "B", font=font, fill="white")
    stroke, reach = (right - left) * 0.09, (bottom - top) * 0.13
    for frac in (0.36, 0.60):
        x = bx + left + (right - left) * frac
        d.rectangle((x - stroke / 2, by + top - reach, x + stroke / 2, by + bottom + reach),
                    fill="white")


def draw_ethereum(d, gx, gy):
    # The Ethereum diamond: a kite on top and a chevron below it.
    s = 165  # half the height, in pixels

    def p(x, y):
        return (gx + x * s * 0.62, gy + y * s)

    d.polygon([p(0, -1.0), p(1, 0.10), p(0, 0.42), p(-1, 0.10)], fill="white")
    d.polygon([p(-1, 0.30), p(0, 0.62), p(1, 0.30), p(0, 1.0)], fill="white")


def build(name, glyph):
    img = Image.new("RGBA", (SIZE, SIZE), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.rounded_rectangle((0, 0, SIZE - 1, SIZE - 1), radius=SIZE // 5, fill=INK)

    # Bell spans y -61..-33 (clapper bottom) in wordmark units; fill ~66% of the icon.
    scale = SIZE * 0.66 / 28

    def pt(x, y):
        return (SIZE / 2 + x * scale, SIZE / 2 + (y - BELL_CENTER_Y) * scale)

    d.polygon([pt(x, y) for x, y in BELL_BODY], fill=BELL)
    cx, cy, r = CLAPPER
    px, py = pt(cx, cy)
    d.ellipse((px - r * scale, py - r * scale, px + r * scale, py + r * scale), fill=BELL)

    glyph(d, *pt(0, -50))  # the coin symbol, centered in the bell's body
    img.save(f"{name}.ico", sizes=SIZES)
    img.resize((256, 256), Image.LANCZOS).save(f"{name}_preview.png")


def main():
    build("btc", draw_bitcoin)
    build("eth", draw_ethereum)


if __name__ == "__main__":
    main()
