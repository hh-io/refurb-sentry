#!/usr/bin/env python3
"""重新生成 assets/icon.png(推送通知图标)。改配色或尺寸时跑一遍即可。

    uv run --with pillow assets/make-icon.py
"""
from PIL import Image, ImageDraw

S = 4  # supersample 倍数,缩回目标尺寸时得到抗锯齿边缘
W = 512 * S
BG, FG, ACCENT = "#1c1c1e", "#f5f5f7", "#ff6b47"

img = Image.new("RGBA", (W, W), (0, 0, 0, 0))
d = ImageDraw.Draw(img)
d.rounded_rectangle([0, 0, W - 1, W - 1], radius=112 * S, fill=BG)

sc = lambda pts: [(x * S, y * S) for x, y in pts]

# 标价签。首尾多绕两个点,让闭合处的接缝被后画的线段盖住,否则会留一个台阶。
tag = [(92, 92), (244, 92), (366, 214), (214, 366), (92, 244)]
d.line(sc(tag + tag[:2]), fill=FG, width=26 * S, joint="curve")
d.ellipse(sc([(138, 138), (186, 186)]), fill=FG)

# 降价箭头单独放在右下角并与标签留出间距——两个形状叠在一起时,
# 缩到通知里的 40pt 就糊成一团了。
ax, top, bot, half = 378, 280, 424, 48
d.line(sc([(ax, top), (ax, bot)]), fill=ACCENT, width=30 * S)
d.line(sc([(ax - half, bot - half), (ax, bot), (ax + half, bot - half)]),
       fill=ACCENT, width=30 * S, joint="curve")

img.resize((512, 512), Image.LANCZOS).save("assets/icon.png")
