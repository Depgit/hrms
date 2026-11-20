-- Round 2, payroll (prototype gaps E1): the day salaries are paid, the list of statutory files
-- created, reminders to declare tax, and Payroll's limits and on/off for declaration items.
-- Additive only: new tables and one column with a default.

-- When salaries are paid, kept with the company payroll settings (Pay schedule and components).
ALTER TABLE company_setting ADD COLUMN IF NOT EXISTS pay_day text NOT NULL DEFAULT 'Last working day'
  CHECK (pay_day IN ('Last working day', '1st of next month', '5th of next month'));

-- Every statutory or insurance file downloaded: which file, period, city, rows and who made it.
-- keys are the rows it held, so the same file can be downloaded again from the list.
CREATE TABLE IF NOT EXISTS statutory_file (
  id bigserial PRIMARY KEY,
  file text NOT NULL,
  from_date date,
  to_date date,
  ym char(7),
  city_id uuid REFERENCES city(id),
  skip_sent boolean NOT NULL DEFAULT false,
  rows int NOT NULL DEFAULT 0,
  keys text[] NOT NULL DEFAULT '{}',
  by_name text NOT NULL,
  by_role text NOT NULL DEFAULT '',
  at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS statutory_file_at_idx ON statutory_file(at DESC);

-- Reminders sent to permanent staff who have not declared their tax for a year.
CREATE TABLE IF NOT EXISTS tax_reminder (
  id bigserial PRIMARY KEY,
  fy text NOT NULL,
  employees text[] NOT NULL DEFAULT '{}',
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now()
);

-- Declaration items: Payroll's limit (0 = only the section limit) and on/off. An item without a
-- row keeps its built-in limit and stays on. Items are never deleted.
CREATE TABLE IF NOT EXISTS tax_item_setting (
  id text PRIMARY KEY,
  cap numeric(12,2) NOT NULL CHECK (cap >= 0),
  active boolean NOT NULL DEFAULT true,
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now()
);
