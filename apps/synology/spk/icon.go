// The launcher icon, drawn in code rather than checked in as PNG files.
//
// Three things follow from generating it. A reviewer can read what ships
// from this file instead of opening a binary nobody can diff; the output
// is byte-identical on every machine, which a hand-exported PNG is not,
// and byte-identical output is the whole point of a package that claims to
// be reproducible; and there is no opaque blob in the tree.
//
// Several sizes ship because the launcher config references its images
// through a templated name and Synology's documentation does not say which
// sizes DSM asks for. Guessing one and being wrong renders nothing at all,
// so the package carries the common set instead.
//
// The mark is deliberately plain. It is a placeholder, and nothing about
// it imitates a Synology icon.
package spk

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// LauncherIconSizes are the pixel sizes shipped under the DSM UI
// directory's images/ folder.
//
// The launcher config references them as "images/backup_manager_{0}.png",
// the templated form Synology's own desktop-application example uses, and
// DSM substitutes a size for {0}. The documentation does not say which
// sizes it asks for, so the package ships the common DSM icon sizes
// rather than guessing one and having the launcher render nothing.
var LauncherIconSizes = []int{16, 24, 32, 48, 64, 72, 256}

// Icon palette. Deliberately flat and neutral: this is a placeholder
// mark, not artwork, and nothing about it imitates a Synology icon.
var (
	iconBackground = color.NRGBA{R: 0x1f, G: 0x3a, B: 0x5f, A: 0xff}
	iconForeground = color.NRGBA{R: 0xf2, G: 0xf5, B: 0xf8, A: 0xff}
)

// renderIcon draws the package icon at size×size and encodes it as PNG.
//
// Generated rather than checked in as binary blobs: a reviewer can read
// what the shipped icon is from this function, the output is identical on
// every machine (which a hand-exported PNG is not), and there is no
// opaque file in the tree whose contents nobody can diff.
func renderIcon(size int) ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	radius := size / 5
	for y := range size {
		for x := range size {
			if insideRoundedSquare(x, y, size, radius) {
				img.SetNRGBA(x, y, iconBackground)
			}
		}
	}

	// A downward arrow into a tray: the product pulls backups down from a
	// remote and lands them on local storage.
	unit := float64(size) / 16
	px := func(v float64) int { return int(v * unit) }

	fill := func(x0, y0, x1, y1 int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				if x >= 0 && y >= 0 && x < size && y < size {
					img.SetNRGBA(x, y, iconForeground)
				}
			}
		}
	}

	// Shaft.
	fill(px(7), px(3), px(9), px(8.5))
	// Head: a solid triangle, drawn row by row.
	headTop, headBottom := px(8), px(11)
	for y := headTop; y < headBottom; y++ {
		spread := float64(headBottom-y) / float64(headBottom-headTop)
		half := int(spread * 3 * unit)
		fill(size/2-half, y, size/2+half, y+1)
	}
	// Tray.
	fill(px(3.5), px(12), px(12.5), px(13))
	fill(px(3.5), px(10.5), px(4.5), px(13))
	fill(px(11.5), px(10.5), px(12.5), px(13))

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode %dpx icon: %w", size, err)
	}
	return buf.Bytes(), nil
}

// insideRoundedSquare reports whether a pixel is inside a square with
// rounded corners.
func insideRoundedSquare(x, y, size, radius int) bool {
	inCorner := func(cx, cy int) bool {
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= radius*radius
	}
	switch {
	case x < radius && y < radius:
		return inCorner(radius, radius)
	case x >= size-radius && y < radius:
		return inCorner(size-radius-1, radius)
	case x < radius && y >= size-radius:
		return inCorner(radius, size-radius-1)
	case x >= size-radius && y >= size-radius:
		return inCorner(size-radius-1, size-radius-1)
	}
	return true
}

// renderLauncherIcons produces every size the DSM desktop launcher may
// ask for.
func renderLauncherIcons() ([]assetFile, error) {
	out := make([]assetFile, 0, len(LauncherIconSizes))
	for _, size := range LauncherIconSizes {
		body, err := renderIcon(size)
		if err != nil {
			return nil, err
		}
		out = append(out, assetFile{
			Name: fmt.Sprintf("backup_manager_%d.png", size),
			Body: body,
		})
	}
	return out, nil
}
