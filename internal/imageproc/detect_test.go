package imageproc

import (
	"bytes"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// readFixture loads a committed testdata image (a real heic/avif/tiff produced
// by libvips, which the stdlib decoders cannot generate).
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// encode renders a w x h image with the given stdlib encoder.
func encode(t *testing.T, w, h int, enc func(*bytes.Buffer, image.Image) error) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if err := enc(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// webp1x1 is a minimal valid 1x1 lossless WebP (VP8L) file.
var webp1x1 = []byte{
	'R', 'I', 'F', 'F', 0x1a, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P',
	'V', 'P', '8', 'L', 0x0d, 0x00, 0x00, 0x00,
	0x2f, 0x00, 0x00, 0x00, 0x10, 0x07, 0x10, 0x11, 0x11, 0x88, 0x88, 0xfe, 0x07, 0x00,
}

func TestDetectImage(t *testing.T) {
	jpegBytes := encode(t, 3, 2, func(b *bytes.Buffer, img image.Image) error {
		return jpeg.Encode(b, img, nil)
	})
	pngBytes := encode(t, 4, 5, func(b *bytes.Buffer, img image.Image) error {
		return png.Encode(b, img)
	})
	gifBytes := encode(t, 2, 2, func(b *bytes.Buffer, img image.Image) error {
		return gif.Encode(b, img, nil)
	})

	tests := []struct {
		name    string
		data    []byte
		want    Info
		wantErr error
	}{
		{
			name: "jpeg",
			data: jpegBytes,
			want: Info{Format: "jpeg", MimeType: "image/jpeg", Width: 3, Height: 2},
		},
		{
			name: "png",
			data: pngBytes,
			want: Info{Format: "png", MimeType: "image/png", Width: 4, Height: 5},
		},
		{
			name: "gif",
			data: gifBytes,
			want: Info{Format: "gif", MimeType: "image/gif", Width: 2, Height: 2},
		},
		{
			name: "webp",
			data: webp1x1,
			want: Info{Format: "webp", MimeType: "image/webp", Width: 1, Height: 1},
		},
		{
			name: "heic",
			data: readFixture(t, "sample.heic"),
			want: Info{Format: "heic", MimeType: "image/heic", Width: 128, Height: 64},
		},
		{
			name: "avif",
			data: readFixture(t, "sample.avif"),
			want: Info{Format: "avif", MimeType: "image/avif", Width: 32, Height: 24},
		},
		{
			name: "tiff",
			data: readFixture(t, "sample.tiff"),
			want: Info{Format: "tiff", MimeType: "image/tiff", Width: 32, Height: 24},
		},
		{
			name:    "truncated jpeg header",
			data:    jpegBytes[:4],
			wantErr: ErrUnsupported,
		},
		{
			name:    "garbage",
			data:    []byte("definitely not an image"),
			wantErr: ErrUnsupported,
		},
		{
			name:    "empty",
			data:    nil,
			wantErr: ErrUnsupported,
		},
		{
			name: "crafted gif header with zero dimensions",
			// Valid GIF magic + logical screen descriptor claiming 0x0.
			data:    []byte("GIF89a\x00\x00\x00\x00\x00\x00\x00"),
			wantErr: ErrUnsupported,
		},
		{
			name: "extension is ignored, magic bytes win",
			// PNG bytes would arrive with a .jpg filename; only bytes matter.
			data: pngBytes,
			want: Info{Format: "png", MimeType: "image/png", Width: 4, Height: 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectImage(tt.data)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("DetectImage() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DetectImage() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("DetectImage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCheckPixelLimit(t *testing.T) {
	tests := []struct {
		name      string
		width     int
		height    int
		maxPixels int64
		wantErr   error
	}{
		{name: "well under limit", width: 100, height: 100, maxPixels: 1_000_000},
		{name: "exactly at limit", width: 100, height: 100, maxPixels: 10_000},
		{name: "one pixel over", width: 100, height: 101, maxPixels: 10_000, wantErr: ErrTooManyPixels},
		{name: "bomb dimensions", width: 100_000, height: 100_000, maxPixels: 50_000_000, wantErr: ErrTooManyPixels},
		{name: "1x1 minimum", width: 1, height: 1, maxPixels: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPixelLimit(tt.width, tt.height, tt.maxPixels)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("CheckPixelLimit(%d, %d, %d) = %v, want %v",
					tt.width, tt.height, tt.maxPixels, err, tt.wantErr)
			}
		})
	}
}
