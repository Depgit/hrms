-- My view, part A: the tax regime is chosen once per financial year.
-- Additive only, safe on a database with real declarations:
--   * regime_locked_at is when the employee's regime choice for the year was saved. While it is set, the employee
--     cannot change the regime for that year. A declaration already submitted counts as a choice made, so it is
--     locked from its submission; a draft stays open until it is next saved.
--   * Payroll or HR Admin can unlock it for one person, with a reason (kept here and in the audit log).
ALTER TABLE tax_declaration ADD COLUMN IF NOT EXISTS regime_locked_at timestamptz;
ALTER TABLE tax_declaration ADD COLUMN IF NOT EXISTS regime_unlocked_at timestamptz;
ALTER TABLE tax_declaration ADD COLUMN IF NOT EXISTS regime_unlocked_by text;
ALTER TABLE tax_declaration ADD COLUMN IF NOT EXISTS regime_unlock_reason text;

UPDATE tax_declaration SET regime_locked_at = COALESCE(submitted_at, updated_at)
WHERE status = 'Declared' AND regime_locked_at IS NULL;
