#!/usr/bin/env python3
"""Generate Android launcher icons from a compass 🧭 + chart 📈 combo.

The Noto Color Emoji font on Linux is a fixed-bitmap CBDT font (109px).
We render the emojis at native size and upscale to the canvas — that's
fine for an app icon at 432px+.
"""
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

EMOJI_FONT = "/usr/share/fonts/truetype/noto/NotoColorEmoji.ttf"
BRAND = (61, 155, 255, 255)          # #3d9bff
ROOT = Path(__file__).resolve().parents[1] / "app/src/main/res"

def render_emoji(char: str) -> Image.Image:
    """Render a single emoji glyph as RGBA at its native size."""
    font = ImageFont.truetype(EMOJI_FONT, size=109)
    img = Image.new("RGBA", (140, 140), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    d.text((70, 70), char, font=font, anchor="mm", embedded_color=True)
    return img.crop(img.getbbox())

def make_foreground(canvas: int) -> Image.Image:
    """Adaptive icon foreground (transparent bg, compass + chart).
    Safe-zone diameter ~= 66% of canvas (Android adaptive-icon spec).
    """
    img = Image.new("RGBA", (canvas, canvas), (0, 0, 0, 0))
    compass = render_emoji("\U0001F9ED")   # 🧭
    chart = render_emoji("\U0001F4C8")      # 📈

    # Sizes: compass as the dominant element; chart tucked bottom-left as accent.
    safe = int(canvas * 0.66)
    compass_w = int(safe * 0.95)
    compass_h = int(compass.height * (compass_w / compass.width))
    compass_r = compass.resize((compass_w, compass_h), Image.LANCZOS)

    chart_w = int(safe * 0.45)
    chart_h = int(chart.height * (chart_w / chart.width))
    chart_r = chart.resize((chart_w, chart_h), Image.LANCZOS)

    # Center the compass a touch above center; chart overlaps lower-left.
    cx, cy = canvas // 2, canvas // 2
    img.paste(compass_r, (cx - compass_w // 2, cy - compass_h // 2 - int(canvas * 0.04)), compass_r)
    img.paste(chart_r, (cx - compass_w // 2 - int(chart_w * 0.25), cy + int(canvas * 0.10)), chart_r)

    return img

def make_legacy(canvas: int) -> Image.Image:
    """Legacy square icon (pre-Android 8) — solid bg + foreground centered."""
    bg = Image.new("RGBA", (canvas, canvas), BRAND)
    fg = make_foreground(canvas)
    bg.alpha_composite(fg)
    return bg

def make_round(canvas: int) -> Image.Image:
    """Legacy round icon — circular mask over solid bg + foreground."""
    base = Image.new("RGBA", (canvas, canvas), (0, 0, 0, 0))
    bg = Image.new("RGBA", (canvas, canvas), BRAND)
    mask = Image.new("L", (canvas, canvas), 0)
    ImageDraw.Draw(mask).ellipse((0, 0, canvas, canvas), fill=255)
    base.paste(bg, (0, 0), mask)
    fg = make_foreground(canvas)
    base.alpha_composite(fg)
    return base

# Density buckets
LEGACY = {"mdpi": 48, "hdpi": 72, "xhdpi": 96, "xxhdpi": 144, "xxxhdpi": 192}
FG     = {"mdpi": 108, "hdpi": 162, "xhdpi": 216, "xxhdpi": 324, "xxxhdpi": 432}

def main() -> None:
    for density, size in LEGACY.items():
        out = ROOT / f"mipmap-{density}"
        out.mkdir(parents=True, exist_ok=True)
        make_legacy(size).save(out / "ic_launcher.png")
        make_round(size).save(out / "ic_launcher_round.png")
    for density, size in FG.items():
        out = ROOT / f"mipmap-{density}"
        out.mkdir(parents=True, exist_ok=True)
        make_foreground(size).save(out / "ic_launcher_foreground.png")
    # Play Store / docs preview (not packaged, but useful)
    make_legacy(512).save(ROOT.parent.parent.parent / "ic_launcher_preview.png")
    print("Icons generated.")

if __name__ == "__main__":
    main()
