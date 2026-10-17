"""Tiny terminal-session recorder: runs a program in a pty, feeds its output to a
terminal emulator (pyte) and renders the *screen* as an SVG "screenshot".

Used by gen.py. Needs: python3 and `pip install pyte`.
"""
import fcntl
import os
import pty
import re
import select
import struct
import termios
import time
from xml.sax.saxutils import escape

import pyte

# GitHub-dark-like palette
FG, BG = "#c9d1d9", "#0d1117"
NAMED = {
    "default": None,
    "black": "#484f58", "red": "#ff7b72", "green": "#3fb950", "brown": "#d29922",
    "blue": "#58a6ff", "magenta": "#bc8cff", "cyan": "#39c5cf", "white": "#b1bac4",
    "brightblack": "#6e7681", "brightred": "#ffa198", "brightgreen": "#56d364",
    "brightbrown": "#e3b341", "brightblue": "#79c0ff", "brightmagenta": "#d2a8ff",
    "brightcyan": "#56d4dd", "brightwhite": "#f0f6fc",
}
CW, CH, PAD, BAR = 8.4, 18.0, 18, 38


def color(name, default):
    if name == "default":
        return default
    if len(name) == 6 and all(c in "0123456789abcdefABCDEF" for c in name):
        return "#" + name.lower()
    return NAMED.get(name) or default


def _normalize(data):
    """pyte does not understand colon-separated SGR sub-parameters. Bubble Tea
    uses them for underline (ESC[4:0m = underline off); without this an
    "underline off" would be read as "underline on" and stick."""
    data = re.sub(rb"\x1b\[4:0m", b"\x1b[24m", data)
    return re.sub(rb"\x1b\[4:[1-5]m", b"\x1b[4m", data)


class Term:
    def __init__(self, argv, cwd, env, rows=40, cols=400):
        self.screen = pyte.Screen(cols, rows)
        self.stream = pyte.ByteStream(self.screen)
        self.rows, self.cols = rows, cols
        self.status = None
        pid, fd = pty.fork()
        if pid == 0:
            os.chdir(cwd)
            os.execvpe(argv[0], argv, env)  # PATH lookup, so "docker" works too
        self.pid, self.fd = pid, fd
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))

    def prompt(self, cmdline):
        """Print a fake shell prompt line before the program's own output."""
        self.stream.feed(b"\x1b[1;32m$\x1b[0m " + cmdline.encode() + b"\r\n")

    def pump(self, t):
        end = time.time() + t
        while time.time() < end:
            r, _, _ = select.select([self.fd], [], [], 0.05)
            if r:
                try:
                    d = os.read(self.fd, 65536)
                except OSError:
                    return
                if not d:
                    return
                self.stream.feed(_normalize(d))

    def send(self, *keys, wait=0.9):
        for k in keys:
            os.write(self.fd, k)
            time.sleep(0.07)
        self.pump(wait)

    def type(self, text, wait=0.6):
        for ch in text:
            os.write(self.fd, ch.encode())
            time.sleep(0.04)
        self.pump(wait)

    def wait_exit(self, timeout=15):
        end = time.time() + timeout
        while time.time() < end:
            self.pump(0.1)
            p, st = os.waitpid(self.pid, os.WNOHANG)
            if p:
                self.status = os.WEXITSTATUS(st) if os.WIFEXITED(st) else -1
                self.pump(0.2)
                return self.status
        return None

    def kill(self):
        try:
            os.kill(self.pid, 9)
            os.waitpid(self.pid, 0)
        except OSError:
            pass

    def text(self):
        return "\n".join(l.rstrip() for l in self.screen.display).rstrip()

    def rows_cells(self):
        out = []
        for y in range(self.rows):
            out.append([self.screen.buffer[y][x] for x in range(self.cols)])
        return out


def _replace_in_row(cells, subs):
    """Apply text substitutions to one row, keeping the style of the first
    replaced character for the whole replacement."""
    text = "".join(c.data for c in cells)
    for old, new in subs:
        while True:
            i = text.find(old)
            if i < 0:
                break
            style = cells[i]
            repl = [style._replace(data=ch) for ch in new]
            cells = cells[:i] + repl + cells[i + len(old):]
            text = text[:i] + new + text[i + len(old):]
    return cells


def render_svg(term, title, path, subs=(), max_cols=112, top=0, bottom=None):
    rows = term.rows_cells()
    rows = [_replace_in_row(r, subs) for r in rows]
    # drop rows above `top`, and trailing blank rows
    rows = rows[top:bottom]
    while rows and not "".join(c.data for c in rows[-1]).strip():
        rows.pop()
    if not rows:
        raise RuntimeError("empty screen for " + path)
    # Wrap long rows like a real terminal of `max_cols` columns would.
    wrapped = []
    for r in rows:
        n = len("".join(c.data for c in r).rstrip())
        if n <= max_cols:
            wrapped.append(r)
            continue
        for i in range(0, n, max_cols):
            wrapped.append(r[i:i + max_cols])
    rows = wrapped
    width = max(len("".join(c.data for c in r).rstrip()) for r in rows)
    width = max(min(width, max_cols), 40)

    w = PAD * 2 + width * CW
    h = BAR + PAD + len(rows) * CH + PAD
    o = []
    o.append(f'<svg xmlns="http://www.w3.org/2000/svg" width="{w:.0f}" height="{h:.0f}" viewBox="0 0 {w:.0f} {h:.0f}" role="img" aria-label="{escape(title)}">')
    o.append('<style>text{font-family:SFMono-Regular,"SF Mono",Menlo,Consolas,"DejaVu Sans Mono","Liberation Mono",monospace;font-size:14px;white-space:pre}</style>')
    o.append(f'<rect width="{w:.0f}" height="{h:.0f}" rx="10" fill="{BG}"/>')
    o.append(f'<path d="M0 10a10 10 0 0 1 10-10h{w-20:.0f}a10 10 0 0 1 10 10v{BAR-10}H0z" fill="#161b22"/>')
    for i, c in enumerate(("#ff5f56", "#ffbd2e", "#27c93f")):
        o.append(f'<circle cx="{18+i*20}" cy="{BAR/2:.0f}" r="6" fill="{c}"/>')
    o.append(f'<text x="{w/2:.0f}" y="{BAR/2+5:.0f}" text-anchor="middle" fill="#8b949e" style="font-size:13px">{escape(title)}</text>')

    y0 = BAR + PAD
    for ri, cells in enumerate(rows):
        cells = cells[:width]
        # background runs
        x = 0
        while x < len(cells):
            c = cells[x]
            bg = color(c.bg, None)
            fg = color(c.fg, FG)
            if c.reverse:
                bg, fg = (color(c.fg, FG), color(c.bg, BG))
            if bg:
                x2 = x
                while x2 < len(cells):
                    d = cells[x2]
                    dbg = color(d.bg, None)
                    if d.reverse:
                        dbg = color(d.fg, FG)
                    if dbg != bg:
                        break
                    x2 += 1
                o.append(f'<rect x="{PAD+x*CW:.1f}" y="{y0+ri*CH:.1f}" width="{(x2-x)*CW:.1f}" height="{CH:.1f}" fill="{bg}"/>')
                x = x2
            else:
                x += 1
        # text runs
        line = "".join(c.data for c in cells).rstrip()
        if not line:
            continue
        spans, i = [], 0
        while i < len(line):
            c = cells[i]
            fg = color(c.fg, FG)
            if c.reverse:
                fg = color(c.bg, BG)
            # mcplogview never emits underline; pyte reports stray ones, so ignore it
            key = (fg, c.bold, c.italics, False)
            j = i
            while j < len(line):
                d = cells[j]
                dfg = color(d.fg, FG)
                if d.reverse:
                    dfg = color(d.bg, BG)
                if (dfg, d.bold, d.italics, False) != key:
                    break
                j += 1
            style = f'fill="{key[0]}"'
            if key[1]:
                style += ' font-weight="bold"'
            if key[2]:
                style += ' font-style="italic"'
            if key[3]:
                style += ' text-decoration="underline"'
            spans.append(f'<tspan {style}>{escape(line[i:j])}</tspan>')
            i = j
        o.append(f'<text x="{PAD}" y="{y0+ri*CH+13:.1f}" xml:space="preserve" textLength="{len(line)*CW:.1f}" lengthAdjust="spacing">{"".join(spans)}</text>')
    o.append("</svg>")
    with open(path, "w") as f:
        f.write("\n".join(o) + "\n")
    return "\n".join("".join(c.data for c in r).rstrip() for r in rows)
