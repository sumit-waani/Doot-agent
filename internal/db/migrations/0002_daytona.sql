-- 0002_daytona.sql
-- Settings for the Daytona sandbox, and columns tracking what the live sandbox
-- actually has (as opposed to what the defaults ask for).

-- Where the repo is cloned inside the sandbox.
ALTER TABLE project ADD COLUMN work_dir TEXT NOT NULL DEFAULT '/home/daytona/project';

-- Resolved once per sandbox from display.get_info(). The configured resolution
-- is a request; this is the geometry the X server actually allocated, and the
-- E2E verifier must scale coordinates against this, not against the setting.
ALTER TABLE project ADD COLUMN vnc_width INTEGER;
ALTER TABLE project ADD COLUMN vnc_height INTEGER;

-- Last observed computer-use process state. These processes do not survive a
-- sandbox stop, so this is a cache, never a source of truth.
ALTER TABLE project ADD COLUMN desktop_status TEXT;

-- Signed preview links survive sandbox restarts until they expire, so the URL
-- and its expiry are worth caching rather than re-issuing on every page load.
ALTER TABLE project ADD COLUMN preview_url TEXT;
ALTER TABLE project ADD COLUMN preview_expires_at TEXT;

ALTER TABLE project ADD COLUMN last_error TEXT;

-- Daytona connection.
INSERT OR IGNORE INTO settings (key, value) VALUES ('daytona.api_url', 'https://app.daytona.io/api');
INSERT OR IGNORE INTO settings (key, value) VALUES ('daytona.target', '');

-- Lifecycle. Every one of these is set explicitly rather than left to a server
-- default, because three of the four defaults would eventually destroy or stop
-- the sandbox out from under a run.
--
--   auto_stop      30  stop when genuinely idle; a heartbeat covers active runs
--   auto_archive    0  maximum delay (30 days); archived sandboxes start slower
--   auto_delete    -1  never. A deleted sandbox loses the working tree
--   ttl             0  disabled. Wall-clock TTL destroys a sandbox in any state
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.auto_archive_minutes', '0');
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.auto_delete_minutes', '-1');
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.ttl_minutes', '0');

-- Daytona's inactivity timer is not reset by work happening inside the sandbox,
-- so a long build reads as idle. This heartbeat is what keeps a running agent
-- from having its sandbox stopped mid-flight.
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.heartbeat_seconds', '300');

INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.public', '0');
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.preview_ttl_seconds', '3600');

-- noVNC's web port inside the sandbox. Configurable because it is inferred
-- rather than documented, and needs confirming against a live sandbox.
INSERT OR IGNORE INTO settings (key, value) VALUES ('sandbox.vnc_port', '6080');

-- Compressed screenshots keep computer-use affordable: a full-resolution PNG at
-- 1280x800 is expensive per step, and E2E is already the costliest part of the
-- loop.
INSERT OR IGNORE INTO settings (key, value) VALUES ('computeruse.screenshot_format', 'jpeg');
INSERT OR IGNORE INTO settings (key, value) VALUES ('computeruse.screenshot_quality', '80');
INSERT OR IGNORE INTO settings (key, value) VALUES ('computeruse.screenshot_scale', '1');
