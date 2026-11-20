-- Sprint 8: scale (spec 22.1 #8). The tables that grow with every person and day are split
-- into yearly partitions, so a month's work reads one partition and old years can be archived.
-- Partitions exist from 2020 to 2035; the maintenance job adds the next years as they come.

-- Attendance days, partitioned by date.
ALTER TABLE attendance_day RENAME TO attendance_day_old;
ALTER TABLE attendance_day_old RENAME CONSTRAINT attendance_day_pkey TO attendance_day_old_pkey;
CREATE TABLE attendance_day (LIKE attendance_day_old INCLUDING DEFAULTS INCLUDING CONSTRAINTS, PRIMARY KEY (employee_id, date)) PARTITION BY RANGE (date);
ALTER TABLE attendance_day ADD FOREIGN KEY (employee_id) REFERENCES employee(id);

-- Raw punches from machines, partitioned by time.
ALTER SEQUENCE punch_id_seq OWNED BY NONE;
ALTER TABLE punch RENAME TO punch_old;
ALTER TABLE punch_old RENAME CONSTRAINT punch_pkey TO punch_old_pkey;
CREATE TABLE punch (LIKE punch_old INCLUDING DEFAULTS INCLUDING CONSTRAINTS, PRIMARY KEY (id, ts)) PARTITION BY RANGE (ts);
ALTER TABLE punch ADD FOREIGN KEY (employee_id) REFERENCES employee(id);
ALTER SEQUENCE punch_id_seq OWNED BY punch.id;
CREATE INDEX punch_emp_ts_idx ON punch(employee_id, ts);

-- Payroll rows, partitioned by payroll month.
ALTER TABLE payroll_row RENAME TO payroll_row_old;
ALTER TABLE payroll_row_old RENAME CONSTRAINT payroll_row_pkey TO payroll_row_old_pkey;
CREATE TABLE payroll_row (LIKE payroll_row_old INCLUDING DEFAULTS INCLUDING CONSTRAINTS, PRIMARY KEY (ym, employee_id)) PARTITION BY RANGE (ym);
ALTER TABLE payroll_row ADD FOREIGN KEY (ym) REFERENCES payroll_month(ym);
ALTER TABLE payroll_row ADD FOREIGN KEY (employee_id) REFERENCES employee(id);

DO $$
DECLARE y int;
BEGIN
  FOR y IN 2020..2035 LOOP
    EXECUTE format('CREATE TABLE attendance_day_%s PARTITION OF attendance_day FOR VALUES FROM (%L) TO (%L)', y, y || '-01-01', (y + 1) || '-01-01');
    EXECUTE format('CREATE TABLE punch_%s PARTITION OF punch FOR VALUES FROM (%L) TO (%L)', y, y || '-01-01', (y + 1) || '-01-01');
    EXECUTE format('CREATE TABLE payroll_row_%s PARTITION OF payroll_row FOR VALUES FROM (%L) TO (%L)', y, y || '-01', (y + 1) || '-01');
  END LOOP;
END $$;
CREATE TABLE attendance_day_default PARTITION OF attendance_day DEFAULT;
CREATE TABLE punch_default PARTITION OF punch DEFAULT;
CREATE TABLE payroll_row_default PARTITION OF payroll_row DEFAULT;

INSERT INTO attendance_day SELECT * FROM attendance_day_old;
INSERT INTO punch SELECT * FROM punch_old;
INSERT INTO payroll_row SELECT * FROM payroll_row_old;
DROP TABLE attendance_day_old, punch_old, payroll_row_old;

-- Indexes for the lists that grow: approvals by person, the audit log by time, ledger by source.
CREATE INDEX IF NOT EXISTS approval_request_emp_idx ON approval_request(employee_id, status);
CREATE INDEX IF NOT EXISTS audit_log_at_idx ON audit_log(at DESC);
CREATE INDEX IF NOT EXISTS employee_change_at_idx ON employee_change(employee_id, changed_at);

-- Punch machines (spec 11.2, built last): each device pushes punches with its own key.
CREATE TABLE attendance_device (
  id serial PRIMARY KEY,
  name text NOT NULL,
  location_id uuid REFERENCES location(id),
  vendor text NOT NULL DEFAULT '',
  serial text NOT NULL DEFAULT '',
  key_hash text NOT NULL UNIQUE,
  key_hint text NOT NULL,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  last_seen_at timestamptz,
  punches bigint NOT NULL DEFAULT 0,
  created_by text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
  inactive_since date, inactive_reason text
);
