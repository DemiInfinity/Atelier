package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// Profile pictures: checking, decoding and resizing an image.
//
// An upload (or a GitHub avatar) is never served as it arrived. It must be a
// PNG, JPEG or WebP by its magic bytes (the file name and the browser's
// Content-Type are ignored), with nothing appended after the image's end (a
// polyglot), and with sane dimensions read from the header before any pixels
// are decoded (a decompression bomb is refused without allocating it). The
// pixels are then cropped to a square and re-encoded as fresh PNGs in
// avatarSizes, which drops every byte of metadata (EXIF, GPS, comments, ICC).

const (
	avatarMaxBytes  = 5 << 20
	avatarMaxSide   = 4096
	avatarMaxPixels = 4096 * 4096
)

// avatarSizes are the stored square sizes, in pixels.
var avatarSizes = []int{64, 160, 460}

var (
	errAvatarTooBig  = errors.New("That picture is over 5 MB. Pick a smaller one.")
	errAvatarType    = errors.New("Use a PNG, JPEG or WebP picture.")
	errAvatarExtra   = errors.New("That file has something extra hidden after the picture, so it wasn't used. Export it again as a plain PNG, JPEG or WebP.")
	errAvatarHuge    = errors.New("That picture is too large: keep it within 4096 × 4096 pixels.")
	errAvatarBroken  = errors.New("That picture couldn't be read. It may be damaged; try exporting it again.")
	errAvatarTooTiny = errors.New("That picture is empty.")
)

// avatarKind names the format by its magic bytes, or "".
func avatarKind(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	}
	return ""
}

// avatarEnd returns where the image data ends, so anything after it can be
// refused. ok is false when the structure can't be followed.
func avatarEnd(kind string, b []byte) (end int, ok bool) {
	switch kind {
	case "png":
		p := 8
		for p+12 <= len(b) {
			n := int(binary.BigEndian.Uint32(b[p:]))
			if n < 0 || n > len(b) {
				return 0, false
			}
			typ := string(b[p+4 : p+8])
			p += 12 + n
			if p > len(b) {
				return 0, false
			}
			if typ == "IEND" {
				return p, true
			}
		}
		return 0, false
	case "jpeg":
		return jpegEnd(b)
	case "webp":
		n := int(binary.LittleEndian.Uint32(b[4:8]))
		end := 8 + n
		if n < 4 || end > len(b) {
			return 0, false
		}
		if n%2 == 1 && end < len(b) && b[end] == 0 {
			end++ // RIFF chunks are padded to an even size
		}
		return end, true
	}
	return 0, false
}

// jpegEnd follows the JPEG marker segments (and entropy-coded data after
// each SOS) to the end-of-image marker.
func jpegEnd(b []byte) (int, bool) {
	p := 2 // after SOI
	for p+1 < len(b) {
		if b[p] != 0xFF {
			return 0, false
		}
		m := b[p+1]
		switch {
		case m == 0xFF: // fill byte
			p++
			continue
		case m == 0xD9: // EOI
			return p + 2, true
		case m == 0x01 || (m >= 0xD0 && m <= 0xD7): // no length
			p += 2
			continue
		}
		if p+4 > len(b) {
			return 0, false
		}
		n := int(binary.BigEndian.Uint16(b[p+2:]))
		if n < 2 {
			return 0, false
		}
		p += 2 + n
		if p > len(b) {
			return 0, false
		}
		if m == 0xDA { // SOS: skip the entropy-coded data to the next marker
			for p+1 < len(b) {
				if b[p] == 0xFF && b[p+1] != 0x00 && !(b[p+1] >= 0xD0 && b[p+1] <= 0xD7) && b[p+1] != 0xFF {
					break
				}
				p++
			}
		}
	}
	return 0, false
}

// processAvatar checks an image and returns it as square PNGs, by size, plus
// a short hash of the result.
func processAvatar(b []byte) (map[int][]byte, string, error) {
	if len(b) == 0 {
		return nil, "", errAvatarTooTiny
	}
	if len(b) > avatarMaxBytes {
		return nil, "", errAvatarTooBig
	}
	kind := avatarKind(b)
	if kind == "" {
		return nil, "", errAvatarType
	}
	end, ok := avatarEnd(kind, b)
	if !ok {
		return nil, "", errAvatarBroken
	}
	if end != len(b) {
		return nil, "", errAvatarExtra
	}
	var cfg image.Config
	var err error
	switch kind {
	case "png":
		cfg, err = png.DecodeConfig(bytes.NewReader(b))
	case "jpeg":
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(b))
	case "webp":
		cfg, err = webp.DecodeConfig(bytes.NewReader(b))
	}
	if err != nil {
		return nil, "", errAvatarBroken
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", errAvatarBroken
	}
	if cfg.Width > avatarMaxSide || cfg.Height > avatarMaxSide || cfg.Width*cfg.Height > avatarMaxPixels {
		return nil, "", errAvatarHuge
	}
	var img image.Image
	switch kind {
	case "png":
		img, err = png.Decode(bytes.NewReader(b))
	case "jpeg":
		img, err = jpeg.Decode(bytes.NewReader(b))
	case "webp":
		img, err = webp.Decode(bytes.NewReader(b))
	}
	if err != nil || img == nil {
		return nil, "", errAvatarBroken
	}
	bd := img.Bounds()
	if bd.Dx() != cfg.Width || bd.Dy() != cfg.Height {
		return nil, "", errAvatarBroken
	}
	// Centre square.
	side := bd.Dx()
	if bd.Dy() < side {
		side = bd.Dy()
	}
	x0 := bd.Min.X + (bd.Dx()-side)/2
	y0 := bd.Min.Y + (bd.Dy()-side)/2
	src := image.Rect(x0, y0, x0+side, y0+side)

	out := map[int][]byte{}
	h := sha256.New()
	for _, n := range avatarSizes {
		dst := image.NewNRGBA(image.Rect(0, 0, n, n))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, src, draw.Src, nil)
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, dst); err != nil {
			return nil, "", err
		}
		out[n] = buf.Bytes()
		h.Write(buf.Bytes())
	}
	return out, hex.EncodeToString(h.Sum(nil))[:16], nil
}
