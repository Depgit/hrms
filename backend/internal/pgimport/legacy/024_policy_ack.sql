-- Company policy acknowledgement (Work policies › Company policies, My view › Dashboard › Compliance).
-- HR uploads a policy document with a version, an effective date and whom it applies to; publishing makes it
-- due. Each employee downloads it, signs it and uploads the signed copy, which goes to the reporting manager
-- through the approval engine (workflow p_ack). While anything is pending when the payroll run is made, the
-- salary for that month is held (salary_hold with source_id 'PACK'), and released once everything is approved.
-- Additive only, safe on a database with real data: new tables, two more document owner kinds, and the p_ack
-- approval flow where none exists. Nothing is due until HR publishes a policy, so nobody is held by this file.

CREATE SEQUENCE IF NOT EXISTS company_policy_seq START 1;

CREATE TABLE IF NOT EXISTS company_policy (
  id text PRIMARY KEY,                       -- CP-1, CP-2 …
  title text NOT NULL,
  status text NOT NULL DEFAULT 'Draft' CHECK (status IN ('Draft','Published','Retired')),
  -- Whom it applies to: {"all": true}, or any of "departments" (names), "locations" (ids) and "types"
  -- (employment types). A person must match every list that is set.
  audience jsonb NOT NULL DEFAULT '{"all": true}',
  new_joiners boolean NOT NULL DEFAULT true, -- people who join after the version is published must sign too
  current_version bigint,
  created_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  retired_by text, retired_at timestamptz, retire_note text
);

CREATE TABLE IF NOT EXISTS company_policy_version (
  id bigserial PRIMARY KEY,
  policy_id text NOT NULL REFERENCES company_policy(id),
  version text NOT NULL,
  effective_from date NOT NULL,
  document_id uuid NOT NULL REFERENCES document(id),
  note text,
  created_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  published_by text, published_at timestamptz,
  UNIQUE (policy_id, version)
);

-- One row per signed copy submitted; the newest row for a version and person is the one that counts. Its
-- status is the approval request's: Pending (waiting for the manager), Approved (signed), Rejected (returned).
CREATE TABLE IF NOT EXISTS policy_ack (
  id bigserial PRIMARY KEY,
  version_id bigint NOT NULL REFERENCES company_policy_version(id),
  employee_id text NOT NULL REFERENCES employee(id),
  document_id uuid NOT NULL REFERENCES document(id),
  request_id text NOT NULL REFERENCES approval_request(id),
  submitted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS policy_ack_emp_idx ON policy_ack(employee_id, version_id, id DESC);
CREATE INDEX IF NOT EXISTS policy_ack_version_idx ON policy_ack(version_id);

-- Documents: 'policy' (the policy file, owner = the version id, or a DRAFT- key before it is saved) and
-- 'policy_ack' (the signed copy, owner = "<employee id>/<version id>"). The owner-kind check is widened in
-- place, keeping every kind already allowed (as 014 does).
DO $$
DECLARE def text;
BEGIN
  SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint WHERE conname='document_owner_kind_check' AND conrelid='document'::regclass;
  IF def IS NOT NULL AND position('''policy''' IN def) = 0 AND position('ARRAY[' IN def) > 0 THEN
    EXECUTE 'ALTER TABLE document DROP CONSTRAINT document_owner_kind_check';
    EXECUTE 'ALTER TABLE document ADD CONSTRAINT document_owner_kind_check ' || replace(def, 'ARRAY[', 'ARRAY[''policy''::text, ''policy_ack''::text, ');
  END IF;
END $$;

-- The approval flow: the reporting manager approves (HR Admin and the HRBP can act too, in the server). If the
-- manager does not respond, the request keeps waiting: the salary stays held, so there is nobody to escalate to.
INSERT INTO approval_matrix(wf, flow, cols, rows, no_response) VALUES
  ('p_ack', 'Operations', '["Line Manager"]',
   '[{"id":"pack1","label":"Every policy","c":{"k":"always"},"on":true,"cells":{"Line Manager":"A"}}]', 'wait'),
  ('p_ack', 'Corporate', '["Line Manager"]',
   '[{"id":"pack2","label":"Every policy","c":{"k":"always"},"on":true,"cells":{"Line Manager":"A"}}]', 'wait')
ON CONFLICT (wf, flow) DO NOTHING;
