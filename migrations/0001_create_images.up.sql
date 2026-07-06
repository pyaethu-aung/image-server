CREATE TABLE images (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    original_filename text NOT NULL,
    content_hash text NOT NULL,
    mime_type text NOT NULL,
    width integer NOT NULL,
    height integer NOT NULL,
    size_bytes bigint NOT NULL,
    storage_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- dedup: identical upload returns the existing record
    CONSTRAINT images_content_hash_key UNIQUE (content_hash)
);
