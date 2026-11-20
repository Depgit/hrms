-- Round 2 UI gaps: employee file, work policies, nominations (additive only).

-- A work policy or leave plan change records the date it applies from.
ALTER TABLE config_history ADD COLUMN IF NOT EXISTS effective_from date;

-- Nomination policy: Payroll checks and verifies (true), or a submission is final when sent (false).
ALTER TABLE nomination_scheme ADD COLUMN IF NOT EXISTS verify boolean NOT NULL DEFAULT true;

-- Reminders sent to employees who have not yet submitted a mandatory nomination.
CREATE TABLE IF NOT EXISTS nomination_reminder (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  scheme_id int NOT NULL REFERENCES nomination_scheme(id),
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS nomination_reminder_emp_idx ON nomination_reminder(employee_id, scheme_id);
