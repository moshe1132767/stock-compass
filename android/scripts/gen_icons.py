#!/usr/bin/env python3
"""Generate the "trade" Android launcher icon — a bold lowercase 't' monogram
in emerald on a dark ink background. Pure geometric primitives (rounded
stadiums + round-capped foot), supersampled x4 and LANCZOS-downscaled.

Design per icon research: premium trading brands use an abstract mark / monogram,
NOT literal candlesticks. Trade Green #00D37F on ink #0A0F1A.
"""
from pathlib import Path
from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parents[1] / "app/src/main/res"

INK1  = (14, 27, 46)    # #0E1B2E  gradient top-left
INK2  = (10, 15, 26)    # #0A0F1A  gradient bottom-right
GREEN = (0, 211, 127)   # #00D37F  Trade Green
SS = 4                  # supersample factor

def lerp(a, b, t):
    return tuple(int(a[i] + (b[i] - a[i]) * t) for i in range(3))

def gradient(cnv):
    """Diagonal ink gradient, built small then upscaled (smooth, cheap)."""
    g = Image.new("RGB", (256, 256))
    px = g.load()
    for y in range(256):
        for x in range(256):
            px[x, y] = lerp(INK1, INK2, (x + y) / 510.0)
    return g.resize((cnv, cnv), Image.LANCZOS).convert("RGBA")

def draw_mark(cnv, scale, color):
    """Return an RGBA (transparent) canvas with the emerald 't', scaled about center."""
    S = cnv
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    def X(f): return (0.5 + (f - 0.5) * scale) * S
    def Y(f): return (0.5 + (f - 0.5) * scale) * S
    def TH(f): return f * scale * S

    stem_w = 0.130
    d.rounded_rectangle([X(0.5 - stem_w/2), Y(0.24), X(0.5 + stem_w/2), Y(0.76)],
                        radius=TH(stem_w/2), fill=color)
    cb_th, cy = 0.118, 0.385
    d.rounded_rectangle([X(0.315), Y(cy - cb_th/2), X(0.685), Y(cy + cb_th/2)],
                        radius=TH(cb_th/2), fill=color)
    # foot: round-capped flick emerging from stem bottom, curving up-right
    foot_w = 0.130
    pts = [(0.50, 0.725), (0.655, 0.720), (0.705, 0.652)]
    w = max(1, int(round(TH(foot_w))))
    for i in range(len(pts) - 1):
        d.line([X(pts[i][0]), Y(pts[i][1]), X(pts[i+1][0]), Y(pts[i+1][1])], fill=color, width=w)
    rr = TH(foot_w/2)
    for p in pts:
        d.ellipse([X(p[0])-rr, Y(p[1])-rr, X(p[0])+rr, Y(p[1])+rr], fill=color)
    return img

def foreground(dp):
    """Adaptive foreground: mark on transparent, sized for the 66dp safe zone."""
    cnv = dp * SS
    return draw_mark(cnv, 1.15, GREEN).resize((dp, dp), Image.LANCZOS)

def legacy(px, round_mask=False):
    cnv = px * SS
    bg = gradient(cnv)
    if round_mask:
        base = Image.new("RGBA", (cnv, cnv), (0, 0, 0, 0))
        mask = Image.new("L", (cnv, cnv), 0)
        ImageDraw.Draw(mask).ellipse([0, 0, cnv, cnv], fill=255)
        base.paste(bg, (0, 0), mask)
        bg = base
    bg.alpha_composite(draw_mark(cnv, 1.34, GREEN))
    return bg.resize((px, px), Image.LANCZOS)

LEGACY = {"mdpi": 48, "hdpi": 72, "xhdpi": 96, "xxhdpi": 144, "xxxhdpi": 192}
FG     = {"mdpi": 108, "hdpi": 162, "xhdpi": 216, "xxhdpi": 324, "xxxhdpi": 432}

def main():
    for d, s in LEGACY.items():
        out = ROOT / f"mipmap-{d}"; out.mkdir(parents=True, exist_ok=True)
        legacy(s).save(out / "ic_launcher.png")
        legacy(s, round_mask=True).save(out / "ic_launcher_round.png")
    for d, s in FG.items():
        out = ROOT / f"mipmap-{d}"; out.mkdir(parents=True, exist_ok=True)
        foreground(s).save(out / "ic_launcher_foreground.png")
    legacy(512).save(ROOT.parent.parent.parent / "ic_launcher_preview.png")
    print("trade icon generated.")

if __name__ == "__main__":
    main()
