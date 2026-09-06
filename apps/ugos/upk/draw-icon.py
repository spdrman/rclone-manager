#!/usr/bin/env python3
"""Draws the App Center icon, rather than checking a PNG into the tree.

Same reasoning as apps/synology/spk/icon.go, which draws the DSM launcher
icon in Go: a reviewer can read what ships out of this file instead of
opening a binary nobody can diff, the output is byte-identical on every
machine, and there is no opaque blob in the repository.

Stdlib only and no Pillow, because this runs on whatever machine stages
the package, including a UGOS Pro NAS whose Python has nothing installed
beside it. A PNG is a signature, three chunks and a zlib stream, and that
is all this writes.

The mark is deliberately plain. It is a placeholder, and nothing about it
imitates a UGREEN icon.
"""

import struct
import sys
import zlib

SIZE = 256

# Flat and neutral, and the same navy apps/synology/spk/icon.go uses, so
# the two packages of the same product do not look like two products.
BACKGROUND = (0x1F, 0x3A, 0x5F)
MARK = (0xE8, 0xEE, 0xF5)


def pixels():
    """The mark: three stacked bars, the way a retained backup set reads."""
    rows = []
    bar_left, bar_right = SIZE // 5, SIZE - SIZE // 5
    bars = [(SIZE // 4, SIZE // 4 + SIZE // 12),
            (SIZE // 2 - SIZE // 24, SIZE // 2 + SIZE // 24),
            (SIZE - SIZE // 4 - SIZE // 12, SIZE - SIZE // 4)]
    for y in range(SIZE):
        row = bytearray()
        inside_bar = any(top <= y < bottom for top, bottom in bars)
        for x in range(SIZE):
            if inside_bar and bar_left <= x < bar_right:
                row += bytes(MARK)
            else:
                row += bytes(BACKGROUND)
        rows.append(bytes(row))
    return rows


def chunk(kind, body):
    return (struct.pack(">I", len(body)) + kind + body
            + struct.pack(">I", zlib.crc32(kind + body) & 0xFFFFFFFF))


def png():
    raw = b"".join(b"\x00" + row for row in pixels())
    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", SIZE, SIZE, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(raw, 9))
            + chunk(b"IEND", b""))


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: draw-icon.py <output.png>")
    with open(sys.argv[1], "wb") as f:
        f.write(png())
