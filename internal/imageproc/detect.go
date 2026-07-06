// Package imageproc validates and transforms images. Detection is
// header-only: the format comes from magic bytes and the dimensions from the
// image header, so no pixel data is ever decoded here. Transforms (libvips)
// land in a later step.
package imageproc

import (
	"bytes"
	"errors"
	"fmt"
	"image"

	// Decoders registered for image.DecodeConfig format sniffing.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// Sentinel errors returned by detection and validation.
var (
	// ErrUnsupported means the bytes are not a supported image format
	// (jpeg, png, gif, webp), as detected from magic bytes.
	ErrUnsupported = errors.New("imageproc: unsupported image format")
	// ErrTooManyPixels means the decoded pixel count (width x height) would
	// exceed the configured cap (decompression-bomb guard).
	ErrTooManyPixels = errors.New("imageproc: image exceeds pixel limit")
)

// Info describes a detected image.
type Info struct {
	Format   string // "jpeg", "png", "gif", "webp"
	MimeType string // "image/jpeg", ...
	Width    int
	Height   int
}

// DetectImage sniffs the real image type from data's magic bytes and reads
// the dimensions from the header without decoding pixels. File extensions
// and client-supplied Content-Type headers are deliberately ignored.
func DetectImage(data []byte) (Info, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Info{}, fmt.Errorf("%w: invalid dimensions %dx%d", ErrUnsupported, cfg.Width, cfg.Height)
	}
	return Info{
		Format:   format,
		MimeType: "image/" + format,
		Width:    cfg.Width,
		Height:   cfg.Height,
	}, nil
}

// CheckPixelLimit rejects images whose decoded pixel count would exceed
// maxPixels. Run it on header dimensions before any pixel decoding.
func CheckPixelLimit(width, height int, maxPixels int64) error {
	if int64(width)*int64(height) > maxPixels {
		return fmt.Errorf("%w: %dx%d exceeds %d pixels", ErrTooManyPixels, width, height, maxPixels)
	}
	return nil
}
