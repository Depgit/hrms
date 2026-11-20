-- Round 2, onboarding and compensation (prototype gaps B and G). Additive only: two new tables,
-- seeded rows that are inserted only when missing, and rights raised only where the owner never set them.

-- Rules by employment type (Compensation, Salary structure, "Rules by employment type"). They were
-- constants in the code; now HR can change them and the payroll engine reads these rows. Seeded with
-- exactly the values the code used, so nothing changes until someone edits them.
CREATE TABLE IF NOT EXISTS comp_type_rule (
  type text PRIMARY KEY CHECK (type IN ('Permanent','Retainer','Contractual')),
  rules jsonb NOT NULL,
  by_name text NOT NULL DEFAULT 'System',
  at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO comp_type_rule(type, rules) VALUES
  ('Permanent',   '{"statutory":true,"lwf":true,"bonus":true,"grat":true,"plEnc":true,"variable":true,"ded":"all","leave":true,"docs":true}'),
  ('Contractual', '{"statutory":false,"lwf":false,"bonus":true,"grat":false,"plEnc":false,"variable":true,"ded":"none","leave":false,"docs":false}'),
  ('Retainer',    '{"statutory":false,"lwf":false,"bonus":false,"grat":false,"plEnc":false,"variable":false,"ded":"tds","leave":false,"docs":false}')
ON CONFLICT (type) DO NOTHING;

-- "Ask candidate" on an offer document: kept with the draft (owner_id DRAFT-…) and moved to the
-- offer id when the offer is raised, so the "Requested from candidate" pill survives a reload.
CREATE TABLE IF NOT EXISTS offer_doc_request (
  owner_id text NOT NULL,
  doc_key text NOT NULL,
  by_id text NOT NULL,
  by_name text NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (owner_id, doc_key)
);

-- Rights the prototype gives: HRBP Corporate sees salary structures; CHRO and Finance Head see salary
-- structures and budgets. A missing row is added. A row the original seed left at 0 is raised only if
-- the owner never changed that right in Access control (no "Changed role rights" entry naming it), so a
-- level the owner set is never lowered or overwritten. On a new database the roles do not exist yet
-- and the seed adds these levels.
INSERT INTO role_right(role_name, right_id, level)
SELECT v.role_name, v.right_id, v.level
FROM (VALUES ('HRBP Corporate', 'comp.structure', 1), ('CHRO', 'comp.structure', 1), ('CHRO', 'comp.budget', 1),
             ('Finance Head', 'comp.structure', 1), ('Finance Head', 'comp.budget', 1)) AS v(role_name, right_id, level)
JOIN role r ON r.name = v.role_name
ON CONFLICT (role_name, right_id) DO UPDATE SET level = EXCLUDED.level
WHERE role_right.level = 0 AND NOT EXISTS (
  SELECT 1 FROM audit_log a WHERE a.action = 'Changed role rights' AND a.entity = 'role' AND a.entity_id = role_right.role_name
    AND (a.after -> 'rights' ->> role_right.right_id) IS NOT NULL);
