package imageproc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrStripUnsupported means a lossless, in-place metadata strip is not
// implemented for the image's format. Callers can still strip such a format
// by re-encoding it (request a format change), but never losslessly here.
var ErrStripUnsupported = errors.New("imageproc: lossless metadata strip unsupported for this format")

// CanStripLossless reports whether StripMetadata can losslessly strip the
// given detection format. Callers use it to reject a strip-only request early
// with a 415 rather than after reading the original from storage.
func CanStripLossless(format string) bool {
	return format == "jpeg" || format == "png"
}

// StripMetadata removes metadata (EXIF, XMP, IPTC, comments) from an encoded
// image without decoding or re-encoding pixels, so the visual content is
// preserved byte-for-byte. It is implemented for JPEG and PNG; any other
// format returns ErrStripUnsupported. format is an imageproc detection format
// string ("jpeg", "png", "gif", "webp"). A structurally invalid image returns
// an error, since the bytes come from our own storage and should be well-formed.
func StripMetadata(data []byte, format string) ([]byte, error) {
	switch format {
	case "jpeg":
		return stripJPEG(data)
	case "png":
		return stripPNG(data)
	default:
		return nil, ErrStripUnsupported
	}
}

// jpegDropMarkers are the second bytes of the JPEG marker segments removed by a
// strip: APP1 (EXIF and XMP), APP13 (IPTC/Photoshop), and COM (comments).
// APP0 (JFIF) and APP2 (ICC colour profile) are kept so decoding and colour
// rendering are unaffected.
var jpegDropMarkers = map[byte]bool{
	0xE1: true, // APP1: EXIF + XMP
	0xED: true, // APP13: IPTC / Photoshop
	0xFE: true, // COM: comment
}

// stripJPEG walks the JPEG marker structure, copying every segment except the
// metadata ones. Once the Start-of-Scan marker is reached the remainder of the
// file (the entropy-coded pixel data and any trailing markers) is copied
// verbatim, guaranteeing the compressed image data is untouched.
func stripJPEG(data []byte) ([]byte, error) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, fmt.Errorf("imageproc: not a jpeg (bad SOI)")
	}
	out := make([]byte, 0, len(data))
	out = append(out, 0xFF, 0xD8) // SOI

	i := 2
	for i < len(data) {
		if data[i] != 0xFF {
			return nil, fmt.Errorf("imageproc: malformed jpeg: expected marker at offset %d", i)
		}
		// Skip any 0xFF fill bytes preceding the marker code.
		j := i
		for j < len(data) && data[j] == 0xFF {
			j++
		}
		if j >= len(data) {
			return nil, fmt.Errorf("imageproc: malformed jpeg: truncated marker")
		}
		marker := data[j]

		switch {
		case marker == 0xD9: // EOI
			out = append(out, 0xFF, 0xD9)
			return out, nil
		case marker == 0xDA: // SOS: copy the rest of the file verbatim.
			out = append(out, data[i:]...)
			return out, nil
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7): // TEM / RSTn: standalone, no length
			out = append(out, 0xFF, marker)
			i = j + 1
		default: // a segment carrying a 2-byte big-endian length
			lenPos := j + 1
			if lenPos+2 > len(data) {
				return nil, fmt.Errorf("imageproc: malformed jpeg: truncated segment length")
			}
			segLen := int(data[lenPos])<<8 | int(data[lenPos+1])
			if segLen < 2 {
				return nil, fmt.Errorf("imageproc: malformed jpeg: bad segment length %d", segLen)
			}
			segEnd := lenPos + segLen
			if segEnd > len(data) {
				return nil, fmt.Errorf("imageproc: malformed jpeg: segment overruns data")
			}
			if !jpegDropMarkers[marker] {
				out = append(out, 0xFF, marker)
				out = append(out, data[lenPos:segEnd]...)
			}
			i = segEnd
		}
	}
	return nil, fmt.Errorf("imageproc: malformed jpeg: no scan data found")
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// pngDropChunks are the PNG ancillary chunk types removed by a strip. Colour
// and rendering chunks (gAMA, cHRM, iCCP, sRGB, bKGD, pHYs, tRNS, ...) are kept
// so the decoded image is unchanged; only human/EXIF/text metadata is removed.
var pngDropChunks = map[string]bool{
	"eXIf": true, // EXIF (PNG 1.5+)
	"tEXt": true, // uncompressed text
	"zTXt": true, // compressed text
	"iTXt": true, // international text (often XMP)
	"tIME": true, // last-modification time
}

// stripPNG copies every PNG chunk except the metadata ones. Critical chunks
// (IHDR, PLTE, IDAT, IEND) and all kept ancillary chunks are copied verbatim,
// including their CRCs, so the pixel data is preserved exactly.
func stripPNG(data []byte) ([]byte, error) {
	if len(data) < 8 || !bytes.Equal(data[:8], pngSignature) {
		return nil, fmt.Errorf("imageproc: not a png (bad signature)")
	}
	out := make([]byte, 0, len(data))
	out = append(out, data[:8]...)

	i := 8
	for i+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[i : i+4]))
		if length < 0 {
			return nil, fmt.Errorf("imageproc: malformed png: chunk length overflow")
		}
		ctype := string(data[i+4 : i+8])
		chunkEnd := i + 12 + length // 4 length + 4 type + data + 4 CRC
		if chunkEnd > len(data) || chunkEnd < i {
			return nil, fmt.Errorf("imageproc: malformed png: chunk %q overruns data", ctype)
		}
		if !pngDropChunks[ctype] {
			out = append(out, data[i:chunkEnd]...)
		}
		i = chunkEnd
		if ctype == "IEND" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("imageproc: malformed png: no IEND chunk")
}
