package imageproc

import (
	"errors"
	"net/url"
	"testing"

	"github.com/google/uuid"
)

func TestParseTransform(t *testing.T) {
	tests := []struct {
		name string
		// query is a raw query string, parsed exactly as r.URL.Query() would.
		query string
		want  Transform
		// wantErrParam is the offending param for cases that must fail; empty
		// means the parse must succeed.
		wantErrParam string
	}{
		{name: "empty is identity", query: "", want: Transform{}},
		{name: "width only", query: "w=100", want: Transform{Width: 100}},
		{name: "height only", query: "h=200", want: Transform{Height: 200}},
		{name: "width and height", query: "w=100&h=200", want: Transform{Width: 100, Height: 200}},
		{name: "param order is irrelevant", query: "h=200&w=100", want: Transform{Width: 100, Height: 200}},
		{name: "width minimum 1", query: "w=1", want: Transform{Width: 1}},
		{name: "quality lower bound", query: "q=1", want: Transform{Quality: 1}},
		{name: "quality upper bound", query: "q=100", want: Transform{Quality: 100}},
		{name: "format jpeg", query: "fmt=jpeg", want: Transform{Format: FormatJPEG}},
		{name: "format png", query: "fmt=png", want: Transform{Format: FormatPNG}},
		{name: "format webp", query: "fmt=webp", want: Transform{Format: FormatWebP}},
		{name: "fit cover", query: "fit=cover", want: Transform{Fit: FitCover}},
		{name: "fit contain", query: "fit=contain", want: Transform{Fit: FitContain}},
		{
			name:  "all params",
			query: "w=800&h=600&fmt=webp&q=80&fit=cover",
			want:  Transform{Width: 800, Height: 600, Format: FormatWebP, Quality: 80, Fit: FitCover},
		},

		{name: "width zero", query: "w=0", wantErrParam: "w"},
		{name: "width negative", query: "w=-5", wantErrParam: "w"},
		{name: "width non-integer", query: "w=abc", wantErrParam: "w"},
		{name: "width empty value", query: "w=", wantErrParam: "w"},
		{name: "height zero", query: "h=0", wantErrParam: "h"},
		{name: "height negative", query: "h=-1", wantErrParam: "h"},
		{name: "height non-integer", query: "h=tall", wantErrParam: "h"},
		{name: "height empty value", query: "h=", wantErrParam: "h"},
		{name: "quality zero", query: "q=0", wantErrParam: "q"},
		{name: "quality over max", query: "q=101", wantErrParam: "q"},
		{name: "quality non-integer", query: "q=high", wantErrParam: "q"},
		{name: "quality empty value", query: "q=", wantErrParam: "q"},
		{name: "format unsupported", query: "fmt=gif", wantErrParam: "fmt"},
		{name: "format empty value", query: "fmt=", wantErrParam: "fmt"},
		{name: "fit invalid", query: "fit=squash", wantErrParam: "fit"},
		{name: "fit empty value", query: "fit=", wantErrParam: "fit"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", tt.query, err)
			}
			got, err := ParseTransform(q)

			if tt.wantErrParam != "" {
				var pe *ParamError
				if !errors.As(err, &pe) {
					t.Fatalf("ParseTransform(%q) error = %v, want *ParamError", tt.query, err)
				}
				if pe.Param != tt.wantErrParam {
					t.Errorf("ParamError.Param = %q, want %q", pe.Param, tt.wantErrParam)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTransform(%q) error = %v", tt.query, err)
			}
			if got != tt.want {
				t.Errorf("ParseTransform(%q) = %+v, want %+v", tt.query, got, tt.want)
			}
		})
	}
}

func TestTransformIsIdentity(t *testing.T) {
	tests := []struct {
		name string
		t    Transform
		want bool
	}{
		{name: "zero value", t: Transform{}, want: true},
		{name: "width set", t: Transform{Width: 1}, want: false},
		{name: "height set", t: Transform{Height: 1}, want: false},
		{name: "format set", t: Transform{Format: FormatJPEG}, want: false},
		{name: "quality set", t: Transform{Quality: 80}, want: false},
		{name: "fit set", t: Transform{Fit: FitCover}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.t.IsIdentity(); got != tt.want {
				t.Errorf("IsIdentity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatContentType(t *testing.T) {
	tests := []struct {
		f    Format
		want string
	}{
		{FormatJPEG, "image/jpeg"},
		{FormatPNG, "image/png"},
		{FormatWebP, "image/webp"},
	}
	for _, tt := range tests {
		if got := tt.f.ContentType(); got != tt.want {
			t.Errorf("Format(%q).ContentType() = %q, want %q", tt.f, got, tt.want)
		}
	}
}

func TestCacheKey(t *testing.T) {
	id := uuid.MustParse("7d444840-9dc0-11d1-b245-5ffdce74fad2")
	otherID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	parse := func(t *testing.T, query string) Transform {
		t.Helper()
		q, err := url.ParseQuery(query)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", query, err)
		}
		tr, err := ParseTransform(q)
		if err != nil {
			t.Fatalf("ParseTransform(%q): %v", query, err)
		}
		return tr
	}

	t.Run("param order does not change the key", func(t *testing.T) {
		a := CacheKey(id, parse(t, "w=100&h=200"))
		b := CacheKey(id, parse(t, "h=200&w=100"))
		if a != b {
			t.Errorf("keys differ for reordered params: %q vs %q", a, b)
		}
	})

	t.Run("deterministic across calls", func(t *testing.T) {
		// Two structurally equal but distinct Transform values must hash the
		// same (guards against any pointer- or order-sensitive serialization).
		a := CacheKey(id, parse(t, "w=800&fmt=webp&q=80&fit=cover"))
		b := CacheKey(id, parse(t, "w=800&fmt=webp&q=80&fit=cover"))
		if a != b {
			t.Errorf("same input produced different keys: %q vs %q", a, b)
		}
	})

	t.Run("known value is stable across runs", func(t *testing.T) {
		// Pins the canonical serialization: if this changes, every cached
		// derivative is silently orphaned, so treat a diff here as breaking.
		got := CacheKey(id, Transform{Width: 100, Height: 200})
		want := "34a5fbf6590d2f57807e034b142155dce3923c99dcbc25a0ee544b7bc62d4e4c"
		if got != want {
			t.Errorf("CacheKey = %q, want pinned %q", got, want)
		}
	})

	t.Run("distinct inputs produce distinct keys", func(t *testing.T) {
		base := parse(t, "w=100&h=200")
		seen := map[string]string{CacheKey(id, base): "base"}
		distinct := map[string]Transform{
			"different width":  parse(t, "w=101&h=200"),
			"different height": parse(t, "w=100&h=201"),
			"format set":       parse(t, "w=100&h=200&fmt=webp"),
			"quality set":      parse(t, "w=100&h=200&q=80"),
			"fit set":          parse(t, "w=100&h=200&fit=cover"),
			"identity":         {},
			"jpeg vs png":      parse(t, "w=100&h=200&fmt=png"),
			"contain vs cover": parse(t, "w=100&h=200&fit=contain"),
			"quality boundary": parse(t, "w=100&h=200&q=100"),
			"width only":       parse(t, "w=100"),
			"height only":      parse(t, "h=200"),
		}
		for name, tr := range distinct {
			k := CacheKey(id, tr)
			if prev, dup := seen[k]; dup {
				t.Errorf("%s collides with %s (key %q)", name, prev, k)
			}
			seen[k] = name
		}
	})

	t.Run("different image ids differ", func(t *testing.T) {
		tr := parse(t, "w=100")
		if CacheKey(id, tr) == CacheKey(otherID, tr) {
			t.Error("same transform on different images produced the same key")
		}
	})
}

func TestParamErrorMessage(t *testing.T) {
	e := &ParamError{Param: "w", Msg: "must be >= 1"}
	if got, want := e.Error(), `invalid "w" parameter: must be >= 1`; got != want {
		t.Errorf("ParamError.Error() = %q, want %q", got, want)
	}
}
