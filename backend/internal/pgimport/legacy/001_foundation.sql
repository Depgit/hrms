-- Saltstayz HRMS: foundation schema (spec sections 4, 5, 6, 7).
-- Rules: nothing is deleted (status + inactive_since + inactive_reason); history tables are append-only;
-- effective-dated values carry effective_from.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------- Organisation masters ----------
CREATE TABLE region (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE state (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  region_id uuid NOT NULL REFERENCES region(id),
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE city (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  state_id uuid NOT NULL REFERENCES state(id),
  headcount_budget int NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (state_id, name)
);
CREATE TABLE cluster (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  city_id uuid NOT NULL REFERENCES city(id),
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (city_id, name)
);
CREATE TABLE location (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  cluster_id uuid NOT NULL REFERENCES cluster(id),
  headcount_budget int NOT NULL DEFAULT 0,
  payroll_budget numeric(14,2) NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- ---------- Job masters ----------
CREATE TABLE department (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  corporate boolean NOT NULL DEFAULT false, -- decides Operations vs Corporate flow (only source)
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE grade (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE designation (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  department_id uuid NOT NULL REFERENCES department(id),
  grades text[] NOT NULL DEFAULT '{}',
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (department_id, name)
);
CREATE TABLE shift (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  code text NOT NULL UNIQUE,
  name text NOT NULL,
  start_time time NOT NULL,
  end_time time NOT NULL,
  break_minutes int NOT NULL DEFAULT 0,
  location_ids uuid[], -- NULL = all locations
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now()
);
-- Simple named lists: qualification, employment type, attendance mode, change reasons.
CREATE TABLE simple_master (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind text NOT NULL CHECK (kind IN ('qualification','employment_type','attendance_mode','change_reason')),
  name text NOT NULL,
  used_for text, -- change_reason: Employee update | Salary | Separation | Master data | Hold | Deduction
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (kind, name, used_for)
);

-- ---------- Employees ----------
CREATE TABLE employee_code_series (
  key text PRIMARY KEY, -- 'All' or an employment type
  prefix text NOT NULL,
  pad int NOT NULL CHECK (pad BETWEEN 3 AND 8),
  next_no int NOT NULL CHECK (next_no > 0)
);
CREATE TABLE employee_code_settings (
  id int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  by_type boolean NOT NULL DEFAULT false,
  rejoin text NOT NULL DEFAULT 'same' CHECK (rejoin IN ('same','new'))
);

CREATE TABLE employee (
  id text PRIMARY KEY, -- employee code, never reused
  name text NOT NULL,
  type text NOT NULL CHECK (type IN ('Permanent','Retainer','Contractual')),
  doj date NOT NULL,
  department_id uuid NOT NULL REFERENCES department(id),
  designation_id uuid NOT NULL REFERENCES designation(id),
  grade text NOT NULL,
  location_id uuid NOT NULL REFERENCES location(id),
  shift_code text NOT NULL,
  manager_id text REFERENCES employee(id),            -- L1
  functional_manager_id text REFERENCES employee(id), -- L2
  attendance_mode text NOT NULL DEFAULT 'Biometric',
  status text NOT NULL DEFAULT 'Pre-boarding' CHECK (status IN ('Pre-boarding','Active','Inactive')),
  exit_status text, -- 'On notice'
  lwd date,
  resign_at date,
  exit_reason text,
  rehire text,
  verified boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX employee_location_idx ON employee(location_id);
CREATE INDEX employee_manager_idx ON employee(manager_id);
CREATE INDEX employee_status_idx ON employee(status);

-- Separate tables so rights differ per group.
CREATE TABLE employee_personal (
  employee_id text PRIMARY KEY REFERENCES employee(id),
  dob date, gender text, mobile text, email text, address text, father_spouse text, marital text,
  ec_name text, ec_relation text, ec_mobile text
);
CREATE TABLE employee_gov_id (
  employee_id text PRIMARY KEY REFERENCES employee(id),
  aadhaar text, pan text, uan text, esic text
);
CREATE TABLE employee_bank (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  bank_name text NOT NULL, account_no text NOT NULL, ifsc text NOT NULL,
  from_month char(7) NOT NULL, -- YYYY-MM
  created_by text, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX employee_bank_emp_idx ON employee_bank(employee_id, from_month);

-- Per-employee change history (bulk and single changes; spec 2 and 9).
CREATE TABLE employee_change (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  field text NOT NULL,
  old_value text, new_value text,
  reason text NOT NULL,
  effective_from date NOT NULL,
  state text NOT NULL DEFAULT 'Applied' CHECK (state IN ('Applied','Scheduled')),
  source text NOT NULL DEFAULT 'Manual',
  apply_column text,  -- employee column to set when a Scheduled change becomes effective
  apply_value text,
  changed_by text NOT NULL,
  changed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX employee_change_emp_idx ON employee_change(employee_id);

-- ---------- Compensation ----------
CREATE TABLE salary_structure (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  rates jsonb NOT NULL, -- StructureRates in @hrms/core
  is_default boolean NOT NULL DEFAULT false,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX one_default_structure ON salary_structure(is_default) WHERE is_default;
CREATE TABLE structure_map (
  employee_id text NOT NULL REFERENCES employee(id),
  from_date date NOT NULL,
  structure_id uuid NOT NULL REFERENCES salary_structure(id),
  PRIMARY KEY (employee_id, from_date)
);
CREATE TABLE gross_history (
  employee_id text NOT NULL REFERENCES employee(id),
  from_month char(7) NOT NULL,
  gross numeric(12,2) NOT NULL CHECK (gross > 0),
  old_gross numeric(12,2),
  reason text,
  created_by text, created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (employee_id, from_month)
);
CREATE TABLE employee_pay (
  employee_id text PRIMARY KEY REFERENCES employee(id),
  var_pct numeric(5,2), var_cycle int CHECK (var_cycle IN (1,3,6,12)),
  meal numeric(10,2) NOT NULL DEFAULT 0, accommodation numeric(10,2) NOT NULL DEFAULT 0,
  tds_pct numeric(5,2)
);
CREATE TABLE lwf_rule (
  state_id uuid NOT NULL REFERENCES state(id),
  effective_from date NOT NULL,
  rule jsonb NOT NULL, -- LwfRule in @hrms/core
  PRIMARY KEY (state_id, effective_from)
);
CREATE TABLE pay_band (
  city_id uuid NOT NULL REFERENCES city(id),
  grade text NOT NULL,
  min_gross numeric(12,2) NOT NULL,
  max_gross numeric(12,2) NOT NULL CHECK (max_gross >= min_gross),
  PRIMARY KEY (city_id, grade)
);

-- ---------- Roles, rights, mapping, sign-in ----------
CREATE TABLE role (
  name text PRIMARY KEY,
  description text,
  active boolean NOT NULL DEFAULT true,
  system boolean NOT NULL DEFAULT false,
  can_download boolean NOT NULL DEFAULT true
);
CREATE TABLE role_right (
  role_name text NOT NULL REFERENCES role(name),
  right_id text NOT NULL,
  level smallint NOT NULL CHECK (level BETWEEN 0 AND 2),
  PRIMARY KEY (role_name, right_id)
);
CREATE TABLE role_sensitive (
  role_name text NOT NULL REFERENCES role(name),
  grp text NOT NULL CHECK (grp IN ('salary','govId','bank','medical')),
  mode text NOT NULL CHECK (mode IN ('Full','Masked','Hidden')),
  PRIMARY KEY (role_name, grp)
);
-- One person can hold several roles and places. Scope: all | locs | team | self, optional departments.
CREATE TABLE user_role_map (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  employee_id text NOT NULL REFERENCES employee(id),
  role_name text NOT NULL REFERENCES role(name),
  access text NOT NULL CHECK (access IN ('all','locs')),
  location_ids uuid[] NOT NULL DEFAULT '{}',
  departments text[] NOT NULL DEFAULT '{}',
  active boolean NOT NULL DEFAULT true,
  created_by text, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (employee_id, role_name)
);

CREATE TABLE auth_credential (
  employee_id text PRIMARY KEY REFERENCES employee(id),
  salt text NOT NULL,
  hash text NOT NULL,
  fails int NOT NULL DEFAULT 0,
  locked_until timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE auth_otp (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  employee_id text NOT NULL REFERENCES employee(id),
  code_hash text NOT NULL,
  purpose text NOT NULL CHECK (purpose IN ('first','forgot')),
  tries int NOT NULL DEFAULT 0,
  verified_at timestamptz,
  used boolean NOT NULL DEFAULT false,
  sent_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL
);
CREATE INDEX auth_otp_emp_idx ON auth_otp(employee_id, sent_at DESC);
CREATE TABLE auth_session (
  token_hash text PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL
);

-- ---------- Approval engine ----------
CREATE TABLE approval_matrix (
  wf text NOT NULL,   -- joining, verify, leaveC, leaveP, regularize, transfer, resign, p_claim, ...
  flow text NOT NULL CHECK (flow IN ('Operations','Corporate')),
  cols jsonb NOT NULL,
  rows jsonb NOT NULL,
  no_response text NOT NULL DEFAULT 'escalate' CHECK (no_response IN ('escalate','auto','wait')),
  updated_by text, updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (wf, flow)
);
CREATE TABLE approval_request (
  id text PRIMARY KEY,
  wf text NOT NULL,
  employee_id text REFERENCES employee(id),
  raised_by text NOT NULL REFERENCES employee(id),
  flow text NOT NULL,
  facts jsonb NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}',
  chain jsonb NOT NULL,
  notify jsonb NOT NULL DEFAULT '[]',
  why jsonb NOT NULL DEFAULT '[]',
  step int NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'Pending' CHECK (status IN ('Pending','Approved','Rejected','Cancelled')),
  stage_since timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX approval_request_status_idx ON approval_request(status, wf);
CREATE SEQUENCE approval_request_seq START 3001;
CREATE TABLE approval_history (
  id bigserial PRIMARY KEY,
  request_id text NOT NULL REFERENCES approval_request(id),
  stage text,
  action text NOT NULL, -- Approved | Rejected | Skipped | Rerouted | Escalated | Cancelled
  by_name text NOT NULL,
  note text,
  at timestamptz NOT NULL DEFAULT now()
);

-- ---------- Pay ledger (every extra pay line, spec 15) ----------
CREATE TABLE ledger (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  ym char(7) NOT NULL,
  kind text NOT NULL CHECK (kind IN ('earn','ded')),
  label text NOT NULL,
  amount numeric(12,2) NOT NULL CHECK (amount >= 0),
  source_id text,
  reason text,
  void boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_emp_ym_idx ON ledger(employee_id, ym);

-- ---------- Audit (append-only) ----------
CREATE TABLE audit_log (
  id bigserial PRIMARY KEY,
  actor text NOT NULL,
  actor_role text,
  action text NOT NULL,
  entity text,
  entity_id text,
  before jsonb,
  after jsonb,
  at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'audit_log is append-only'; END $$;
CREATE TRIGGER audit_log_no_change BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();
