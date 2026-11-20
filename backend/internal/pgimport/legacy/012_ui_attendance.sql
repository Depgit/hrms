-- Punch machines (spec 11.2): settings for pulling punches from the office server's API, next to
-- the machines that push. One row. The key is write-only: the API never returns it.
CREATE TABLE IF NOT EXISTS attendance_pull_config (
  id int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  url text NOT NULL DEFAULT '',
  method text NOT NULL DEFAULT 'GET' CHECK (method IN ('GET','POST')),
  auth text NOT NULL DEFAULT 'API key',
  secret text NOT NULL DEFAULT '',
  since_param text NOT NULL DEFAULT 'from',
  every_min int NOT NULL DEFAULT 15 CHECK (every_min BETWEEN 1 AND 1440),
  emp_field text NOT NULL DEFAULT 'emp_code',
  time_field text NOT NULL DEFAULT 'punch_time',
  dir_field text NOT NULL DEFAULT 'punch_type',
  in_value text NOT NULL DEFAULT 'IN',
  out_value text NOT NULL DEFAULT 'OUT',
  tested_at timestamptz,
  test_ok boolean,
  test_note text,
  updated_by text,
  updated_at timestamptz
);
INSERT INTO attendance_pull_config(id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- Recent attendance uploads are read from the audit log by entity.
CREATE INDEX IF NOT EXISTS audit_log_entity_idx ON audit_log(entity, entity_id, at DESC);
