-- Payroll run checklist (Payroll › Payroll run, "Before you run payroll"): five closures per payroll month,
-- Attendance, Leave, Bank verification, Reimbursement and Deduction. Each turns Done by itself when nothing is
-- pending; Payroll then closes it, with a name and a time. The dry run and every later step need all five
-- closed. Closing Attendance locks attendance as Month close did (payroll_month.locked_at), so Month close is gone.
-- In-app messages (notice): Payroll asks the person an item waits on (the employee to raise it, or the approver
-- to decide it); the bell in the header and the strip on the Dashboard show them.
-- Additive only, safe on a database with real data: three new tables, and months already verified, approved or
-- published (or with attendance locked by Month close) start with those closures closed, so nothing that was
-- allowed before is refused now.

CREATE TABLE IF NOT EXISTS payroll_closure (
  ym char(7) NOT NULL,
  closure text NOT NULL CHECK (closure IN ('attendance','leave','bank','claim','deduction')),
  closed_at timestamptz,
  closed_by text,
  reopened_at timestamptz,
  reopened_by text,
  reopen_reason text,
  PRIMARY KEY (ym, closure)
);

-- Not used: it was for Payroll setting pending items aside, which the owner ruled out (a closure closes only when
-- nothing is pending). Kept so the migration stays additive.
CREATE TABLE IF NOT EXISTS payroll_closure_item (
  ym char(7) NOT NULL,
  closure text NOT NULL,
  ref text NOT NULL,
  employee_id text NOT NULL REFERENCES employee(id),
  what text NOT NULL DEFAULT '',
  action text NOT NULL,
  note text NOT NULL,
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (ym, closure, ref)
);

CREATE TABLE IF NOT EXISTS notice (
  id bigserial PRIMARY KEY,
  to_employee_id text NOT NULL REFERENCES employee(id),
  from_employee_id text REFERENCES employee(id),
  kind text NOT NULL DEFAULT 'payroll',
  text text NOT NULL,
  link text,                         -- the page that opens it, e.g. /approvals?id=LV-3021
  view text,                         -- the role view the link belongs to (Employee for My view)
  ref text[] NOT NULL DEFAULT '{}',  -- the items it is about, "<ym>:<closure>:<item>"; done when all are resolved
  created_at timestamptz NOT NULL DEFAULT now(),
  read_at timestamptz,
  done_at timestamptz,               -- the item was raised, approved or rejected (or its closure closed)
  dismissed_at timestamptz           -- the person dismissed it from the Dashboard
);
CREATE INDEX IF NOT EXISTS notice_to_idx ON notice(to_employee_id, created_at DESC);
CREATE INDEX IF NOT EXISTS notice_ref_idx ON notice USING gin(ref);

-- Months already past the checklist keep working: verified, approved and published months have all five closed.
INSERT INTO payroll_closure(ym, closure, closed_at, closed_by)
SELECT m.ym, c.k, COALESCE(m.locked_at, m.verified_at, now()), COALESCE(m.locked_by, m.verified_by, 'Before the checklist')
FROM payroll_month m CROSS JOIN (VALUES ('attendance'), ('leave'), ('bank'), ('claim'), ('deduction')) AS c(k)
WHERE m.status IN ('Verified', 'Approved', 'Published')
ON CONFLICT DO NOTHING;

-- Attendance locked at Month close is the Attendance closure, closed.
INSERT INTO payroll_closure(ym, closure, closed_at, closed_by)
SELECT ym, 'attendance', locked_at, COALESCE(locked_by, 'Month close') FROM payroll_month WHERE locked_at IS NOT NULL
ON CONFLICT DO NOTHING;
