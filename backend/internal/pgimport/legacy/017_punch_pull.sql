-- Scheduled pull of punches from the office server's API: when it last ran, what it found, and the time of the
-- last successful pull (the next one asks for punches since then, with a short overlap).
ALTER TABLE attendance_pull_config ADD COLUMN IF NOT EXISTS last_pull_at timestamptz;
ALTER TABLE attendance_pull_config ADD COLUMN IF NOT EXISTS last_pull_ok boolean;
ALTER TABLE attendance_pull_config ADD COLUMN IF NOT EXISTS last_pull_note text;
ALTER TABLE attendance_pull_config ADD COLUMN IF NOT EXISTS last_pull_count int;
ALTER TABLE attendance_pull_config ADD COLUMN IF NOT EXISTS pull_cursor timestamptz;
