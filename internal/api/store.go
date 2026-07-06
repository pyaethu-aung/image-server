package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/pyaethu-aung/image-server/internal/db"
	"github.com/pyaethu-aung/image-server/internal/fetch"
)

// imageStore is the persistence seam the handlers depend on. *db.Queries
// satisfies it, so the untagged unit suite can substitute an in-memory fake
// and never needs a database.
type imageStore interface {
	CreateImage(ctx context.Context, arg db.CreateImageParams) (db.Image, error)
	GetImage(ctx context.Context, id uuid.UUID) (db.Image, error)
	GetImageByContentHash(ctx context.Context, contentHash string) (db.Image, error)
	DeleteImage(ctx context.Context, id uuid.UUID) (db.Image, error)
}

var _ imageStore = (*db.Queries)(nil)

// imageFetcher is the URL-download seam for POST /v1/images/from-url.
type imageFetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

var _ imageFetcher = (*fetch.Client)(nil)
