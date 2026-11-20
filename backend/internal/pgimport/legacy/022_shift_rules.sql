-- Set shift: attendance rules per shift, with the work policy as the default, and shift versions by effective date.
-- Additive only, safe on a database with real shifts:
--   * shift.rules holds the rules in effect today (like start_time and end_time, it mirrors the version in effect
--     today). It starts empty ('{}'), and an empty rule follows the employee's work policy, so nothing changes for an
--     existing shift until someone sets a rule on it.
--   * shift_version keeps each shift's timings and rules from a date. Every existing shift gets one version copying
--     its current values from 2000-01-01, so every date has a version and past days keep today's timings.
ALTER TABLE shift ADD COLUMN IF NOT EXISTS rules jsonb NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS shift_version (
  id bigserial PRIMARY KEY,
  shift_id uuid NOT NULL REFERENCES shift(id),
  effective_from date NOT NULL,
  start_time time NOT NULL,
  end_time time NOT NULL,
  break_minutes int NOT NULL DEFAULT 0,
  rules jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(),
  created_by text,
  note text,
  UNIQUE (shift_id, effective_from)
);

INSERT INTO shift_version(shift_id, effective_from, start_time, end_time, break_minutes, rules, created_by, note)
SELECT id, DATE '2000-01-01', start_time, end_time, break_minutes, rules, 'System', 'Shift as it stood before shift versions'
FROM shift
ON CONFLICT (shift_id, effective_from) DO NOTHING;
