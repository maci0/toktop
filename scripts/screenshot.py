#!/usr/bin/env python3
# Copyright (C) 2026 Marcel W. Wysocki
# SPDX-License-Identifier: MIT
"""Render a tmux ANSI capture (SGR truecolor/256/8) to a PNG screenshot.

Usage: screenshot.py <capture.txt> <out.png> [scale] [cols] [rows]
       screenshot.py -h|--help

scale defaults to 2; cols and rows default to 0, which measures the capture
instead of taking the pane geometry from the caller.

Dependencies are declared in scripts/requirements.txt (pyte, wcwidth,
pillow) and installed --no-deps, so all three are required, not just the two
this file imports directly.

The capture must come from `tmux capture-pane -e -p` (one line per row,
escape sequences preserved). Rendering uses Meslo (LG Nerd Font where it is
installed), the family the shipped capture was made with, on the toktop.ai
dark base the TUI paints. Set TOKTOP_SCREENSHOT_FONT to a regular-weight
.ttf when no Meslo build
is installed where the script looks.

Exit codes: 0 the image was written, 1 a runtime failure (unreadable
capture, missing dependency, no usable font), 2 a usage error (unknown
option, missing or extra argument, a value that is not a non-negative
integer). The written path and its size go to stdout; everything else
goes to stderr.
"""

import os
import re
import string
import sys
from pathlib import Path
from typing import TextIO

RGB = tuple[int, int, int]

ANSI_RE = re.compile(
    rb"\x1b(?:\[[0-9;?]*[a-zA-Z]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()][A-Z0-9])",
)

BG: RGB = (13, 17, 23)  # #0d1117, matches internal/ui/theme.go cBase
FG_DEFAULT: RGB = (215, 221, 229)  # #d7dde5

# Font roots cover the common layouts; exact subdirectories vary by distro.
FONT_ROOTS: tuple[str, ...] = (
    "/usr/share/fonts",
    "/usr/local/share/fonts",
    str(Path.home() / ".local/share/fonts"),
    "/Library/Fonts",
    str(Path.home() / "Library/Fonts"),
    str(Path(os.environ.get("WINDIR", "C:\\Windows")) / "Fonts"),
    str(Path.home() / "AppData/Local/Microsoft/Windows/Fonts"),
)

# The 16 ANSI colors as SGR 30-37/90-97, tuned to the dashboard's palette.
ANSI16: dict[int, RGB] = {
    0: (95, 107, 122),  # black-ish (cBorder)
    1: (227, 109, 109),  # red
    2: (76, 195, 138),  # green / accent
    3: (227, 179, 65),  # yellow / warm
    4: (122, 162, 212),  # blue
    5: (201, 149, 108),  # unused ANSI magenta; sand, not purple
    6: (94, 200, 216),  # cyan
    7: (215, 221, 229),  # white / fg
}

# Positional arguments are src out [scale] [cols] [rows]; scale, cols and rows
# are optional and default to 2x, autodetected. 0 is a valid cols or rows, so
# autodetect is spelled as 0 rather than as an absent argument.
MIN_ARGS = 2
OPTIONAL_ARGS: tuple[tuple[str, int], ...] = (
    ("scale", 2),
    ("cols", 0),
    ("rows", 0),
)
MAX_ARGS = MIN_ARGS + len(OPTIONAL_ARGS)

# The two help spellings Go's flag package also accepts, so the two CLIs in
# this repo answer 'script --help' the same way.
HELP_FLAGS: frozenset[str] = frozenset({"-h", "--help"})

# A truecolor SGR payload is #rrggbb.
HEX_DIGITS = 6


def _search(pattern: str) -> list[str]:
    hits: list[str] = []
    for root in FONT_ROOTS:
        hits.extend(str(p) for p in sorted(Path(root).glob(f"**/{pattern}")))
    return hits


def resolve_fonts() -> tuple[str, str]:
    """Return (regular, bold) ttf paths for the dashboard's font family.

    TOKTOP_SCREENSHOT_FONT pins an explicit regular-weight face; its Bold
    sibling is used when present. Otherwise the standard font roots are
    searched, preferring a Nerd Font build of Meslo.

    Raises:
        SystemExit: no usable regular-weight face was found.
    """
    if override := os.environ.get("TOKTOP_SCREENSHOT_FONT"):
        pinned = Path(override)
        if not pinned.is_file():
            print(
                f"screenshot.py: TOKTOP_SCREENSHOT_FONT: no such file: {override}",
                file=sys.stderr,
            )
            raise SystemExit(1)
        # Only the file name is rewritten: a directory whose own name carries
        # "Regular" would otherwise be turned into a path that does not exist,
        # and the check below would fall back to the regular face for the bold
        # one, silently rendering every bold glyph in the wrong weight.
        bold_name = pinned.name.replace("Regular", "Bold")
        sibling = pinned.with_name(bold_name)
        return override, str(sibling) if sibling.is_file() else override
    regular = _search("Meslo*Nerd*[Rr]egular*.ttf") or _search("Meslo*.ttf")
    if not regular:
        print(
            "screenshot.py: no Meslo Nerd Font found; install one or set "
            "TOKTOP_SCREENSHOT_FONT to a regular-weight .ttf",
            file=sys.stderr,
        )
        raise SystemExit(1)
    bold = _search("Meslo*Nerd*[Bb]old*.ttf") or regular
    return regular[0], bold[0]


def usage(out: TextIO) -> None:
    """Write the module docstring, the usage text, to out."""
    print((__doc__ or "screenshot.py").strip(), file=out)


def parse_int(name: str, raw: str) -> int:
    """Parse raw as an integer, naming the argument in any failure."""
    try:
        return int(raw)
    except ValueError:
        print(f"screenshot.py: {name} must be an integer, got {raw!r}", file=sys.stderr)
        raise SystemExit(2) from None


def parse_optional(args: list[str]) -> tuple[int, int, int]:
    """Parse the optional scale, cols and rows, falling back to their defaults.

    The values are positional and may be omitted only from the right, so the
    count of what was passed decides which default each missing one gets.
    """
    parsed = [
        parse_int(name, args[MIN_ARGS + i]) if len(args) > MIN_ARGS + i else default
        for i, (name, default) in enumerate(OPTIONAL_ARGS)
    ]
    scale, cols, rows = parsed
    return scale, cols, rows


def main() -> None:
    """Validate the command line and render the capture."""
    args = sys.argv[1:]
    # Options are named before the help flag is answered and before the arity
    # is judged: a lone mistyped flag is one argument, so the count check ran
    # first and answered it with the whole usage screen, which says nothing
    # about the flag that was wrong. The help spellings are exempt here and
    # read below, so 'screenshot.py --help --bogus' still exits 2, the way
    # 'toktop --help --bogus' does, instead of printing help and exiting 0
    # over a flag the parser never looked at.
    for a in args:
        if a.startswith("-") and a not in HELP_FLAGS:
            print(
                f"screenshot.py: unknown option {a!r} (see 'screenshot.py --help')",
                file=sys.stderr,
            )
            raise SystemExit(2)
    if any(a in HELP_FLAGS for a in args):
        usage(sys.stdout)
        raise SystemExit(0)
    if len(args) < MIN_ARGS:
        usage(sys.stderr)
        raise SystemExit(2)
    if len(args) > MAX_ARGS:
        print(
            f"screenshot.py: unexpected argument {args[MAX_ARGS]!r} "
            "(see 'screenshot.py --help')",
            file=sys.stderr,
        )
        raise SystemExit(2)
    src, out = args[0], args[1]
    # Exact pane geometry keeps pyte from wrapping or scrolling; pass the
    # values of #{pane_width} #{pane_height} from the capturing tmux session.
    # 0 in either means autodetect from the capture, so zero stays valid.
    scale, cols, rows = parse_optional(args)
    if scale < 1:
        print(f"screenshot.py: scale must be >= 1, got {scale}", file=sys.stderr)
        raise SystemExit(2)
    if cols < 0:
        print(f"screenshot.py: cols must be >= 0, got {cols}", file=sys.stderr)
        raise SystemExit(2)
    if rows < 0:
        print(f"screenshot.py: rows must be >= 0, got {rows}", file=sys.stderr)
        raise SystemExit(2)
    render(src, out, scale, cols, rows)


def render(src: str, out: str, scale: int, cols: int, rows: int) -> None:
    """Draw the capture in src as a PNG at out, one cell per terminal cell.

    A cols or rows of 0 measures the capture instead of taking the pane
    geometry from the caller.
    """
    try:
        # Imported here, not at module scope, so `screenshot.py --help` works
        # without pyte/pillow and the except branch below reports them by name.
        import pyte  # noqa: PLC0415
        from PIL import Image, ImageDraw, ImageFont  # noqa: PLC0415
    except ImportError as e:
        print(f"screenshot.py: missing dependency ({e})", file=sys.stderr)
        print(
            "install with: make scripts-env, then run "
            "dist/scripts-env/bin/python scripts/screenshot.py",
            file=sys.stderr,
        )
        raise SystemExit(1) from e

    try:
        with Path(src).open("rb") as f:
            data = f.read().rstrip(b"\r\n")
    except OSError as e:
        print(f"screenshot.py: {src}: {e}", file=sys.stderr)
        raise SystemExit(1) from e

    lines = [ln.rstrip(b"\r") for ln in data.split(b"\n")]
    if cols <= 0:
        # count runes after stripping escapes: braille dots are 3 UTF-8 bytes
        cols = max(len(ANSI_RE.sub(b"", ln).decode("utf-8", "replace")) for ln in lines)
    if rows <= 0:
        rows = len(lines)
    # Rejoin with CRLF: capture-pane trims trailing spaces, so bare \n would
    # start each row at the previous row's final column instead of col 0.
    text = b"\r\n".join(lines).decode("utf-8", errors="replace")
    screen = pyte.Screen(cols, rows)
    stream = pyte.Stream(screen)
    stream.feed(text)
    # feed() leaves the cursor on a final empty line when the capture ends in
    # a newline; rstrip above prevents that, so screen rows map 1:1. Cells
    # holding "" are the trailing half of a double-width glyph, drawn from
    # its leading cell, so they render as nothing.

    cell_w = 9 * scale
    cell_h = 19 * scale
    font_size = 16 * scale
    font_path, font_bold_path = resolve_fonts()
    font = ImageFont.truetype(font_path, font_size)
    font_bold = ImageFont.truetype(font_bold_path, font_size)

    img = Image.new("RGB", (cols * cell_w, rows * cell_h), BG)
    draw = ImageDraw.Draw(img)

    for y in range(rows):
        line = screen.buffer[y]
        x = 0
        while x < cols:
            ch = line[x].data
            if ch == " " and line[x].bg is None and line[x].fg is None:
                x += 1
                continue
            fg = ansi_or_truecolor(line[x].fg) or FG_DEFAULT
            bg = ansi_or_truecolor(line[x].bg)
            bold = line[x].bold
            if bg is not None:
                draw.rectangle(
                    [
                        x * cell_w,
                        y * cell_h,
                        (x + 1) * cell_w - 1,
                        (y + 1) * cell_h - 1,
                    ],
                    fill=bg,
                )
            face = font_bold if bold else font
            draw.text(
                (x * cell_w + cell_w // 2, y * cell_h + cell_h // 2),
                ch,
                font=face,
                fill=fg,
                anchor="mm",
            )
            x += 1

    img.save(out)
    print(f"{out}: {img.width}x{img.height} from {cols}x{rows} cells")


def ansi_or_truecolor(color: str | None) -> RGB | None:
    """Map pyte color names to RGB tuples.

    Returns:
        An RGB tuple, or None when the name is unknown or default.
    """
    if color is None:
        return None
    v = color.lstrip("#")
    if len(v) == HEX_DIGITS and all(c in string.hexdigits for c in v):
        return (int(v[0:2], 16), int(v[2:4], 16), int(v[4:6], 16))
    named: dict[str, int | None] = {
        "black": 0,
        "red": 1,
        "green": 2,
        "brown": 3,
        "blue": 4,
        "magenta": 5,
        "cyan": 6,
        "white": 7,
        "brightblack": 8,
        "brightred": 9,
        "brightgreen": 10,
        "brightbrown": 11,
        "brightyellow": 11,
        "brightblue": 12,
        "brightmagenta": 13,
        "brightcyan": 14,
        "brightwhite": 15,
        "default": None,
    }
    key = color.lower().replace("light", "bright")
    idx = named.get(key)
    if idx is None:
        return None
    if idx < len(ANSI16):
        return ANSI16[idx]
    # bright variants reuse the palette
    return ANSI16[idx - len(ANSI16)]


if __name__ == "__main__":
    main()
