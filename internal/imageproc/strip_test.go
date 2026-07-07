package imageproc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// testImage builds a small non-uniform image so encoders emit real scan data.
func testImage(t *testing.T) image.Image {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			m.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 16), B: 128, A: 255})
		}
	}
	return m
}

func encodeJPEG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func encodePNG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// jpegSegment builds a JPEG marker segment (0xFF, marker, 2-byte length, body).
func jpegSegment(marker byte, body []byte) []byte {
	seg := []byte{0xFF, marker}
	l := len(body) + 2
	seg = append(seg, byte(l>>8), byte(l))
	return append(seg, body...)
}

// spliceAfterSOI inserts segments immediately after the JPEG SOI marker.
func spliceAfterSOI(jpegBytes []byte, segments ...[]byte) []byte {
	out := append([]byte{}, jpegBytes[:2]...)
	for _, s := range segments {
		out = append(out, s...)
	}
	return append(out, jpegBytes[2:]...)
}

// pngChunk builds a valid PNG chunk with a correct CRC.
func pngChunk(ctype string, data []byte) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(len(data)))
	b = append(b, ctype...)
	b = append(b, data...)
	crc := crc32.ChecksumIEEE(append([]byte(ctype), data...))
	var crcb [4]byte
	binary.BigEndian.PutUint32(crcb[:], crc)
	return append(b, crcb[:]...)
}

// spliceAfterPNGHeader inserts chunks right after the 8-byte signature + IHDR
// (a fixed 25-byte chunk), i.e. before IDAT.
func spliceAfterPNGHeader(pngBytes []byte, chunks ...[]byte) []byte {
	const headerEnd = 8 + 25 // signature + IHDR chunk
	out := append([]byte{}, pngBytes[:headerEnd]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, pngBytes[headerEnd:]...)
}

func TestStripJPEG(t *testing.T) {
	orig := encodeJPEG(t, testImage(t))

	// Go's jpeg encoder emits none of the metadata markers we drop, so a strip
	// of the metadata-laden image must reproduce the original bytes exactly.
	exif := jpegSegment(0xE1, append([]byte("Exif\x00\x00"), []byte("GPS 12.34,56.78 secret")...))
	com := jpegSegment(0xFE, []byte("a private comment"))
	iptc := jpegSegment(0xED, []byte("Photoshop 3.0 IPTC payload"))
	withMeta := spliceAfterSOI(orig, exif, com, iptc)

	// The spliced input is still a valid, decodable JPEG carrying the metadata.
	if !bytes.Contains(withMeta, []byte("Exif")) || !bytes.Contains(withMeta, []byte("secret")) {
		t.Fatal("test setup: spliced JPEG missing expected metadata")
	}
	if _, err := jpeg.Decode(bytes.NewReader(withMeta)); err != nil {
		t.Fatalf("test setup: spliced JPEG does not decode: %v", err)
	}

	out, err := stripJPEG(withMeta)
	if err != nil {
		t.Fatalf("stripJPEG: %v", err)
	}
	if !bytes.Equal(out, orig) {
		t.Errorf("stripped JPEG (%d bytes) is not byte-identical to the metadata-free original (%d bytes)", len(out), len(orig))
	}
	if bytes.Contains(out, []byte("Exif")) || bytes.Contains(out, []byte("secret")) || bytes.Contains(out, []byte("IPTC")) {
		t.Error("stripped JPEG still contains metadata")
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("stripped JPEG does not decode: %v", err)
	}
}

func TestStripJPEGNoMetadataUnchanged(t *testing.T) {
	orig := encodeJPEG(t, testImage(t))
	out, err := stripJPEG(orig)
	if err != nil {
		t.Fatalf("stripJPEG: %v", err)
	}
	if !bytes.Equal(out, orig) {
		t.Error("stripping a metadata-free JPEG changed the bytes")
	}
}

func TestStripJPEGKeepsICCAndJFIF(t *testing.T) {
	orig := encodeJPEG(t, testImage(t))
	jfif := jpegSegment(0xE0, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")) // APP0
	icc := jpegSegment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), make([]byte, 8)...))
	withColor := spliceAfterSOI(orig, jfif, icc)

	out, err := stripJPEG(withColor)
	if err != nil {
		t.Fatalf("stripJPEG: %v", err)
	}
	if !bytes.Contains(out, []byte("JFIF")) {
		t.Error("APP0 JFIF was dropped; it must be kept")
	}
	if !bytes.Contains(out, []byte("ICC_PROFILE")) {
		t.Error("APP2 ICC profile was dropped; it must be kept for colour fidelity")
	}
}

func TestStripJPEGMalformed(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "bad SOI", data: []byte{0x00, 0x01, 0x02}},
		{name: "truncated after SOI", data: []byte{0xFF, 0xD8, 0xFF}},
		{name: "truncated segment length", data: []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00}},
		{name: "segment overruns", data: []byte{0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF, 0x01}},
		{name: "bad length below 2", data: []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x01}},
		{name: "non-marker byte", data: []byte{0xFF, 0xD8, 0x12, 0x34}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := stripJPEG(tt.data); err == nil {
				t.Error("expected an error for malformed JPEG")
			}
		})
	}
}

func TestStripPNG(t *testing.T) {
	orig := encodePNG(t, testImage(t))

	text := pngChunk("tEXt", []byte("Comment\x00captured at a private location"))
	exif := pngChunk("eXIf", []byte("MM\x00\x2aGPS-secret"))
	itxt := pngChunk("iTXt", []byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00<x:xmpmeta/>"))
	withMeta := spliceAfterPNGHeader(orig, text, exif, itxt)

	if _, err := png.Decode(bytes.NewReader(withMeta)); err != nil {
		t.Fatalf("test setup: spliced PNG does not decode: %v", err)
	}

	out, err := stripPNG(withMeta)
	if err != nil {
		t.Fatalf("stripPNG: %v", err)
	}
	if !bytes.Equal(out, orig) {
		t.Errorf("stripped PNG (%d bytes) is not byte-identical to the metadata-free original (%d bytes)", len(out), len(orig))
	}
	for _, marker := range []string{"tEXt", "eXIf", "iTXt", "secret", "xmpmeta"} {
		if bytes.Contains(out, []byte(marker)) {
			t.Errorf("stripped PNG still contains %q", marker)
		}
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("stripped PNG does not decode: %v", err)
	}
}

func TestStripPNGKeepsRenderingChunks(t *testing.T) {
	orig := encodePNG(t, testImage(t))
	gama := pngChunk("gAMA", []byte{0x00, 0x00, 0xb1, 0x8f})
	withGama := spliceAfterPNGHeader(orig, gama)

	out, err := stripPNG(withGama)
	if err != nil {
		t.Fatalf("stripPNG: %v", err)
	}
	if !bytes.Contains(out, []byte("gAMA")) {
		t.Error("gAMA rendering chunk was dropped; it must be kept")
	}
}

func TestStripPNGMalformed(t *testing.T) {
	orig := encodePNG(t, testImage(t))
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "bad signature", data: []byte("not-a-png-at-all!")},
		{name: "truncated chunk", data: orig[:20]},
		{name: "no IEND", data: orig[:len(orig)-12]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := stripPNG(tt.data); err == nil {
				t.Error("expected an error for malformed PNG")
			}
		})
	}
}

func TestStripMetadataDispatch(t *testing.T) {
	jpg := encodeJPEG(t, testImage(t))
	pg := encodePNG(t, testImage(t))

	t.Run("jpeg", func(t *testing.T) {
		out, err := StripMetadata(jpg, "jpeg")
		if err != nil {
			t.Fatalf("StripMetadata jpeg: %v", err)
		}
		if len(out) == 0 {
			t.Error("empty output")
		}
	})
	t.Run("png", func(t *testing.T) {
		out, err := StripMetadata(pg, "png")
		if err != nil {
			t.Fatalf("StripMetadata png: %v", err)
		}
		if len(out) == 0 {
			t.Error("empty output")
		}
	})
	for _, format := range []string{"gif", "webp", "bmp", ""} {
		t.Run("unsupported "+format, func(t *testing.T) {
			if _, err := StripMetadata([]byte("x"), format); !errors.Is(err, ErrStripUnsupported) {
				t.Errorf("StripMetadata(%q) error = %v, want ErrStripUnsupported", format, err)
			}
		})
	}
}

func TestCanStripLossless(t *testing.T) {
	tests := map[string]bool{
		"jpeg": true, "png": true,
		"gif": false, "webp": false, "bmp": false, "": false,
	}
	for format, want := range tests {
		if got := CanStripLossless(format); got != want {
			t.Errorf("CanStripLossless(%q) = %v, want %v", format, got, want)
		}
	}
}
