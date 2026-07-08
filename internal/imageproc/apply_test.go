package imageproc

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// noisyPNG builds a w x h PNG whose pixels vary, so JPEG quality actually
// affects the encoded size (a flat image compresses to near nothing).
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8((x*7 + y*13) % 256),
				G: uint8((x*29 + y*3) % 256),
				B: uint8((x*17 + y*23) % 256),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png fixture: %v", err)
	}
	return buf.Bytes()
}

// jpegBytes builds a w x h JPEG fixture.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatalf("encode jpeg fixture: %v", err)
	}
	return buf.Bytes()
}

func TestApplyResize(t *testing.T) {
	src := noisyPNG(t, 100, 50)

	tests := []struct {
		name       string
		t          Transform
		wantW      int
		wantH      int
		wantFormat string
	}{
		{name: "width only, aspect preserved", t: Transform{Width: 50}, wantW: 50, wantH: 25, wantFormat: "png"},
		{name: "height only, aspect preserved", t: Transform{Height: 10}, wantW: 20, wantH: 10, wantFormat: "png"},
		{name: "contain fits within box", t: Transform{Width: 40, Height: 40, Fit: FitContain}, wantW: 40, wantH: 20, wantFormat: "png"},
		{name: "cover fills box exactly", t: Transform{Width: 40, Height: 40, Fit: FitCover}, wantW: 40, wantH: 40, wantFormat: "png"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, ct, err := Apply(src, tt.t, 1_000_000)
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			info, err := DetectImage(out)
			if err != nil {
				t.Fatalf("DetectImage(output): %v", err)
			}
			if info.Width != tt.wantW || info.Height != tt.wantH {
				t.Errorf("output dims = %dx%d, want %dx%d", info.Width, info.Height, tt.wantW, tt.wantH)
			}
			if info.Format != tt.wantFormat {
				t.Errorf("output format = %q, want %q", info.Format, tt.wantFormat)
			}
			if want := "image/" + tt.wantFormat; ct != want {
				t.Errorf("content type = %q, want %q", ct, want)
			}
		})
	}
}

func TestApplyFormatConversion(t *testing.T) {
	src := noisyPNG(t, 20, 20)

	tests := []struct {
		format     Format
		wantFormat string
	}{
		{FormatJPEG, "jpeg"},
		{FormatPNG, "png"},
		{FormatWebP, "webp"},
	}
	for _, tt := range tests {
		t.Run(string(tt.format), func(t *testing.T) {
			out, ct, err := Apply(src, Transform{Format: tt.format}, 1_000_000)
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			info, err := DetectImage(out)
			if err != nil {
				t.Fatalf("DetectImage(output): %v", err)
			}
			if info.Format != tt.wantFormat {
				t.Errorf("output format = %q, want %q", info.Format, tt.wantFormat)
			}
			if ct != tt.format.ContentType() {
				t.Errorf("content type = %q, want %q", ct, tt.format.ContentType())
			}
		})
	}
}

func TestApplyKeepsSourceFormat(t *testing.T) {
	src := jpegBytes(t, 30, 30)
	out, ct, err := Apply(src, Transform{Width: 15}, 1_000_000)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if ct != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg (source format kept)", ct)
	}
	if info, _ := DetectImage(out); info.Format != "jpeg" {
		t.Errorf("output format = %q, want jpeg", info.Format)
	}
}

func TestApplyQualityAffectsSize(t *testing.T) {
	src := noisyPNG(t, 64, 64)

	low, _, err := Apply(src, Transform{Format: FormatJPEG, Quality: 10}, 1_000_000)
	if err != nil {
		t.Fatalf("Apply(q=10): %v", err)
	}
	high, _, err := Apply(src, Transform{Format: FormatJPEG, Quality: 95}, 1_000_000)
	if err != nil {
		t.Fatalf("Apply(q=95): %v", err)
	}
	if len(low) >= len(high) {
		t.Errorf("q=10 size %d not smaller than q=95 size %d", len(low), len(high))
	}
}

// TestApplyStripsMetadata covers the combined regime: a resize that also
// strips. The EXIF payload spliced into the source must not survive the
// libvips re-encode when Strip is set.
func TestApplyStripsMetadata(t *testing.T) {
	base := jpegBytes(t, 20, 20)
	withExif := spliceAfterSOI(base, jpegSegment(0xE1, append([]byte("Exif\x00\x00"), []byte("secret-gps-payload")...)))
	if !bytes.Contains(withExif, []byte("secret-gps-payload")) {
		t.Fatal("test setup: EXIF payload not spliced into source")
	}

	out, _, err := Apply(withExif, Transform{Width: 10, Strip: true}, 1_000_000)
	if err != nil {
		t.Fatalf("Apply(strip): %v", err)
	}
	if bytes.Contains(out, []byte("secret-gps-payload")) {
		t.Error("EXIF payload survived the strip re-encode")
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("stripped output does not decode: %v", err)
	}
}

// TestApplyDecodesNewInputFormats exercises the real libvips decode path for
// the newly accepted input formats: each is resized and re-encoded to a
// web-safe jpeg (the server never encodes back to heic/heif/avif/tiff).
func TestApplyDecodesNewInputFormats(t *testing.T) {
	tests := []struct {
		fixture string
		wantW   int // resized to width 16, aspect preserved
		wantH   int
	}{
		{"sample.heic", 16, 8}, // 128x64 -> 16x8
		{"sample.avif", 16, 12},
		{"sample.tiff", 16, 12},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			src := readFixture(t, tt.fixture)
			out, ct, err := Apply(src, Transform{Width: 16, Format: FormatJPEG}, 1_000_000)
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if ct != "image/jpeg" {
				t.Errorf("content type = %q, want image/jpeg", ct)
			}
			info, err := DetectImage(out)
			if err != nil {
				t.Fatalf("DetectImage(output): %v", err)
			}
			if info.Format != "jpeg" {
				t.Errorf("output format = %q, want jpeg", info.Format)
			}
			if info.Width != tt.wantW || info.Height != tt.wantH {
				t.Errorf("output dims = %dx%d, want %dx%d", info.Width, info.Height, tt.wantW, tt.wantH)
			}
		})
	}
}

func TestApplyBombGuard(t *testing.T) {
	src := noisyPNG(t, 100, 50) // 5000 pixels
	_, _, err := Apply(src, Transform{Width: 10}, 4_999)
	if !errors.Is(err, ErrTooManyPixels) {
		t.Errorf("Apply() error = %v, want ErrTooManyPixels", err)
	}
}

func TestApplyRejectsNonImage(t *testing.T) {
	_, _, err := Apply([]byte("definitely not an image"), Transform{Width: 10}, 1_000_000)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Apply() error = %v, want ErrUnsupported", err)
	}
}
