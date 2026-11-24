#!/usr/bin/env python3
"""Render the README demo from a compose-quickstart-check transcript.

Every value on the frames comes from transcript.json written by
hack/compose-quickstart-check.py on a real run (the CI artifact). Nothing is
typed in by hand, so the picture cannot drift from what the server returned.

    python3 hack/render_quickstart_demo.py RUN_DIR OUT_PREFIX

writes OUT_PREFIX.gif (576x340, for the README), OUT_PREFIX.mp4 (1152x680,
for social posts) and OUT_PREFIX-frames/. Needs Pillow, and ffmpeg for the MP4.
"""
import json
from pathlib import Path
import shutil
import subprocess
import sys

from PIL import Image, ImageDraw, ImageFont

W, H = 1152, 680
BG, INK, ACCENT, RULE = '#eef3f7', '#172634', '#2b5d7d', '#b9d7ea'
CARD, KEY, VAL, PASS, FAIL = '#172634', '#8cc4e0', '#ffffff', '#86d993', '#ff8a80'


def font(names, size):
    for name in names:
        for base in (Path('C:/Windows/Fonts'), Path('/usr/share/fonts/truetype/dejavu'), Path('/usr/share/fonts')):
            p = base / name
            if p.exists():
                return ImageFont.truetype(str(p), size)
    return ImageFont.load_default(size)


SANS_B = font(['segoeuib.ttf', 'DejaVuSans-Bold.ttf'], 60)
SANS = font(['segoeui.ttf', 'DejaVuSans.ttf'], 34)
SMALL = font(['segoeui.ttf', 'DejaVuSans.ttf'], 28)
MONO = font(['consola.ttf', 'DejaVuSansMono.ttf'], 40)
MONO_S = font(['consola.ttf', 'DejaVuSansMono.ttf'], 30)


def short(h):
    return h[:12] + '…' if isinstance(h, str) and len(h) > 14 else h


def fit(d, text, base, width):
    """The largest size of this font, up to its own, that fits the width."""
    size = base.size
    f = base
    while d.textlength(text, font=f) > width and size > 12:
        size -= 2
        f = base.font_variant(size=size)
    return f


def frame(n, total, label, title, who, rows, note=''):
    im = Image.new('RGB', (W, H), BG)
    d = ImageDraw.Draw(im)
    d.text((48, 40), 'Shearwater', font=SANS_B.font_variant(size=46), fill=ACCENT)
    right = f'{n}/{total}  {label}'
    d.text((W - 48 - d.textlength(right, font=MONO), 44), right, font=MONO, fill=ACCENT)
    d.line((48, 120, W - 48, 120), fill=RULE, width=3)
    d.text((48, 140), title, font=fit(d, title, SANS_B, W - 96), fill=INK)
    d.text((48, 214), who, font=SMALL, fill=ACCENT)
    top = 262
    d.rounded_rectangle((44, top, W - 44, top + 56 + 64 * len(rows)), radius=18, fill=CARD)
    for i, (k, v, color) in enumerate(rows):
        y = top + 30 + 64 * i
        d.text((84, y), k, font=MONO, fill=KEY)
        d.text((520, y), str(v), font=fit(d, str(v), MONO, W - 44 - 40 - 520), fill=color or VAL)
    if note:
        d.text((48, H - 62), note, font=SMALL, fill=INK)
    return im


def main():
    run, prefix = Path(sys.argv[1]).resolve(), Path(sys.argv[2]).resolve()
    t = json.loads((run / 'transcript.json').read_text())
    summary = json.loads((run / 'summary.json').read_text())

    def find(scene, who, pred):
        return next(e for e in t if e['scene'] == scene and e['who'] == who and pred(e))

    upd = find('update', 'agent', lambda e: e['request'].get('action') == 'update')['response']
    appr = find('update', 'operator', lambda e: 'approve' in e['request'])['response']
    ok = find('update', 'agent', lambda e: e['request'].get('action') == 'continue' and e['response'].get('state') == 'healthy')['response']
    bad = find('broken', 'agent', lambda e: e['request'].get('action') == 'continue')['response']
    seen = find('broken', 'page', lambda e: True)['response']
    expected = next(e['request'] for e in t if e['who'] == 'operator' and 'create' in e['request'])
    expected = expected[expected.index('--expect-body') + 1]
    rec = find('recover', 'agent', lambda e: e['request'].get('action') == 'recover')['response']
    rok = find('recover', 'agent', lambda e: e['request'].get('action') == 'continue')['response']
    images = summary['images']

    def tag(digest):
        return next((k for k, v in images.items() if v == digest or digest.endswith(v.split('@')[-1])), short(digest))

    frames = [
        frame(1, 6, 'update', 'Agent asks to update web to v2', 'agent → {"action": "update", "image": "shop-web@sha256:…"}', [
            ('state', upd['state'], None), ('applied', str(upd['applied']).lower(), None), ('plan', short(upd['plan']), None)],
            'Nothing has changed yet. The plan waits for a person.'),
        frame(2, 6, 'approve', 'You approve that exact plan', 'operator → sw task approve --confirm=<plan hash>', [
            ('state', appr['state'], None), ('approved', str(appr['approved']).lower(), None), ('plan', short(appr['plan']), None)],
            'Approval is bound to this hash; another change needs its own approval.'),
        frame(3, 6, 'continue', 'v2 is live and answered', 'agent → {"action": "continue"}', [
            ('state', ok['state'], PASS), ('http.state', ok['http']['state'], PASS), ('verifiedPods', ok['verifiedPods'], None)],
            'Every new container returned HTTP 200 with the expected text.'),
        frame(4, 6, 'bad update', 'The next image answers 200…', 'approved and applied the same way', [
            ('page', f'HTTP {seen["status"]} "{seen["body"]}"', None), ('expected text', f'"{expected}"', None),
            ('applied', str(bad['applied']).lower(), None)],
            'A status-code check would call this a success.'),
        frame(5, 6, 'not done', '…so it is not "healthy"', 'agent → {"action": "continue"}', [
            ('state', bad['state'], FAIL), ('http.state', bad['http']['state'], FAIL), ('next', 'diagnose, then approve a recovery', None)],
            'The agent cannot report this deploy as done.'),
        frame(6, 6, 'recover', 'Recovery goes back to the last verified image', 'agent → {"action": "recover"}, approved, continue', [
            ('image', f"shop-web:{tag(rec.get('image', images['v2']))}", None), ('state', rok['state'], PASS),
            ('http.state', rok['http']['state'], PASS)],
            'The server picks the image that last passed, not the agent.'),
    ]
    out = Path(str(prefix) + '-frames')
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)
    for i, im in enumerate(frames, 1):
        im.save(out / f'{i:02d}.png')
    small = [im.resize((576, 340), Image.LANCZOS).convert('P', palette=Image.ADAPTIVE, colors=64) for im in frames]
    durations = [3600, 3200, 3600, 3600, 4200, 4200]
    small[0].save(str(prefix) + '.gif', save_all=True, append_images=small[1:], duration=durations, loop=0, optimize=True)
    if shutil.which('ffmpeg'):
        concat = out / 'list.txt'
        concat.write_text(''.join(f"file '{(out / f'{i:02d}.png').as_posix()}'\nduration {d / 1000}\n"
                                  for i, d in enumerate(durations, 1)) + f"file '{(out / '06.png').as_posix()}'\n")
        subprocess.run(['ffmpeg', '-v', 'error', '-y', '-f', 'concat', '-safe', '0', '-i', str(concat), '-vf',
                        'fps=30,format=yuv420p', '-c:v', 'libx264', '-crf', '18', '-movflags', '+faststart',
                        str(prefix) + '.mp4'], check=True)
    print('wrote', str(prefix) + '.gif')


if __name__ == '__main__':
    main()
