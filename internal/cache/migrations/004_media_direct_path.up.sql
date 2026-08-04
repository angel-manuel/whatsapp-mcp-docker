-- media_direct_path stores the CDN-relative path whatsmeow needs to re-request
-- an attachment (`DownloadMediaWithPath`). It is the durable half of the media
-- locator: plain `media_url` values go stale, and whatsmeow's own Download
-- falls back to the direct path whenever the URL is a web.whatsapp.net one.
--
-- NOTE: this column CANNOT be backfilled. The direct path only exists on the
-- live protobuf at ingest time, so rows written before this migration keep the
-- empty-string default and can only be downloaded via their (possibly expired)
-- media_url. Re-ingesting via cache_sync is the recovery path.
ALTER TABLE messages ADD COLUMN media_direct_path TEXT NOT NULL DEFAULT '';
