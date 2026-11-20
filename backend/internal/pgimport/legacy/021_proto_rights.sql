-- Role rights as in the prototype's role definitions (see access.DefaultRights):
--   * Finance, Finance Head and CHRO view employee dues and claims; Finance also views budgets and structures.
--   * Only HR Admin manages work policies and masters, so Payroll, Payroll Manager, Finance, Finance Head and
--     HRBP Corporate lose the Work policies and Masters pages. Their pages still read the policy and master
--     lists they need.
-- A right is changed only while it still has the old seed level and the owner never changed it in Access
-- control, as in 015. On a new database the roles do not exist yet and the seed adds these levels.
UPDATE role_right rr SET level = v.level
FROM (VALUES ('CHRO', 'emp.assets', 0, 1), ('CHRO', 'pay.claims', 0, 1),
             ('Finance Head', 'emp.assets', 0, 1), ('Finance Head', 'pay.claims', 0, 1),
             ('Finance', 'emp.assets', 0, 1), ('Finance', 'pay.claims', 0, 1),
             ('Finance', 'comp.budget', 0, 1), ('Finance', 'comp.structure', 0, 1),
             ('Payroll', 'pol.policies', 1, 0), ('Payroll Manager', 'pol.policies', 1, 0),
             ('Finance', 'pol.policies', 1, 0), ('Finance Head', 'pol.policies', 1, 0),
             ('HRBP Corporate', 'pol.policies', 1, 0), ('HRBP Corporate', 'mas.org', 1, 0),
             ('HRBP Corporate', 'mas.job', 1, 0)) AS v(role_name, right_id, old_level, level)
WHERE rr.role_name = v.role_name AND rr.right_id = v.right_id AND rr.level = v.old_level
  AND NOT EXISTS (
    SELECT 1 FROM audit_log a WHERE a.action = 'Changed role rights' AND a.entity = 'role' AND a.entity_id = rr.role_name
      AND (a.after -> 'rights' ->> rr.right_id) IS NOT NULL);
