-- UI round 2, Employee dues (inventory, uniform, deductions): asset name or model, uniform kits,
-- the uniform issue date, deduction waivers and deduction proofs. Additive only.

-- An asset's name or model, for example "Dell Latitude 5440" (the type is the catalogue item).
ALTER TABLE asset ADD COLUMN IF NOT EXISTS name text NOT NULL DEFAULT '';

-- The date a uniform was handed over (the record's own time stays in at). Partial returns are
-- kept per line in lines[].ret.
ALTER TABLE uniform_issue ADD COLUMN IF NOT EXISTS issued_on date;

-- Uniform kits: a named set of items and quantities used to fill an issue or a stock dialog.
-- Edited in Setup; made inactive, never deleted.
CREATE TABLE IF NOT EXISTS inv_kit (
  id serial PRIMARY KEY,
  name text NOT NULL UNIQUE,
  lines jsonb NOT NULL DEFAULT '{}',                    -- {"<item id>": quantity}
  status text NOT NULL DEFAULT 'Active' CHECK (status IN ('Active','Inactive')),
  by_name text NOT NULL DEFAULT 'System',
  at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO inv_kit(name, lines)
SELECT k.name, COALESCE((SELECT jsonb_object_agg(i.id::text, q.qty) FROM inv_item i JOIN (VALUES ('Shirt',2),('Trousers',2),('Name badge',1),('Shoes',1)) q(name, qty) ON q.name=i.name
  WHERE i.kind='uniform' AND i.status='Active'), '{}') FROM (VALUES ('Front office kit')) k(name)
ON CONFLICT (name) DO NOTHING;
INSERT INTO inv_kit(name, lines)
SELECT k.name, COALESCE((SELECT jsonb_object_agg(i.id::text, q.qty) FROM inv_item i JOIN (VALUES ('Shirt',2),('Trousers',2),('Apron',2),('Shoes',1)) q(name, qty) ON q.name=i.name
  WHERE i.kind='uniform' AND i.status='Active'), '{}') FROM (VALUES ('Housekeeping kit')) k(name)
ON CONFLICT (name) DO NOTHING;

-- A deduction's balance waived after approval: the instalments not yet deducted are voided in the
-- ledger, and who waived how much and why is kept here.
CREATE TABLE IF NOT EXISTS deduction_waiver (
  request_id text PRIMARY KEY REFERENCES approval_request(id),
  amount numeric(12,2) NOT NULL,
  note text NOT NULL,
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now()
);

-- Proof attached to a pay request (a deduction) by Payroll: documents with owner kind 'approval',
-- owner "<employee id>/<key>". The owner-kind check is widened in place, keeping every kind
-- already allowed (other migrations may have added their own).
DO $$
DECLARE def text;
BEGIN
  SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint WHERE conname='document_owner_kind_check' AND conrelid='document'::regclass;
  IF def IS NOT NULL AND position('''approval''' IN def) = 0 AND position('ARRAY[' IN def) > 0 THEN
    EXECUTE 'ALTER TABLE document DROP CONSTRAINT document_owner_kind_check';
    EXECUTE 'ALTER TABLE document ADD CONSTRAINT document_owner_kind_check ' || replace(def, 'ARRAY[', 'ARRAY[''approval''::text, ');
  END IF;
END $$;
