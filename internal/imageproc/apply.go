package imageproc

import (
	"fmt"
	"math"

	"github.com/h2non/bimg"
)

// bimgType maps a requested Format to the bimg image type. An unset Format
// maps to bimg.UNKNOWN, which tells Process to keep the source encoding.
func bimgType(f Format) bimg.ImageType {
	switch f {
	case FormatJPEG:
		return bimg.JPEG
	case FormatPNG:
		return bimg.PNG
	case FormatWebP:
		return bimg.WEBP
	default:
		return bimg.UNKNOWN
	}
}

// Apply renders src according to t and returns the encoded output bytes and
// their MIME type. This is the only file in the package that touches libvips.
//
// The decompression-bomb guard is re-run on the source header dimensions
// before any pixels are handed to libvips, so an oversize original can never
// be decoded here even if it somehow bypassed upload validation. src must be
// a previously validated image; a decode or transform failure is returned as
// an error for the handler to map to a 500 (the source came from our own
// storage, so a failure here is server-side, not client input).
func Apply(src []byte, t Transform, maxPixels int64) ([]byte, string, error) {
	info, err := DetectImage(src)
	if err != nil {
		return nil, "", err
	}
	if err := CheckPixelLimit(info.Width, info.Height, maxPixels); err != nil {
		return nil, "", err
	}

	w, h := t.Width, t.Height
	// When both dimensions are given and the mode is not cover, fit the image
	// inside the box preserving aspect (contain). bimg's default with both
	// dimensions set would fill the box exactly, so the contained size is
	// computed here and passed as the exact target. cover keeps the full box
	// and lets bimg crop; a single-dimension request resizes proportionally.
	if w > 0 && h > 0 && t.Fit != FitCover {
		w, h = containDims(info.Width, info.Height, w, h)
	}

	opts := bimg.Options{
		Width:         w,
		Height:        h,
		Quality:       t.Quality, // 0 lets bimg pick its default
		Type:          bimgType(t.Format),
		StripMetadata: t.Strip, // drop EXIF/XMP/ICC on this re-encode when asked
	}
	if t.Fit == FitCover {
		opts.Crop = true
		opts.Gravity = bimg.GravityCentre
	}

	out, err := bimg.NewImage(src).Process(opts)
	if err != nil {
		return nil, "", fmt.Errorf("imageproc: transform failed: %w", err)
	}

	contentType := info.MimeType
	if t.Format != "" {
		contentType = t.Format.ContentType()
	}
	return out, contentType, nil
}

// containDims returns the largest width and height that fit a srcW x srcH
// image inside a boxW x boxH box while preserving aspect ratio.
func containDims(srcW, srcH, boxW, boxH int) (int, int) {
	// Scale by the smaller of the two ratios so both dimensions fit.
	if boxW*srcH <= boxH*srcW {
		// Width is the binding constraint.
		h := int(math.Round(float64(srcH) * float64(boxW) / float64(srcW)))
		return boxW, max(h, 1)
	}
	// Height is the binding constraint.
	w := int(math.Round(float64(srcW) * float64(boxH) / float64(srcH)))
	return max(w, 1), boxH
}
