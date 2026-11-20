-- Sprint 5: payroll cycle, pay items, payslips (spec 13-15).

-- A ledger line is never deleted; a moved or replaced line is voided with the reason.
ALTER TABLE ledger ADD COLUMN void_reason text, ADD COLUMN voided_at timestamptz;
CREATE INDEX ledger_source_idx ON ledger(source_id);

-- One row per payroll month: Open -> Dry run -> Verified -> Approved -> Published (spec 15.1).
CREATE TABLE payroll_month (
  ym char(7) PRIMARY KEY,
  status text NOT NULL DEFAULT 'Open' CHECK (status IN ('Open','Dry run','Verified','Approved','Published')),
  locked_at timestamptz, locked_by text,           -- attendance lock at month close
  pilot text[],                                    -- dry run limited to these people; NULL = everyone
  reminded jsonb NOT NULL DEFAULT '{}',            -- approver key -> when reminded
  computed_at timestamptz, computed_by text,
  verified_at timestamptz, verified_by text,
  approved_at timestamptz, approved_by text,
  published_at timestamptz, published_by text,
  totals jsonb NOT NULL DEFAULT '{}'
);

-- The computed row for each person in the run; data holds the full breakup.
CREATE TABLE payroll_row (
  ym char(7) NOT NULL REFERENCES payroll_month(ym),
  employee_id text NOT NULL REFERENCES employee(id),
  net numeric(12,2) NOT NULL,
  hold boolean NOT NULL DEFAULT false,
  flags text[] NOT NULL DEFAULT '{}',
  data jsonb NOT NULL,
  computed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (ym, employee_id)
);

-- Payroll's manual edits on the dry run: paid days, other earning, other deduction, always with a reason.
CREATE TABLE payroll_override (
  ym char(7) NOT NULL,
  employee_id text NOT NULL REFERENCES employee(id),
  paid_days numeric(5,2),
  other_earn numeric(12,2) NOT NULL DEFAULT 0,
  other_ded numeric(12,2) NOT NULL DEFAULT 0,
  reason text NOT NULL,
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (ym, employee_id)
);

CREATE TABLE payslip (
  ym char(7) NOT NULL,
  employee_id text NOT NULL REFERENCES employee(id),
  snap jsonb NOT NULL,
  net numeric(12,2) NOT NULL,
  published_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (ym, employee_id)
);

-- Salary TDS deducted, written when a month is published (spec 16).
CREATE TABLE tds_history (
  employee_id text NOT NULL REFERENCES employee(id),
  ym char(7) NOT NULL,
  section text NOT NULL DEFAULT '192',
  amount numeric(12,2) NOT NULL,
  PRIMARY KEY (employee_id, ym, section)
);

-- Salary holds (spec 15.2): reasons from the Hold change reasons; release pays every held month.
CREATE SEQUENCE salary_hold_seq START 1;
CREATE TABLE salary_hold (
  id text PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  reason text NOT NULL,
  note text NOT NULL,
  from_ym char(7) NOT NULL,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Released')),
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  source_id text,                                  -- set when a separation case placed the hold
  released_by text, released_at timestamptz, release_note text,
  paid jsonb NOT NULL DEFAULT '[]'
);
CREATE UNIQUE INDEX one_active_hold ON salary_hold(employee_id) WHERE status = 'Active';

-- A negative net carried to next month; for people who have left it waits for the full and final.
CREATE TABLE carry_over (
  key text PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  ym char(7) NOT NULL,
  amount numeric(12,2) NOT NULL,
  settled boolean NOT NULL DEFAULT false,
  settled_in text
);

-- Payslip setup and payroll cut-off live with the company details.
ALTER TABLE company_setting
  ADD COLUMN cin text NOT NULL DEFAULT '',
  ADD COLUMN payslip jsonb NOT NULL DEFAULT '{"accent":"#0f6e6e","footer":"This is a computer-generated payslip and does not need a signature.","showEmployer":true}',
  ADD COLUMN logo_doc uuid,
  ADD COLUMN cutoff text NOT NULL DEFAULT 'calendar' CHECK (cutoff IN ('calendar','26-25')),
  ADD COLUMN first_ym char(7);

-- The payslip logo is a company document.
ALTER TABLE document DROP CONSTRAINT document_owner_kind_check;
ALTER TABLE document ADD CONSTRAINT document_owner_kind_check CHECK (owner_kind IN ('offer','employee','nomination','company'));

-- Approve and publish belong to the Payroll Manager (and CHRO), not the Payroll executive (spec 15.1).
UPDATE role_right SET level = 1 WHERE role_name = 'Payroll' AND right_id = 'pay.approve' AND level = 2;
INSERT INTO role_right(role_name, right_id, level) SELECT 'CHRO', 'pay.approve', 2 WHERE EXISTS (SELECT 1 FROM role WHERE name = 'CHRO')
  ON CONFLICT (role_name, right_id) DO UPDATE SET level = 2;

-- Large runs are computed by a background job; the month shows it while it runs.
ALTER TABLE payroll_month ADD COLUMN job text, ADD COLUMN job_error text;

-- Comp-off earned by working a weekly off or holiday, once the deviation is marked OK (spec 10.1).
CREATE TABLE compoff_credit (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  date date NOT NULL,
  days numeric(3,1) NOT NULL CHECK (days > 0),
  expires date NOT NULL,
  hours numeric(5,2) NOT NULL,
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (employee_id, date)
);
