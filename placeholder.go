// Copyright (c) 2026 karamble
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package tilecache

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// defaultPlaceholder renders the default placeholder: a 256x256 tile in dark
// grey #11151a.
func defaultPlaceholder() ([]byte, error) {
	img := image.NewPaletted(image.Rect(0, 0, 256, 256), color.Palette{color.RGBA{R: 0x11, G: 0x15, B: 0x1a, A: 0xff}})
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
