-- Separation settings kept in the database (they were constants in the code), and a case event log
-- for what the other records do not carry with a date and a name: last working day changes, terms
-- agreed by an approver, and the rehire decision. Additive only: two new tables and one default row.

CREATE TABLE IF NOT EXISTS sep_setting (
  id int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  abs_days int NOT NULL DEFAULT 7 CHECK (abs_days >= 1),               -- days absent without leave before absconding can be started
  abs_letters int NOT NULL DEFAULT 2 CHECK (abs_letters BETWEEN 1 AND 5), -- show-cause letters before approval
  abs_wait int NOT NULL DEFAULT 3 CHECK (abs_wait >= 0),               -- days to wait after each letter
  exit_req text[] NOT NULL DEFAULT ARRAY['Resignation','Contract end','Retirement'], -- exit types that need an exit interview
  updated_by text,
  updated_at timestamptz
);
INSERT INTO sep_setting(id) VALUES (1) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS sep_event (
  id bigserial PRIMARY KEY,
  sep_id text NOT NULL REFERENCES separation(id),
  kind text NOT NULL,                                  -- lwd | terms | rehire
  t text NOT NULL,                                     -- what happened, as shown in the case history
  by_name text NOT NULL,
  by_role text,
  at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sep_event_sep_idx ON sep_event(sep_id, at);
