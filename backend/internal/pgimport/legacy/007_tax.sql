-- Sprint 6: income tax declarations and proofs, salary TDS, deposits, returns, Form 16,
-- statutory and insurance files (spec 16, 17).

-- Declaration and proof windows per financial year (force: NULL follows the dates).
CREATE TABLE tax_window (
  fy text PRIMARY KEY,                      -- 2026-27
  decl_from date, decl_to date, decl_force boolean,
  proof_from date, proof_to date, proof_force boolean,
  finalised_at timestamptz, finalised_by text,
  updated_by text, updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tax_declaration (
  employee_id text NOT NULL REFERENCES employee(id),
  fy text NOT NULL,
  regime text NOT NULL DEFAULT 'New' CHECK (regime IN ('New','Old')),
  status text NOT NULL DEFAULT 'Draft' CHECK (status IN ('Draft','Declared')),
  items jsonb NOT NULL DEFAULT '{}',        -- item id -> declared amount for the year
  landlord_name text NOT NULL DEFAULT '', landlord_pan text NOT NULL DEFAULT '',
  other_income numeric(12,2) NOT NULL DEFAULT 0,
  prev_income numeric(12,2) NOT NULL DEFAULT 0, prev_tds numeric(12,2) NOT NULL DEFAULT 0,
  submitted_at timestamptz,
  final_at timestamptz, final_diff numeric(12,2),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (employee_id, fy)
);

CREATE TABLE tax_proof (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  fy text NOT NULL,
  item text NOT NULL,
  doc uuid NOT NULL REFERENCES document(id),
  amount numeric(12,2) NOT NULL CHECK (amount > 0),
  remark text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'Pending' CHECK (status IN ('Pending','Approved','Rejected','Withdrawn')),
  approved numeric(12,2) NOT NULL DEFAULT 0,
  note text NOT NULL DEFAULT '',
  at timestamptz NOT NULL DEFAULT now(),
  reviewed_by text, reviewed_at timestamptz
);
CREATE INDEX tax_proof_emp_idx ON tax_proof(employee_id, fy);

-- TDS deposited to the government each month: s = salary (192), f = fees (194J).
CREATE TABLE tds_deposit (
  kind char(1) NOT NULL CHECK (kind IN ('s','f')),
  ym char(7) NOT NULL,
  date date NOT NULL,
  bsr char(7) NOT NULL CHECK (bsr ~ '^[0-9]{7}$'),
  challan char(5) NOT NULL CHECK (challan ~ '^[0-9]{5}$'),
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (kind, ym)
);

-- Quarterly returns 24Q (salary) and 26Q (fees): the receipt number after filing.
CREATE TABLE tds_return (
  fy text NOT NULL, form text NOT NULL CHECK (form IN ('24Q','26Q')), quarter int NOT NULL CHECK (quarter BETWEEN 1 AND 4),
  receipt text NOT NULL, by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (fy, form, quarter)
);

-- Form 16 is released to employees by a payroll approver, provisional during the year.
CREATE TABLE form16_release (
  fy text PRIMARY KEY,
  final boolean NOT NULL,
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE company_setting ADD COLUMN responsible_name text NOT NULL DEFAULT '', ADD COLUMN responsible_desig text NOT NULL DEFAULT '';

-- Range files can skip people already sent: the system remembers each person and action sent.
CREATE TABLE statutory_sent (
  file text NOT NULL,
  key text NOT NULL,
  sent_at timestamptz NOT NULL DEFAULT now(),
  by_name text NOT NULL,
  PRIMARY KEY (file, key)
);
