-- Sprint 7: separation case tracker, clearance, exit interview, letters, full and final (spec 18);
-- inventory, assets and uniform (spec 19.1).

CREATE SEQUENCE separation_seq START 1;
CREATE TABLE separation (
  id text PRIMARY KEY,                                   -- SEP-n
  employee_id text NOT NULL REFERENCES employee(id),
  type text NOT NULL CHECK (type IN ('Resignation','Termination','Absconding','Contract end','Retirement','Death')),
  status text NOT NULL DEFAULT 'Open' CHECK (status IN ('Open','Closed','Cancelled')),
  given_on date NOT NULL,                                -- resignation received, or the day HR started the case
  lwd date,
  notice_days int NOT NULL DEFAULT 0,
  notice_option text,                                    -- termination: pay | serve | none
  reason text NOT NULL DEFAULT '',
  note text NOT NULL DEFAULT '',
  request_id text REFERENCES approval_request(id),
  hold_id text,
  show_cause jsonb NOT NULL DEFAULT '[]',               -- absconding letters [{n, at, by}]
  absent_days int,
  rehire text,
  started_by text NOT NULL, started_at timestamptz NOT NULL DEFAULT now(),
  closed_by text, closed_at timestamptz, close_note text
);
CREATE UNIQUE INDEX one_open_separation ON separation(employee_id) WHERE status = 'Open';

-- Clearance items (configurable, made inactive not deleted). auto: assets | exit.
CREATE TABLE clearance_item (
  id serial PRIMARY KEY,
  owner text NOT NULL CHECK (owner IN ('Reporting manager','HRBP','Finance')),
  name text NOT NULL,
  auto text CHECK (auto IN ('assets','exit')),
  sort int NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text
);
INSERT INTO clearance_item(owner, name, auto, sort) VALUES
 ('Reporting manager','Work handed over',NULL,1), ('Reporting manager','Open tasks, keys and passwords handed over',NULL,2),
 ('HRBP','Assets and uniform returned to the store','assets',3), ('HRBP','ID card and access card collected',NULL,4), ('HRBP','Exit interview done','exit',5),
 ('Finance','Advances and loans checked for recovery',NULL,6), ('Finance','Claims and cash imprest settled',NULL,7);

CREATE TABLE clearance (
  sep_id text NOT NULL REFERENCES separation(id),
  item_id int NOT NULL REFERENCES clearance_item(id),
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now(), note text,
  PRIMARY KEY (sep_id, item_id)
);

CREATE TABLE exit_interview (
  sep_id text PRIMARY KEY REFERENCES separation(id),
  main_reason text NOT NULL,
  ratings jsonb NOT NULL,                               -- Manager, Work and shifts, Pay and benefits, Growth, Workplace and team: 1-5
  recommend text NOT NULL CHECK (recommend IN ('Yes','Maybe','No')),
  comments text NOT NULL DEFAULT '',
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sep_letter (
  id bigserial PRIMARY KEY,
  sep_id text NOT NULL REFERENCES separation(id),
  kind text NOT NULL CHECK (kind IN ('Show-cause notice','Termination letter','Relieving letter','Experience letter','No dues certificate')),
  body text NOT NULL,
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);

-- Full and final: lines computed from the record, approved through p_fnf; on approval the
-- brought-forward lines are voided and the settlement written to the ledger.
CREATE TABLE fnf (
  id text PRIMARY KEY,                                   -- the p_fnf approval request id
  sep_id text REFERENCES separation(id),
  employee_id text NOT NULL REFERENCES employee(id),
  lwd date NOT NULL,
  pay_ym char(7) NOT NULL,
  lines jsonb NOT NULL,
  net numeric(12,2) NOT NULL,
  status text NOT NULL DEFAULT 'Pending approval' CHECK (status IN ('Pending approval','Approved','Rejected','Cancelled')),
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);

-- ---------- Inventory (spec 19.1) ----------
-- Catalogue: uniform items (counted) and asset types (serial numbered).
CREATE TABLE inv_item (
  id serial PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('uniform','asset')),
  name text NOT NULL,
  price numeric(10,2) NOT NULL DEFAULT 0,
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  inactive_since date, inactive_reason text,
  UNIQUE (kind, name)
);
INSERT INTO inv_item(kind, name, price) VALUES
 ('uniform','Shirt',450), ('uniform','Trousers',550), ('uniform','Apron',250), ('uniform','Name badge',120), ('uniform','Shoes',900),
 ('asset','Laptop',0), ('asset','SIM card',0), ('asset','Handset',0), ('asset','Tablet',0);

-- Stores: the central store and one per HRBP (store id 'central' or 'hrbp:<employee id>').
CREATE TABLE inv_store (
  id text PRIMARY KEY,
  name text NOT NULL,
  owner_id text REFERENCES employee(id)
);
INSERT INTO inv_store(id, name) VALUES ('central','Central store');

-- Every movement: Addition, Transfer in, Return (+); Issue, Transfer out, Write-off (-).
CREATE TABLE inv_move (
  id bigserial PRIMARY KEY,
  store_id text NOT NULL REFERENCES inv_store(id),
  item_id int NOT NULL REFERENCES inv_item(id),
  kind text NOT NULL CHECK (kind IN ('Addition','Transfer in','Return','Issue','Transfer out','Write-off')),
  qty int NOT NULL CHECK (qty > 0),
  employee_id text REFERENCES employee(id),
  ref text,
  serials text[] NOT NULL DEFAULT '{}',
  note text NOT NULL DEFAULT '',
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inv_move_store_idx ON inv_move(store_id, item_id);

-- Assets with serial numbers; never deleted.
CREATE TABLE asset (
  id bigserial PRIMARY KEY,
  item_id int NOT NULL REFERENCES inv_item(id),
  serial text NOT NULL UNIQUE,
  store_id text NOT NULL REFERENCES inv_store(id),
  status text NOT NULL DEFAULT 'Available' CHECK (status IN ('Available','Issued','Damaged','Lost')),
  employee_id text REFERENCES employee(id),
  issued_at date,
  note text NOT NULL DEFAULT ''
);

-- Uniform issued to an employee; recovery and refund are ledger lines (source UNI-n).
CREATE SEQUENCE uniform_seq START 1;
CREATE TABLE uniform_issue (
  id text PRIMARY KEY,
  employee_id text NOT NULL REFERENCES employee(id),
  store_id text NOT NULL REFERENCES inv_store(id),
  lines jsonb NOT NULL,                                  -- [{itemId, name, qty, price}]
  amount numeric(10,2) NOT NULL,
  recover boolean NOT NULL,
  returned boolean NOT NULL DEFAULT false,
  by_name text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);

-- Uniform recovery split and refund delay (spec 15.2: 50% / 50%, refund after 6 months).
ALTER TABLE company_setting ADD COLUMN uniform jsonb NOT NULL DEFAULT '{"firstPct":50,"refundAfter":6}';
