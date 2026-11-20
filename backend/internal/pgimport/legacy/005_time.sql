-- Sprint 4: work policies, holiday calendars, leave plans, roster, attendance, deviations, consequences.

-- ---------- Holiday calendars (spec 10) ----------
CREATE TABLE holiday_calendar (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE holiday (
  id bigserial PRIMARY KEY,
  calendar_id uuid NOT NULL REFERENCES holiday_calendar(id),
  date date NOT NULL,
  name text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('National','Regional','Optional')),
  on_list boolean NOT NULL DEFAULT true,
  UNIQUE (calendar_id, date, name)
);

-- ---------- Leave types and plans (spec 10.2) ----------
CREATE TABLE leave_type (
  name text PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('balance','none','event')),
  paid boolean NOT NULL DEFAULT true,
  builtin boolean NOT NULL DEFAULT false,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  sort int NOT NULL DEFAULT 100
);
INSERT INTO leave_type(name, kind, paid, builtin, sort) VALUES
 ('Casual leave','balance',true,true,1), ('Privilege leave','balance',true,true,2), ('Sick leave','balance',true,true,3),
 ('Comp-off','none',true,true,4), ('Loss of pay','none',false,true,5), ('Maternity leave','event',true,true,6);

CREATE TABLE leave_plan (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  description text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  lv jsonb NOT NULL,     -- leave year, pro-rate, half day, negative balance, year-end encashment
  lrx jsonb NOT NULL,    -- staffing limit, sandwich, clubbing
  types jsonb NOT NULL,  -- leave type name -> settings
  version int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- ---------- Work policies (spec 10) ----------
CREATE TABLE work_policy (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  sort int NOT NULL,                      -- the first active policy is the default
  settings jsonb NOT NULL DEFAULT '{}',   -- merged over the defaults in code
  leave_plan_id uuid REFERENCES leave_plan(id),
  calendar_id uuid REFERENCES holiday_calendar(id),
  cal_map jsonb NOT NULL DEFAULT '{}',    -- city id -> calendar id
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE work_policy_map (
  id bigserial PRIMARY KEY,
  policy_id uuid NOT NULL REFERENCES work_policy(id),
  level text NOT NULL CHECK (level IN ('employee','location','cluster','department','designation','grade','type','city','state')),
  ref text NOT NULL,                      -- id for places and employees, name for department, designation, grade, type
  on_map boolean NOT NULL DEFAULT true,
  created_by text, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX work_policy_map_ref_idx ON work_policy_map(level, ref) WHERE on_map;
-- Every change to a policy or plan, as field, old, new (never rewritten).
CREATE TABLE config_history (
  id bigserial PRIMARY KEY,
  entity text NOT NULL,                   -- work_policy | leave_plan | holiday_calendar
  entity_id text NOT NULL,
  changes jsonb NOT NULL,
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now()
);

-- Opening leave balances (balance as at a date; monthly credit continues after it).
CREATE TABLE leave_opening (
  employee_id text NOT NULL REFERENCES employee(id),
  leave_type text NOT NULL REFERENCES leave_type(name),
  at date NOT NULL,
  balance numeric(6,2) NOT NULL,
  created_by text, created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (employee_id, leave_type, at)
);

-- Payroll opens a window for one employee and date range until a date (revoked, never deleted).
CREATE TABLE time_exception (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  kind text NOT NULL CHECK (kind IN ('leave','regularize')),
  from_date date NOT NULL, to_date date NOT NULL, until date NOT NULL,
  reason text NOT NULL,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Revoked')),
  created_by text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  revoked_by text, revoked_at timestamptz
);

-- ---------- Roster (spec 11.1): only changes from the policy default are stored ----------
CREATE TABLE roster_entry (
  employee_id text NOT NULL REFERENCES employee(id),
  date date NOT NULL,
  code text NOT NULL,                     -- a shift code, WO or PH
  why text, late boolean NOT NULL DEFAULT false, from_code text,
  by_dev boolean NOT NULL DEFAULT false,  -- set by an approved deviation (roster regularization)
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (employee_id, date)
);
CREATE TABLE roster_log (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  date date NOT NULL, from_code text, to_code text NOT NULL,
  why text, late boolean NOT NULL DEFAULT false, by_dev boolean NOT NULL DEFAULT false,
  location_id uuid, department text,
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX roster_log_month_idx ON roster_log(date);
CREATE TABLE roster_publish (
  location_id uuid NOT NULL REFERENCES location(id),
  department text NOT NULL,
  week_start date NOT NULL,
  late boolean NOT NULL,
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (location_id, department, week_start)
);

-- ---------- Attendance (spec 11.2): one row per employee and day; status is derived, never stored ----------
CREATE TABLE attendance_day (
  employee_id text NOT NULL REFERENCES employee(id),
  date date NOT NULL,
  in_time text,                           -- HH:MM
  out_time text,
  out_date date,                          -- when the out punch is on a later date than the next day
  source text NOT NULL CHECK (source IN ('Punch machine','Mobile app','MIS upload','Regularized')),
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (employee_id, date)
);
-- Raw punches from the machine feed (built last); the day row above is derived from them.
CREATE TABLE punch (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  ts timestamptz NOT NULL, direction text CHECK (direction IN ('IN','OUT')), source text NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now()
);

-- ---------- Deviations and discipline (spec 11.5) ----------
CREATE TABLE deviation_decision (
  employee_id text NOT NULL REFERENCES employee(id),
  date date NOT NULL,
  planned text NOT NULL, actual text NOT NULL, kinds text[] NOT NULL,
  in_time text, out_time text, hours numeric(5,2),
  status text NOT NULL CHECK (status IN ('OK','Not OK')),
  note text, by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (employee_id, date)
);

-- ---------- Consequence management (spec 11.6) ----------
CREATE TABLE consequence (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  kind text NOT NULL CHECK (kind IN ('Warning','PIP')),
  seq int NOT NULL,
  days int NOT NULL DEFAULT 0,
  reason text NOT NULL, details text NOT NULL,
  status text NOT NULL CHECK (status IN ('Issued','Pending approval','Active','Completed','Rejected','Withdrawn')),
  approver_role text,
  from_date date, to_date date,
  outcome text,
  issued_by text NOT NULL, issued_role text NOT NULL, issued_at date NOT NULL DEFAULT current_date,
  decided_by text, decided_at timestamptz, note text,
  history jsonb NOT NULL DEFAULT '[]'
);
CREATE INDEX consequence_emp_idx ON consequence(employee_id);

-- ---------- Seed configuration (editable) ----------
INSERT INTO holiday_calendar(name) VALUES ('NCR 2026'), ('Karnataka 2026'), ('Telangana 2026'), ('Maharashtra 2026');
INSERT INTO holiday(calendar_id, date, name, kind)
SELECT c.id, h.d::date, h.n, h.k FROM holiday_calendar c CROSS JOIN (VALUES
  ('2026-01-26','Republic Day','National'), ('2026-03-04','Holi','National'), ('2026-08-15','Independence Day','National'),
  ('2026-10-02','Gandhi Jayanti','National'), ('2026-10-20','Dussehra','National'), ('2026-11-08','Diwali','National'), ('2026-12-25','Christmas','National'),
  ('2026-02-15','Maha Shivratri','Optional'), ('2026-04-03','Good Friday','Optional'), ('2026-08-28','Raksha Bandhan','Optional'), ('2026-11-24','Guru Nanak Jayanti','Optional')) h(d, n, k);
INSERT INTO holiday(calendar_id, date, name, kind) SELECT id, r.d::date, r.n, 'Regional' FROM holiday_calendar c JOIN (VALUES
  ('NCR 2026','2026-11-01','Haryana Day'), ('Karnataka 2026','2026-11-01','Karnataka Rajyotsava'),
  ('Telangana 2026','2026-06-02','Telangana Formation Day'), ('Maharashtra 2026','2026-05-01','Maharashtra Day')) r(cal, d, n) ON r.cal=c.name;
