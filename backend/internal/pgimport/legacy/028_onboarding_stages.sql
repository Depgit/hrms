-- Onboarding in stages (owner's process, 11 Nov 2026). A candidate keeps one number through the stages:
-- Shortlisting ID SI-0001 → Offer ID OI-0001 → Joining ID JI-0001. The Employee ID comes later from the
-- employee code series (last created + 1), at joining or once the mandatory documents are in (a setting),
-- so a candidate who never joins never uses up an Employee ID.
CREATE SEQUENCE candidate_seq START 1;
CREATE TABLE candidate (
  id text PRIMARY KEY,                        -- SI-0001
  no int NOT NULL UNIQUE,                     -- 1 → SI-0001, OI-0001, JI-0001
  employee_id text REFERENCES employee(id),   -- set when the Employee ID is created
  created_at timestamptz NOT NULL DEFAULT now(),
  -- Stage 1, Shortlist: what HR fills in. Personal details, work experience rows and the rejoin code choice
  -- are kept in data; the job and place are references, so a renamed master shows its new name.
  name text NOT NULL,
  data jsonb NOT NULL DEFAULT '{}',
  existing_ctc numeric(14,2) CHECK (existing_ctc >= 0),     -- a year, in rupees
  department_id uuid NOT NULL REFERENCES department(id),
  designation_id uuid NOT NULL REFERENCES designation(id),
  location_id uuid NOT NULL REFERENCES location(id),
  manager_id text NOT NULL REFERENCES employee(id),         -- the hiring manager (reporting manager for the role)
  rejoin_of text REFERENCES employee(id),
  raised_by text NOT NULL REFERENCES employee(id),
  raised_role text NOT NULL DEFAULT '',
  -- Where the candidate is: Shortlisted (approval), Interview, Offer, Joined (JI) or Closed.
  stage text NOT NULL DEFAULT 'Shortlisted' CHECK (stage IN ('Shortlisted','Interview','Offer','Joined','Closed')),
  shortlist_request_id text,                  -- wf shortlist: the hiring manager, then the Function Head
  interview_request_id text,                  -- wf interview: the hiring manager records Selected or Not selected
  interview_date date,
  interview_result text CHECK (interview_result IN ('Selected','Not selected')),
  interview_feedback text,
  interviewed_by text REFERENCES employee(id),
  interviewed_at timestamptz,
  joined_on date,                             -- the actual joining date (Mark joined), never in the future
  joined_at timestamptz,
  joined_by text,
  employee_created_at timestamptz,
  closed_kind text CHECK (closed_kind IN ('Rejected','Withdrawn','Not selected','Declined','Not joined')),
  closed_reason text,
  closed_at timestamptz,
  closed_by text
);
CREATE INDEX candidate_employee_idx ON candidate(employee_id);
CREATE INDEX candidate_stage_idx ON candidate(stage, created_at DESC);
CREATE INDEX candidate_location_idx ON candidate(location_id);
CREATE INDEX candidate_rejoin_idx ON candidate(rejoin_of) WHERE rejoin_of IS NOT NULL;

-- Documents uploaded for a candidate: owner_kind 'candidate', owner_id = the SI id.
DO $$
DECLARE def text;
BEGIN
  SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint WHERE conname='document_owner_kind_check' AND conrelid='document'::regclass;
  IF def IS NOT NULL AND position('''candidate''' IN def) = 0 THEN
    EXECUTE 'ALTER TABLE document DROP CONSTRAINT document_owner_kind_check';
    EXECUTE 'ALTER TABLE document ADD CONSTRAINT document_owner_kind_check ' || replace(def, 'ARRAY[', 'ARRAY[''candidate''::text, ');
  END IF;
END $$;

-- The offer of a candidate is OI-0001 (same number). Offers raised before stages (OFR-1001) have no candidate
-- and keep working as before until they finish.
ALTER TABLE offer ADD COLUMN candidate_id text UNIQUE REFERENCES candidate(id);
-- A shortlist or interview request is about a candidate.
ALTER TABLE approval_request ADD COLUMN candidate_id text REFERENCES candidate(id);
CREATE INDEX approval_request_candidate_idx ON approval_request(candidate_id) WHERE candidate_id IS NOT NULL;

-- Candidates are approval subjects too, so routes, department scope (Function Head) and location scope work.
CREATE OR REPLACE VIEW approval_subject AS
  SELECT e.id, e.name, e.type, e.grade, e.location_id, e.department_id, e.manager_id, e.functional_manager_id, e.status FROM employee e
  UNION ALL
  SELECT o.id, o.candidate_name, o.type, o.grade, o.location_id, o.department_id, o.manager_id, o.functional_manager_id, 'Candidate' FROM offer o
  UNION ALL
  SELECT c.id, c.name, NULL::text, ''::text, c.location_id, c.department_id, c.manager_id, NULL::text, 'Candidate' FROM candidate c;

-- When the Employee ID is created: once the mandatory joining documents (and details) are in, or at joining.
CREATE TABLE onb_setting (
  id int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  employee_id_at text NOT NULL DEFAULT 'documents' CHECK (employee_id_at IN ('documents','joining')),
  updated_by text,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO onb_setting(id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- The shortlist approval (shown in Approval routes, so it can be changed): the hiring manager (Line Manager of the
-- candidate), then the Function Head of the department. The interview is always the hiring manager's, so it keeps
-- waiting for them rather than escalating, and it is not listed among the routes.
INSERT INTO approval_matrix(wf, flow, cols, rows, no_response) VALUES
  ('shortlist', 'Operations', '["Line Manager","Function Head"]',
   '[{"id":"sl1","label":"Every shortlist","c":{"k":"always"},"on":true,"cells":{"Line Manager":"A","Function Head":"A"}}]', 'escalate'),
  ('shortlist', 'Corporate', '["Line Manager","Function Head"]',
   '[{"id":"sl2","label":"Every shortlist","c":{"k":"always"},"on":true,"cells":{"Line Manager":"A","Function Head":"A"}}]', 'escalate'),
  ('interview', 'Operations', '["Line Manager"]',
   '[{"id":"iv1","label":"Every interview","c":{"k":"always"},"on":true,"cells":{"Line Manager":"A"}}]', 'wait'),
  ('interview', 'Corporate', '["Line Manager"]',
   '[{"id":"iv2","label":"Every interview","c":{"k":"always"},"on":true,"cells":{"Line Manager":"A"}}]', 'wait')
ON CONFLICT (wf, flow) DO NOTHING;
