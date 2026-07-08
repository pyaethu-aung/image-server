package imageproc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// The helpers below assemble minimal ISOBMFF byte structures so the sniffer can
// be exercised deterministically without libvips. A committed real-file path is
// covered separately in detect_test.go (sample.heic/.avif/.tiff).

// box wraps a payload in a 32-bit-sized box of the given four-character type.
func box(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(body)))
	copy(b[4:8], typ)
	return append(b, body...)
}

// box64 wraps a payload in a box using the 64-bit largesize form (size == 1).
func box64(typ string, payload []byte) []byte {
	b := make([]byte, 16, 16+len(payload))
	binary.BigEndian.PutUint32(b[0:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(16+len(payload)))
	return append(b, payload...)
}

// box0 wraps a payload in a box using the extends-to-end form (size == 0). Only
// valid as the last box in its container.
func box0(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	copy(b[4:8], typ)
	return append(b, payload...)
}

// ftyp builds an ftyp box from a major brand and optional compatible brands.
func ftyp(major string, compat ...string) []byte {
	p := append([]byte(major), 0, 0, 0, 0) // major brand + minor version
	for _, c := range compat {
		p = append(p, []byte(c)...)
	}
	return box("ftyp", p)
}

// ispePayload is the body of an ispe box: 4 bytes version/flags, then width and
// height as big-endian uint32s.
func ispePayload(w, h uint32) []byte {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[4:8], w)
	binary.BigEndian.PutUint32(p[8:12], h)
	return p
}

// metaWith wraps property boxes as meta -> iprp -> ipco -> children, with no
// pitm/ipma (so the sniffer falls back to the first ispe).
func metaWith(ipcoChildren ...[]byte) []byte {
	ipco := box("ipco", ipcoChildren...)
	iprp := box("iprp", ipco)
	// meta is a FullBox: 4 bytes of version/flags precede its children.
	return box("meta", append([]byte{0, 0, 0, 0}, iprp...))
}

// pitm builds a version-0 primary item box naming item id.
func pitm(id uint16) []byte {
	return box("pitm", binary.BigEndian.AppendUint16([]byte{0, 0, 0, 0}, id))
}

// ipma builds a version-0, narrow-index item property association box mapping
// one item id to a list of 1-based property indices.
func ipma(itemID uint16, propIdxs ...byte) []byte {
	p := binary.BigEndian.AppendUint32([]byte{0, 0, 0, 0}, 1) // version/flags, one entry
	p = binary.BigEndian.AppendUint16(p, itemID)
	p = append(p, byte(len(propIdxs)))
	for _, idx := range propIdxs {
		p = append(p, idx&0x7f) // essential bit clear
	}
	return box("ipma", p)
}

// metaPrimary builds a meta box with a pitm and an ipma, mirroring a real HEIF
// file where the primary item's associated ispe is authoritative.
func metaPrimary(primaryID uint16, ipmaBox []byte, ipcoChildren ...[]byte) []byte {
	ipco := box("ipco", ipcoChildren...)
	iprp := box("iprp", ipco, ipmaBox)
	body := append([]byte{0, 0, 0, 0}, pitm(primaryID)...)
	return box("meta", append(body, iprp...))
}

func TestDetectHEIF(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		wantFormat string
		wantW      int
		wantH      int
		wantOK     bool
	}{
		{
			name:       "heic with ispe",
			data:       append(ftyp("heic", "mif1"), metaWith(box("ispe", ispePayload(640, 480)))...),
			wantFormat: "heic", wantW: 640, wantH: 480, wantOK: true,
		},
		{
			name:       "avif brand",
			data:       append(ftyp("avif"), metaWith(box("ispe", ispePayload(100, 50)))...),
			wantFormat: "avif", wantW: 100, wantH: 50, wantOK: true,
		},
		{
			name:       "generic mif1 reports heif",
			data:       append(ftyp("mif1"), metaWith(box("ispe", ispePayload(8, 8)))...),
			wantFormat: "heif", wantW: 8, wantH: 8, wantOK: true,
		},
		{
			name:       "avif wins over heic when both present",
			data:       append(ftyp("mif1", "heic", "avif"), metaWith(box("ispe", ispePayload(3, 3)))...),
			wantFormat: "avif", wantW: 3, wantH: 3, wantOK: true,
		},
		{
			name:       "no ipma falls back to first ispe",
			data:       append(ftyp("heic"), metaWith(box("ispe", ispePayload(16, 16)), box("ispe", ispePayload(64, 64)), box("ispe", ispePayload(8, 8)))...),
			wantFormat: "heic", wantW: 16, wantH: 16, wantOK: true,
		},
		{
			// The primary item (id 2) is associated with property #2 (32x24),
			// even though a larger coded-tile ispe (64x64, property #1) exists,
			// exactly as libvips writes a real .heic.
			name:       "primary item selects its ispe over a larger tile",
			data:       append(ftyp("heic"), metaPrimary(2, ipma(2, 2), box("ispe", ispePayload(64, 64)), box("ispe", ispePayload(32, 24)))...),
			wantFormat: "heic", wantW: 32, wantH: 24, wantOK: true,
		},
		{
			name:       "64-bit sized ispe box",
			data:       append(ftyp("heic"), box("meta", append([]byte{0, 0, 0, 0}, box("iprp", box("ipco", box64("ispe", ispePayload(20, 30))))...))...),
			wantFormat: "heic", wantW: 20, wantH: 30, wantOK: true,
		},
		{
			name:       "size-zero ispe box extends to end",
			data:       append(ftyp("heic"), box("meta", append([]byte{0, 0, 0, 0}, box("iprp", box("ipco", box0("ispe", ispePayload(12, 9))))...))...),
			wantFormat: "heic", wantW: 12, wantH: 9, wantOK: true,
		},
		{
			name:       "heic brand but no ispe",
			data:       append(ftyp("heic"), box("meta", []byte{0, 0, 0, 0})...),
			wantFormat: "heic", wantW: 0, wantH: 0, wantOK: true,
		},
		{
			name:       "heic brand, no meta box at all",
			data:       ftyp("heic"),
			wantFormat: "heic", wantW: 0, wantH: 0, wantOK: true,
		},
		{
			name:   "non-image isobmff brand (mp4)",
			data:   append(ftyp("isom", "mp41"), metaWith(box("ispe", ispePayload(9, 9)))...),
			wantOK: false,
		},
		{
			name:   "first box is not ftyp",
			data:   box("moov", []byte{0, 0, 0, 0}),
			wantOK: false,
		},
		{
			name:   "too short to hold a box header",
			data:   []byte{0, 0},
			wantOK: false,
		},
		{
			name:   "ftyp size larger than data",
			data:   []byte{0, 0, 0, 0xff, 'f', 't', 'y', 'p', 'h', 'e', 'i', 'c'},
			wantOK: false,
		},
		{
			name:       "ispe with zero dimensions is ignored",
			data:       append(ftyp("heic"), metaWith(box("ispe", ispePayload(0, 0)))...),
			wantFormat: "heic", wantW: 0, wantH: 0, wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			format, w, h, ok := detectHEIF(tt.data)
			if ok != tt.wantOK {
				t.Fatalf("detectHEIF() ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if format != tt.wantFormat || w != tt.wantW || h != tt.wantH {
				t.Errorf("detectHEIF() = (%q, %d, %d), want (%q, %d, %d)",
					format, w, h, tt.wantFormat, tt.wantW, tt.wantH)
			}
		})
	}
}

// TestDetectImageHEIFNoDimensions covers the DetectImage branch where a HEIF
// container is recognised but its dimensions cannot be read: it must be an
// ErrUnsupported rather than a zero-dimension Info.
func TestDetectImageHEIFNoDimensions(t *testing.T) {
	data := ftyp("heic") // recognised brand, no ispe
	if _, err := DetectImage(data); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("DetectImage() error = %v, want ErrUnsupported", err)
	}
}

func TestPrimaryItemID(t *testing.T) {
	tests := []struct {
		name   string
		meta   []byte
		wantID uint32
		wantOK bool
	}{
		{"version 0 (16-bit id)", pitm(7), 7, true},
		{"version 1 (32-bit id)", box("pitm", binary.BigEndian.AppendUint32([]byte{1, 0, 0, 0}, 70000)), 70000, true},
		{"no pitm box", box("iinf", []byte{0, 0, 0, 0}), 0, false},
		{"version 0 truncated body", box("pitm", []byte{0, 0, 0, 0, 0}), 0, false},
		{"version 1 truncated body", box("pitm", []byte{1, 0, 0, 0, 0, 0}), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := primaryItemID(tt.meta)
			if id != tt.wantID || ok != tt.wantOK {
				t.Errorf("primaryItemID() = (%d, %v), want (%d, %v)", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

// ipmaV1Wide builds a version-1 (32-bit item id), wide-index (2-byte property
// index) ipma box for one item.
func ipmaV1Wide(itemID uint32, idxs ...int) []byte {
	p := []byte{1, 0, 0, 1} // version 1, flags 1 (wide index)
	p = binary.BigEndian.AppendUint32(p, 1)
	p = binary.BigEndian.AppendUint32(p, itemID)
	p = append(p, byte(len(idxs)))
	for _, i := range idxs {
		p = binary.BigEndian.AppendUint16(p, uint16(i)&0x7fff)
	}
	return box("ipma", p)
}

func TestPropertyIndices(t *testing.T) {
	// propertyIndices receives the iprp payload (its child boxes), as primaryISPE
	// hands it via firstBox, so the inputs below are ipco + ipma concatenated,
	// not a wrapping iprp box.
	tests := []struct {
		name   string
		iprp   []byte
		itemID uint32
		want   []int
	}{
		{"version 0 narrow index", append(box("ipco"), ipma(1, 1, 2)...), 1, []int{1, 2}},
		{"version 1 wide index", append(box("ipco"), ipmaV1Wide(70000, 5, 300)...), 70000, []int{5, 300}},
		{"item not present", append(box("ipco"), ipma(1, 1)...), 9, nil},
		{"no ipma box", box("ipco"), 1, nil},
		{"truncated ipma", append(box("ipco"), box("ipma", []byte{0, 0, 0, 0, 0, 0})...), 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := propertyIndices(tt.iprp, tt.itemID); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("propertyIndices() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestForEachBoxMalformed(t *testing.T) {
	// A box declaring the 64-bit size form (size==1) but truncated before the
	// 8-byte largesize ends the walk with no boxes visited.
	var count int
	forEachBox([]byte{0, 0, 0, 1, 'i', 's', 'p', 'e', 0, 0}, func(string, []byte) { count++ })
	if count != 0 {
		t.Errorf("truncated largesize: visited %d boxes, want 0", count)
	}

	// A 64-bit largesize larger than the buffer ends the walk.
	big := []byte{0, 0, 0, 1, 'i', 's', 'p', 'e'}
	big = binary.BigEndian.AppendUint64(big, 1<<40)
	count = 0
	forEachBox(big, func(string, []byte) { count++ })
	if count != 0 {
		t.Errorf("oversize largesize: visited %d boxes, want 0", count)
	}
}
