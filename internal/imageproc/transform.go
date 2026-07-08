package imageproc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

// Format is a requested output image format.
type Format string

// Supported output formats (the `fmt` query param).
const (
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
	FormatWebP Format = "webp"
)

// ContentType returns the MIME type for the format (e.g. "image/jpeg").
func (f Format) ContentType() string {
	return "image/" + string(f)
}

// IsOutputFormat reports whether format (a bare type like "jpeg", as stored in
// a MIME type's subtype) is one the server can encode to. Sources in other
// formats (heic, heif, avif, tiff) are accepted for upload and served back
// unchanged, but must carry an explicit fmt to be transformed, since the
// server never re-encodes to those formats (HEIC in particular would need a
// patent-encumbered HEVC encoder).
func IsOutputFormat(format string) bool {
	switch Format(format) {
	case FormatJPEG, FormatPNG, FormatWebP:
		return true
	default:
		return false
	}
}

// Fit is a requested resize mode.
type Fit string

// Supported fit modes (the `fit` query param).
const (
	// FitCover crops to fill the target box exactly.
	FitCover Fit = "cover"
	// FitContain resizes to fit within the target box, preserving aspect.
	FitContain Fit = "contain"
)

// Transform is a normalized, validated set of transform parameters. A zero
// field means "unset": Width/Height/Quality of 0 and empty Format/Fit are all
// defaults to be filled in (or left to the source) at apply time. Because the
// struct captures parameters by field rather than by URL order, two query
// strings that differ only in param order produce an identical Transform.
type Transform struct {
	Width   int    // target width in pixels; 0 = unset
	Height  int    // target height in pixels; 0 = unset
	Format  Format // output format; "" = keep source format
	Quality int    // output quality 1..100 for lossy formats; 0 = unset
	Fit     Fit    // resize mode; "" = unset
	Strip   bool   // strip metadata (EXIF/XMP/IPTC/comments); false = keep
}

// IsIdentity reports whether the transform requests no change at all, in which
// case the original bytes can be served without touching libvips. Strip=true
// is not identity: it must remove metadata, so it never takes the raw path.
func (t Transform) IsIdentity() bool {
	return t == Transform{}
}

// IsStripOnly reports whether the only requested change is metadata removal
// (no resize, format change, or quality change). Such a request can be served
// by a lossless, byte-preserving strip instead of a libvips re-encode.
func (t Transform) IsStripOnly() bool {
	return t.Strip && t == (Transform{Strip: true})
}

// CacheKey returns a deterministic cache key for one image + transform pair:
// hex(sha256(imageID + "|" + canonical)). The canonical form serializes every
// field in a fixed order with stable sentinels for unset values (w=0, fmt=,
// ...), so two URLs that differ only in query-param order hash identically.
// Format is deliberately NOT defaulted to the source type here: the key must
// not depend on per-image metadata.
func CacheKey(imageID uuid.UUID, t Transform) string {
	canonical := fmt.Sprintf("w=%d|h=%d|fmt=%s|q=%d|fit=%s|strip=%t",
		t.Width, t.Height, t.Format, t.Quality, t.Fit, t.Strip)
	sum := sha256.Sum256([]byte(imageID.String() + "|" + canonical))
	return hex.EncodeToString(sum[:])
}

// ParamError describes a single invalid transform query parameter. Handlers
// map it to a 400 response.
type ParamError struct {
	Param string
	Msg   string
}

func (e *ParamError) Error() string {
	return fmt.Sprintf("invalid %q parameter: %s", e.Param, e.Msg)
}

// ParseTransform reads and validates the transform query parameters from q.
// It is pure (no libvips) and is the single source of truth for parameter
// validation: w and h must be >= 1, q must be in 1..100, fmt must be one of
// jpeg/png/webp, and fit must be cover or contain. An absent parameter is
// left unset; a present-but-empty or out-of-range parameter is a *ParamError.
func ParseTransform(q url.Values) (Transform, error) {
	var t Transform
	var err error

	if t.Width, err = parseBoundedInt(q, "w", 1, 0); err != nil {
		return Transform{}, err
	}
	if t.Height, err = parseBoundedInt(q, "h", 1, 0); err != nil {
		return Transform{}, err
	}
	if t.Quality, err = parseBoundedInt(q, "q", 1, 100); err != nil {
		return Transform{}, err
	}
	if t.Format, err = parseFormat(q); err != nil {
		return Transform{}, err
	}
	if t.Fit, err = parseFit(q); err != nil {
		return Transform{}, err
	}
	if t.Strip, err = parseBool(q, "strip"); err != nil {
		return Transform{}, err
	}
	return t, nil
}

// parseBool parses a boolean query param. An absent key returns false (unset);
// only "true" and "false" are accepted, anything else is a *ParamError.
func parseBool(q url.Values, key string) (bool, error) {
	if !q.Has(key) {
		return false, nil
	}
	switch q.Get(key) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &ParamError{Param: key, Msg: "must be true or false"}
	}
}

// parseBoundedInt parses an integer query param constrained to [min, max].
// A max of 0 means unbounded above. An absent key returns 0 (unset); a
// present-but-empty, non-integer, or out-of-range value is a *ParamError.
func parseBoundedInt(q url.Values, key string, min, max int) (int, error) {
	if !q.Has(key) {
		return 0, nil
	}
	n, convErr := strconv.Atoi(q.Get(key))
	if convErr != nil {
		return 0, &ParamError{Param: key, Msg: "must be an integer"}
	}
	if n < min {
		return 0, &ParamError{Param: key, Msg: fmt.Sprintf("must be >= %d", min)}
	}
	if max > 0 && n > max {
		return 0, &ParamError{Param: key, Msg: fmt.Sprintf("must be <= %d", max)}
	}
	return n, nil
}

func parseFormat(q url.Values) (Format, error) {
	if !q.Has("fmt") {
		return "", nil
	}
	switch f := Format(q.Get("fmt")); f {
	case FormatJPEG, FormatPNG, FormatWebP:
		return f, nil
	default:
		return "", &ParamError{Param: "fmt", Msg: "must be one of jpeg, png, webp"}
	}
}

func parseFit(q url.Values) (Fit, error) {
	if !q.Has("fit") {
		return "", nil
	}
	switch f := Fit(q.Get("fit")); f {
	case FitCover, FitContain:
		return f, nil
	default:
		return "", &ParamError{Param: "fit", Msg: "must be one of cover, contain"}
	}
}
