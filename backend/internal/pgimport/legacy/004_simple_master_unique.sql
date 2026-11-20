-- The unique rule on simple_master treated an empty used_for as never equal, so re-running the
-- reference seed added identical copies of employment types, attendance modes and qualifications.
-- Remove only exact, untouched copies (same kind, name and status), keeping the first of each,
-- then make the rule hold for empty used_for too.
DELETE FROM simple_master a USING simple_master b
 WHERE a.kind = b.kind AND a.name = b.name AND a.used_for IS NULL AND b.used_for IS NULL
   AND a.status = b.status AND a.inactive_since IS NULL AND b.inactive_since IS NULL
   AND (a.created_at, a.id::text) > (b.created_at, b.id::text);
ALTER TABLE simple_master DROP CONSTRAINT simple_master_kind_name_used_for_key;
ALTER TABLE simple_master ADD CONSTRAINT simple_master_kind_name_used_for_key UNIQUE NULLS NOT DISTINCT (kind, name, used_for);
