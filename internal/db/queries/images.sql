-- name: CreateImage :one
INSERT INTO images (
    original_filename, content_hash, mime_type,
    width, height, size_bytes, storage_key
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING *;

-- name: GetImage :one
SELECT * FROM images
WHERE id = $1;

-- name: GetImageByContentHash :one
SELECT * FROM images
WHERE content_hash = $1;

-- name: DeleteImage :one
DELETE FROM images
WHERE id = $1
RETURNING *;
