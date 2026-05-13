ALTER TABLE stores ADD COLUMN allowed_origins text[] NOT NULL DEFAULT '{}';
