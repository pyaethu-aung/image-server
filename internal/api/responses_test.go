package api

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/pyaethu-aung/image-server/internal/db"
)

func TestToAPIImage(t *testing.T) {
	id := uuid.New()
	created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	got := toAPIImage(db.Image{
		ID:               id,
		OriginalFilename: "photo.jpg",
		ContentHash:      "abc123",
		MimeType:         "image/jpeg",
		Width:            800,
		Height:           600,
		SizeBytes:        12345,
		StorageKey:       "originals/ab/c1/abc123",
		CreatedAt:        pgtype.Timestamptz{Time: created, Valid: true},
	})

	if got.Id != id {
		t.Errorf("Id = %v, want %v", got.Id, id)
	}
	if got.OriginalFilename != "photo.jpg" {
		t.Errorf("OriginalFilename = %q", got.OriginalFilename)
	}
	if got.ContentHash != "abc123" {
		t.Errorf("ContentHash = %q", got.ContentHash)
	}
	if got.MimeType != "image/jpeg" {
		t.Errorf("MimeType = %q", got.MimeType)
	}
	if got.Width != 800 || got.Height != 600 {
		t.Errorf("dims = %dx%d, want 800x600", got.Width, got.Height)
	}
	if got.SizeBytes != 12345 {
		t.Errorf("SizeBytes = %d", got.SizeBytes)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}
}
