-- Sprint 3: onboarding (offer to active employee), documents, company details, rejoin, nominations.

-- ---------- Company details (payslips, letters, statutory files) ----------
CREATE TABLE company_setting (
  id int PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  name text NOT NULL DEFAULT 'Saltstayz',
  legal_name text NOT NULL DEFAULT 'Saltstayz Hospitality Private Limited',
  address text NOT NULL DEFAULT 'Registered office address (edit in Masters, Company)',
  pan text NOT NULL DEFAULT '', tan text NOT NULL DEFAULT '', gstin text NOT NULL DEFAULT '',
  pf_code text NOT NULL DEFAULT '', esi_code text NOT NULL DEFAULT '',
  offer_template text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO company_setting(id, offer_template) VALUES (1, 'Dear {{name}},

We are pleased to offer you the position of {{designation}} in the {{department}} department at {{company}}, based at {{location}}, {{city}}. Your grade is {{grade}} and your date of joining is {{doj}}.

Compensation: monthly gross of {{gross}} (fixed {{fixed}}, variable {{variable}}), with an annual CTC of {{ctcYear}}. Estimated net in hand is {{net}} a month before tax.

Your employment is subject to a probation of {{probation}}, verification of documents and the policies of the company.

Please confirm your acceptance by replying to this email.

Warm regards,
People Team, {{company}}');

-- ---------- Onboarding form setup (spec 8.2): state 0 not asked, 1 optional, 2 mandatory ----------
CREATE TABLE onb_field (
  key text PRIMARY KEY,
  label text NOT NULL,
  section text NOT NULL,
  kind text NOT NULL,            -- text, number, date, email, tel, sel, flow, policy
  options text,                  -- comma separated, for custom drop-downs
  on_form boolean NOT NULL DEFAULT true,
  sys boolean NOT NULL DEFAULT false,
  custom boolean NOT NULL DEFAULT false,
  offer_state smallint NOT NULL CHECK (offer_state BETWEEN 0 AND 2),
  join_state smallint NOT NULL CHECK (join_state BETWEEN 0 AND 2),
  types text[] NOT NULL DEFAULT '{Permanent,Retainer,Contractual}',
  sort int NOT NULL
);
CREATE TABLE onb_document (
  key text PRIMARY KEY,
  label text NOT NULL,
  on_form boolean NOT NULL DEFAULT true,
  sys boolean NOT NULL DEFAULT false,
  offer_state smallint NOT NULL CHECK (offer_state BETWEEN 0 AND 2),
  join_state smallint NOT NULL CHECK (join_state BETWEEN 0 AND 2),
  types text[] NOT NULL DEFAULT '{Permanent,Retainer,Contractual}',
  depts text[] NOT NULL DEFAULT '{}',  -- empty = every department
  sort int NOT NULL
);

INSERT INTO onb_field(key, label, section, kind, options, sys, offer_state, join_state, types, sort) VALUES
 ('first','First name','Personal','text',NULL,true,2,0,'{Permanent,Retainer,Contractual}',1),
 ('last','Last name','Personal','text',NULL,true,2,0,'{Permanent,Retainer,Contractual}',2),
 ('gender','Gender','Personal','sel','Male,Female,Other,Prefer not to say',false,0,2,'{Permanent,Retainer,Contractual}',3),
 ('dob','Date of birth','Personal','date',NULL,false,0,2,'{Permanent,Retainer,Contractual}',4),
 ('marital','Marital status','Personal','sel','Single,Married,Other',false,0,1,'{Permanent,Retainer,Contractual}',5),
 ('nationality','Nationality','Personal','text',NULL,false,0,1,'{Permanent,Retainer,Contractual}',6),
 ('email','Personal email ID','Personal','email',NULL,false,2,0,'{Permanent,Retainer,Contractual}',7),
 ('phone','Mobile number','Personal','tel',NULL,false,2,0,'{Permanent,Retainer,Contractual}',8),
 ('blood','Blood group','Personal','sel','A+,A-,B+,B-,O+,O-,AB+,AB-',false,0,1,'{Permanent,Retainer,Contractual}',9),
 ('ecName','Emergency contact name','Personal','text',NULL,false,0,1,'{Permanent,Retainer,Contractual}',10),
 ('ecRel','Emergency contact relation','Personal','sel','Spouse,Father,Mother,Brother,Sister,Son,Daughter,Friend,Other',false,0,1,'{Permanent,Retainer,Contractual}',11),
 ('ecPhone','Emergency contact mobile','Personal','tel',NULL,false,0,1,'{Permanent,Retainer,Contractual}',12),
 ('addr','Current address','Personal','text',NULL,false,0,1,'{Permanent,Retainer,Contractual}',13),
 ('aadhaar','Aadhaar number','Identity and statutory','text',NULL,false,0,2,'{Permanent,Retainer,Contractual}',20),
 ('pan','PAN','Identity and statutory','text',NULL,false,0,2,'{Permanent,Retainer,Contractual}',21),
 ('uanAvail','Existing UAN available?','Identity and statutory','sel','Yes,No',false,0,2,'{Permanent,Contractual}',22),
 ('uan','UAN','Identity and statutory','text',NULL,false,0,2,'{Permanent,Contractual}',23),
 ('esiIp','ESI IP number, if existing','Identity and statutory','text',NULL,false,0,1,'{Permanent,Contractual}',24),
 ('tax','Tax regime','Identity and statutory','sel','New regime,Old regime',false,0,1,'{Permanent,Contractual}',25),
 ('bankName','Bank name','Bank','text',NULL,false,0,2,'{Permanent,Retainer,Contractual}',30),
 ('acct','Account number','Bank','text',NULL,false,0,2,'{Permanent,Retainer,Contractual}',31),
 ('ifsc','IFSC code','Bank','text',NULL,false,0,2,'{Permanent,Retainer,Contractual}',32),
 ('type','Employment type','Job','sel',NULL,true,2,0,'{Permanent,Retainer,Contractual}',40),
 ('doj','Date of joining','Job','date',NULL,true,2,0,'{Permanent,Retainer,Contractual}',41),
 ('dept','Department','Job','sel',NULL,true,2,0,'{Permanent,Retainer,Contractual}',42),
 ('desig','Designation','Job','sel',NULL,true,2,0,'{Permanent,Retainer,Contractual}',43),
 ('grade','Grade','Job','sel',NULL,false,2,0,'{Permanent,Retainer,Contractual}',44),
 ('flow','Approval flow','Job','flow',NULL,true,1,0,'{Permanent,Retainer,Contractual}',45),
 ('skill','Skill or trade','Job','text',NULL,false,0,1,'{Permanent,Retainer,Contractual}',46),
 ('qual','Highest qualification','Job','sel',NULL,false,0,1,'{Permanent,Retainer,Contractual}',47),
 ('endDate','Contract end date','Job','date',NULL,false,2,0,'{Retainer,Contractual}',48),
 ('agency','Agency or vendor','Job','text',NULL,false,2,0,'{Contractual}',49),
 ('city','Work city','Location and reporting','sel',NULL,true,2,0,'{Permanent,Retainer,Contractual}',50),
 ('loc','Reporting location','Location and reporting','sel',NULL,true,2,0,'{Permanent,Retainer,Contractual}',51),
 ('policy','Work policy','Location and reporting','policy',NULL,true,1,0,'{Permanent,Retainer,Contractual}',52),
 ('shift','Shift','Location and reporting','sel',NULL,false,0,2,'{Permanent,Retainer,Contractual}',53),
 ('attMode','Attendance mode','Location and reporting','sel',NULL,false,0,2,'{Permanent,Retainer,Contractual}',54),
 ('l1','Reporting manager (L1)','Location and reporting','sel',NULL,false,0,2,'{Permanent,Retainer,Contractual}',55),
 ('l2','Functional manager (L2)','Location and reporting','sel',NULL,false,0,1,'{Permanent,Retainer,Contractual}',56),
 ('medical','Medical insurance nomination','Benefits','sel','Employee only,Employee + spouse,Employee + family,Opt out',false,0,1,'{Permanent}',60),
 ('recruiter','Recruiter name','Hiring','text',NULL,false,0,1,'{Permanent,Retainer,Contractual}',70),
 ('source','Source of hire','Hiring','sel','Referral,Job portal,Walk-in,Agency,Campus',false,0,1,'{Permanent,Retainer,Contractual}',71),
 ('offerRef','Offer letter reference','Hiring','text',NULL,false,0,1,'{Permanent,Retainer,Contractual}',72),
 ('gross','Monthly gross (₹)','Compensation','number',NULL,true,2,0,'{Permanent,Retainer,Contractual}',80),
 ('structId','Salary structure version','Compensation','sel',NULL,false,1,0,'{Permanent,Retainer,Contractual}',81),
 ('varPct','Variable pay, % of gross, on top of fixed (blank = standard)','Compensation','number',NULL,false,1,0,'{Permanent,Contractual}',82),
 ('varCycle','Variable pay payout (blank = standard)','Compensation','sel',NULL,false,1,0,'{Permanent,Contractual}',83),
 ('dMeal','Meal deduction (₹ a month)','Compensation','number',NULL,false,1,0,'{Permanent}',84),
 ('dAccom','Accommodation deduction (₹ a month)','Compensation','number',NULL,false,1,0,'{Permanent}',85),
 ('tdsPct','TDS (% of gross, blank = standard)','Compensation','number',NULL,false,1,0,'{Retainer}',86);

INSERT INTO onb_document(key, label, sys, offer_state, join_state, types, depts, sort) VALUES
 ('aadhaar','Aadhaar card',true,2,2,'{Permanent,Retainer,Contractual}','{}',1),
 ('pan','PAN card',false,0,2,'{Permanent,Retainer,Contractual}','{}',2),
 ('photo','Passport-size photo',false,0,2,'{Permanent,Retainer,Contractual}','{}',3),
 ('bankProof','Cancelled cheque or bank passbook',false,0,2,'{Permanent,Retainer,Contractual}','{}',4),
 ('addr','Address proof',false,0,1,'{Permanent,Retainer,Contractual}','{}',5),
 ('edu','Highest qualification certificate',false,0,1,'{Permanent}','{}',6),
 ('prev','Relieving or experience letter',false,0,1,'{Permanent}','{}',7),
 ('form11','Form 11 or UAN passbook',false,0,1,'{Permanent,Contractual}','{}',8),
 ('police','Police verification',false,0,2,'{Permanent,Retainer,Contractual}','{Housekeeping,"Front Office",Operations}',9),
 ('medcert','Medical fitness certificate for food handlers',false,0,2,'{Permanent,Retainer,Contractual}','{F&B}',10),
 ('gst','GST registration',false,0,1,'{Retainer}','{}',11),
 ('agreement','Signed contract or retainer agreement',false,0,2,'{Retainer,Contractual}','{}',12),
 ('agencyId','Agency ID or vendor authorisation',false,0,2,'{Contractual}','{}',13);

-- ---------- Offers: the candidate until verified (spec 8) ----------
CREATE SEQUENCE offer_seq START 1001;
CREATE TABLE offer (
  id text PRIMARY KEY,                       -- OFR-1001
  candidate_name text NOT NULL,
  type text NOT NULL CHECK (type IN ('Permanent','Retainer','Contractual')),
  doj date NOT NULL,
  department_id uuid NOT NULL REFERENCES department(id),
  designation_id uuid NOT NULL REFERENCES designation(id),
  grade text NOT NULL,
  location_id uuid NOT NULL REFERENCES location(id),
  manager_id text REFERENCES employee(id),
  functional_manager_id text REFERENCES employee(id),
  gross numeric(12,2) NOT NULL CHECK (gross > 0),
  band_pos text,                             -- below, within, above, none
  budget_over text[] NOT NULL DEFAULT '{}',  -- which budgets the hire is above
  data jsonb NOT NULL DEFAULT '{}',          -- every form value, offer and joining stage
  rejoin_of text REFERENCES employee(id),
  request_id text,                           -- offer approval (wf joining)
  employee_id text REFERENCES employee(id),  -- created on final approval, as pre-boarding
  released_at timestamptz,
  accepted_at timestamptz,
  declined_at timestamptz,
  verify_request_id text,                    -- joining verification (wf verify)
  verified_at timestamptz,
  raised_by text NOT NULL REFERENCES employee(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX offer_location_idx ON offer(location_id);

ALTER TABLE approval_request ADD COLUMN offer_id text REFERENCES offer(id);

-- Who a request is about: an employee, or a candidate while the offer is being approved.
CREATE VIEW approval_subject AS
  SELECT e.id, e.name, e.type, e.grade, e.location_id, e.department_id, e.manager_id, e.functional_manager_id, e.status FROM employee e
  UNION ALL
  SELECT o.id, o.candidate_name, o.type, o.grade, o.location_id, o.department_id, o.manager_id, o.functional_manager_id, 'Candidate' FROM offer o;

-- ---------- Documents (files live in object storage; this is the index) ----------
CREATE TABLE document (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_kind text NOT NULL CHECK (owner_kind IN ('offer','employee','nomination')),
  owner_id text NOT NULL,
  employee_id text REFERENCES employee(id),  -- set once the person has an employee record
  doc_key text NOT NULL,
  file_name text NOT NULL,
  content_type text NOT NULL,
  size_bytes bigint NOT NULL,
  storage_key text NOT NULL,
  uploaded_by text NOT NULL,
  uploaded_at timestamptz NOT NULL DEFAULT now(),
  replaced_at timestamptz                    -- never deleted; a newer upload replaces it
);
CREATE INDEX document_owner_idx ON document(owner_kind, owner_id) WHERE replaced_at IS NULL;
CREATE INDEX document_emp_idx ON document(employee_id);

-- ---------- Rejoin (spec 8.7) ----------
ALTER TABLE employee ADD COLUMN prev_code text;
CREATE TABLE employee_service (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  code text NOT NULL,
  doj date NOT NULL,
  lwd date,
  exit_reason text,
  recorded_at timestamptz NOT NULL DEFAULT now()
);

-- ---------- Nominations (spec 19.2) ----------
CREATE TABLE nomination_scheme (
  id serial PRIMARY KEY,
  name text NOT NULL UNIQUE,
  form text NOT NULL DEFAULT '',
  note text NOT NULL DEFAULT '',
  on_form boolean NOT NULL DEFAULT true,
  types text[] NOT NULL DEFAULT '{Permanent}',
  mandatory boolean NOT NULL DEFAULT true,
  max_people int NOT NULL CHECK (max_people BETWEEN 1 AND 10),
  shares boolean NOT NULL DEFAULT true,       -- shares must add up to 100%
  relations text[] NOT NULL,
  guardian boolean NOT NULL DEFAULT true,     -- an adult guardian for nominees under 18
  due_days int NOT NULL CHECK (due_days BETWEEN 1 AND 180),
  docs jsonb NOT NULL DEFAULT '[]'            -- [{id, label, per: nominee|employee, req}]
);
INSERT INTO nomination_scheme(name, form, note, types, mandatory, max_people, shares, relations, guardian, due_days, docs) VALUES
 ('Provident fund (EPF) nomination','Form 2','Who receives the PF balance and pension if the employee dies in service.','{Permanent}',true,4,true,'{Spouse,Son,Daughter,Father,Mother}',true,30,
  '[{"id":"aadhaar","label":"Nominee Aadhaar copy","per":"nominee","req":true},{"id":"rel","label":"Relationship proof","per":"nominee","req":false},{"id":"form","label":"Signed nomination form","per":"employee","req":true}]'),
 ('Gratuity nomination','Form F','Who receives gratuity if the employee dies before it is paid.','{Permanent}',true,3,true,'{Spouse,Son,Daughter,Father,Mother,Brother,Sister}',true,30,
  '[{"id":"aadhaar","label":"Nominee Aadhaar copy","per":"nominee","req":true},{"id":"form","label":"Signed Form F","per":"employee","req":true}]'),
 ('Group insurance beneficiary','Insurer form','Beneficiary for group term life and personal accident cover.','{Permanent}',true,3,true,'{Spouse,Son,Daughter,Father,Mother,Brother,Sister,Other}',true,15,
  '[{"id":"id","label":"Nominee ID proof","per":"nominee","req":true},{"id":"cheque","label":"Cancelled cheque of the nominee","per":"nominee","req":false}]'),
 ('Medical insurance family members','Enrolment form','Family members covered under the group medical policy.','{Permanent}',false,5,false,'{Spouse,Son,Daughter,Father,Mother}',false,30,
  '[{"id":"id","label":"Aadhaar or birth certificate","per":"nominee","req":true},{"id":"marriage","label":"Marriage certificate (spouse)","per":"nominee","req":false}]');

CREATE TABLE nomination (
  id bigserial PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  scheme_id int NOT NULL REFERENCES nomination_scheme(id),
  status text NOT NULL CHECK (status IN ('Submitted','Verified','Returned')),
  people jsonb NOT NULL,                      -- [{name, rel, dob, share, guardian}]
  submitted_at timestamptz NOT NULL DEFAULT now(),
  decided_by text, decided_at timestamptz, note text
);
CREATE INDEX nomination_emp_idx ON nomination(employee_id, scheme_id);
