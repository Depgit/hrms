-- Round 3, payroll and masters (prototype gaps): master order.
-- Masters, holiday calendars and leave plans are listed in the order they were created, as the
-- prototype lists them. Items seeded together share one created_at (one transaction), so each
-- table gets a sequence number: existing rows are numbered by created_at and then by their stored
-- order; new rows take the next number. Additive only: a new column filled from a new sequence.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['region','state','city','cluster','location','department','grade','designation','shift',
                           'simple_master','holiday_calendar','leave_plan'] LOOP
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = t AND column_name = 'seq') THEN
      EXECUTE format('CREATE SEQUENCE IF NOT EXISTS %I', t || '_seq_seq');
      EXECUTE format('ALTER TABLE %I ADD COLUMN seq bigint', t);
      EXECUTE format('UPDATE %I x SET seq = o.n FROM (SELECT id, row_number() OVER (ORDER BY created_at, ctid) AS n FROM %I) o WHERE x.id = o.id', t, t);
      EXECUTE format('SELECT setval(%L, COALESCE((SELECT max(seq) FROM %I), 0) + 1, false)', t || '_seq_seq', t);
      EXECUTE format('ALTER TABLE %I ALTER COLUMN seq SET DEFAULT nextval(%L)', t, t || '_seq_seq');
      EXECUTE format('ALTER TABLE %I ALTER COLUMN seq SET NOT NULL', t);
      EXECUTE format('ALTER SEQUENCE %I OWNED BY %I.seq', t || '_seq_seq', t);
    END IF;
  END LOOP;
END $$;
