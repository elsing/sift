-- spam_flags.reasons was a TEXT[] (postgres array) — pgx's stdlib database/sql driver
-- returns array columns as their raw string literal by default, not a Go []string,
-- so every read since 0028 (mails.go's handleMailBody) failed with "unsupported Scan,
-- storing driver.Value type string into type *[]string" and silently fell back to
-- "not yet scanned". A plain TEXT column (reasons joined with newlines) sidesteps the
-- array-decoding problem entirely instead of fighting the driver for it.
ALTER TABLE spam_flags ALTER COLUMN reasons DROP DEFAULT;
ALTER TABLE spam_flags ALTER COLUMN reasons TYPE TEXT USING array_to_string(reasons, E'\n');
ALTER TABLE spam_flags ALTER COLUMN reasons SET DEFAULT '';
