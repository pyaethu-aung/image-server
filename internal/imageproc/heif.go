package imageproc

// HEIF-family (ISO/IEC 14496-12 ISOBMFF) detection: HEIC, HEIF, and AVIF. Like
// the rest of detection this is header-only. It reads the `ftyp` brand and the
// image spatial extents (`ispe`) box, so the decompression-bomb guard runs on
// real dimensions before libvips is ever handed the file. It is pure Go, with
// no cgo and no third-party dependency: the container is a simple tree of
// length-prefixed boxes and only the handful we need are walked.

import "encoding/binary"

// ftyp brands grouped by the format DetectImage reports. AVIF (AV1-coded) is
// distinguished from the HEVC-coded HEIC variants; the generic mif1/msf1 brands
// (shared by both) report as the neutral "heif". Classification prefers avif,
// then heic, then heif when several brands are present.
var (
	brandsAVIF = map[string]bool{"avif": true, "avis": true, "avio": true}
	brandsHEIC = map[string]bool{"heic": true, "heix": true, "heim": true, "heis": true, "hevc": true, "hevx": true}
	brandsHEIF = map[string]bool{"mif1": true, "msf1": true}
)

// detectHEIF reports whether data is an ISOBMFF HEIF-family image and, if so,
// its format ("heic"/"heif"/"avif") and the pixel dimensions from its largest
// ispe box. ok is false for any input that is not a recognised HEIF-family
// container (including ISOBMFF video brands such as mp4/qt), so DetectImage
// falls back to the stdlib decoders. When ok is true but dimensions could not
// be read, width and height are 0 and the caller rejects the file.
func detectHEIF(data []byte) (format string, width, height int, ok bool) {
	// A valid ISOBMFF file opens with an ftyp box; if the first box is not
	// ftyp this is not a file we recognise.
	if len(data) < 8 {
		return "", 0, 0, false
	}
	size := int(binary.BigEndian.Uint32(data[0:4]))
	if string(data[4:8]) != "ftyp" || size < 8 || size > len(data) {
		return "", 0, 0, false
	}
	format = classifyBrands(data[8:size])
	if format == "" {
		return "", 0, 0, false
	}
	width, height = primaryISPE(data)
	return format, width, height, true
}

// classifyBrands inspects an ftyp payload (major brand, minor version, then a
// list of compatible brands) and returns the reported format, or "" when no
// brand is a HEIF-family image brand.
func classifyBrands(payload []byte) string {
	var hasAVIF, hasHEIC, hasHEIF bool
	check := func(b string) {
		switch {
		case brandsAVIF[b]:
			hasAVIF = true
		case brandsHEIC[b]:
			hasHEIC = true
		case brandsHEIF[b]:
			hasHEIF = true
		}
	}
	if len(payload) >= 4 {
		check(string(payload[0:4])) // major brand
	}
	// Compatible brands follow the 4-byte major brand and 4-byte minor version.
	for i := 8; i+4 <= len(payload); i += 4 {
		check(string(payload[i : i+4]))
	}
	switch {
	case hasAVIF:
		return "avif"
	case hasHEIC:
		return "heic"
	case hasHEIF:
		return "heif"
	default:
		return ""
	}
}

// dims is the width and height carried by an ispe property (0, 0 for a
// property that is not an ispe).
type dims struct{ w, h int }

// primaryISPE returns the pixel dimensions of the primary image. It reads the
// ispe (image spatial extents) property associated with the primary item via
// meta -> pitm (primary item id) and meta -> iprp -> ipma (item-to-property
// associations), indexing into the ordered property list in iprp -> ipco. This
// matters because a HEIF file commonly carries several ispe boxes (a padded
// coded tile, thumbnails, auxiliary images); the largest is not the display
// size, so only the primary item's association is authoritative. When the
// primary item cannot be resolved it falls back to the first ispe in property
// order. Returns 0, 0 when no usable ispe is found.
func primaryISPE(data []byte) (int, int) {
	meta, ok := firstBox(data, "meta")
	if !ok || len(meta) < 4 {
		return 0, 0
	}
	// meta is a FullBox: skip its 1-byte version and 3-byte flags.
	meta = meta[4:]
	iprp, ok := firstBox(meta, "iprp")
	if !ok {
		return 0, 0
	}
	ipco, ok := firstBox(iprp, "ipco")
	if !ok {
		return 0, 0
	}

	// The ipco children are the property list, indexed from 1 in association
	// order.
	var props []dims
	forEachBox(ipco, func(typ string, payload []byte) {
		var d dims
		// ispe is a FullBox: version(1) + flags(3), then width(4), height(4).
		if typ == "ispe" && len(payload) >= 12 {
			d.w = int(binary.BigEndian.Uint32(payload[4:8]))
			d.h = int(binary.BigEndian.Uint32(payload[8:12]))
		}
		props = append(props, d)
	})

	// Preferred: the ispe associated with the primary item.
	if id, ok := primaryItemID(meta); ok {
		for _, idx := range propertyIndices(iprp, id) {
			if idx >= 1 && idx <= len(props) {
				if d := props[idx-1]; d.w > 0 && d.h > 0 {
					return d.w, d.h
				}
			}
		}
	}
	// Fallback: the first ispe in property order.
	for _, d := range props {
		if d.w > 0 && d.h > 0 {
			return d.w, d.h
		}
	}
	return 0, 0
}

// primaryItemID reads the primary item id from the pitm box in a meta payload
// (with the meta FullBox header already stripped).
func primaryItemID(meta []byte) (uint32, bool) {
	pitm, ok := firstBox(meta, "pitm")
	if !ok || len(pitm) < 4 {
		return 0, false
	}
	body := pitm[4:] // skip version + flags
	if pitm[0] == 0 {
		if len(body) < 2 {
			return 0, false
		}
		return uint32(binary.BigEndian.Uint16(body[0:2])), true
	}
	if len(body) < 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(body[0:4]), true
}

// propertyIndices returns the 1-based property indices associated with itemID,
// parsed from the ipma box inside iprp. It returns nil when ipma is absent or
// malformed, or when itemID has no entry.
func propertyIndices(iprp []byte, itemID uint32) []int {
	ipma, ok := firstBox(iprp, "ipma")
	if !ok || len(ipma) < 8 {
		return nil
	}
	version := ipma[0]
	flags := uint32(ipma[1])<<16 | uint32(ipma[2])<<8 | uint32(ipma[3])
	wideIndex := flags&1 == 1
	p := ipma[4:]
	entryCount := binary.BigEndian.Uint32(p[0:4])
	p = p[4:]
	for e := uint32(0); e < entryCount; e++ {
		var id uint32
		if version >= 1 {
			if len(p) < 4 {
				return nil
			}
			id = binary.BigEndian.Uint32(p[0:4])
			p = p[4:]
		} else {
			if len(p) < 2 {
				return nil
			}
			id = uint32(binary.BigEndian.Uint16(p[0:2]))
			p = p[2:]
		}
		if len(p) < 1 {
			return nil
		}
		assoc := int(p[0])
		p = p[1:]
		indices := make([]int, 0, assoc)
		for a := 0; a < assoc; a++ {
			if wideIndex {
				if len(p) < 2 {
					return nil
				}
				indices = append(indices, int(uint16(p[0]&0x7f)<<8|uint16(p[1])))
				p = p[2:]
			} else {
				if len(p) < 1 {
					return nil
				}
				indices = append(indices, int(p[0]&0x7f))
				p = p[1:]
			}
		}
		if id == itemID {
			return indices
		}
	}
	return nil
}

// firstBox returns the payload (bytes after the box header) of the first
// top-level box of type want in b, and whether one was found.
func firstBox(b []byte, want string) ([]byte, bool) {
	var found []byte
	var ok bool
	forEachBox(b, func(typ string, payload []byte) {
		if typ == want && !ok {
			found, ok = payload, true
		}
	})
	return found, ok
}

// forEachBox walks the top-level boxes in b, invoking fn(type, payload) for
// each. A box with a size that is malformed (truncated, or larger than what
// remains) ends the walk silently: detection treats a container it cannot fully
// parse as simply not yielding the box it was looking for, never as an error.
func forEachBox(b []byte, fn func(typ string, payload []byte)) {
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b[0:4]))
		typ := string(b[4:8])
		header := 8
		switch size {
		case 1:
			// 64-bit largesize follows the type.
			if len(b) < 16 {
				return
			}
			size64 := binary.BigEndian.Uint64(b[8:16])
			if size64 > uint64(len(b)) {
				return
			}
			size = int(size64) //nolint:gosec // G115: bounded by the size64 > len(b) check above
			header = 16
		case 0:
			// Box extends to the end of the enclosing bytes.
			size = len(b)
		}
		if size < header || size > len(b) {
			return
		}
		fn(typ, b[header:size])
		b = b[size:]
	}
}
