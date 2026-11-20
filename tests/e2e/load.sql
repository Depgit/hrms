-- 5,000 extra employees modelled on the sample organisation, with pay, IDs, bank and a month of punches.
CREATE TEMP TABLE tpl AS SELECT row_number() OVER (ORDER BY id) AS n, department_id, designation_id, grade, location_id, shift_code, manager_id
  FROM employee WHERE status='Active' AND type='Permanent';
INSERT INTO employee(id, name, type, doj, department_id, designation_id, grade, location_id, shift_code, manager_id, status, verified)
SELECT 'EMP-' || (3000 + g), 'Load person ' || g, CASE WHEN g % 10 = 0 THEN 'Contractual' WHEN g % 17 = 0 THEN 'Retainer' ELSE 'Permanent' END,
  date '2021-01-01' + (g % 1500), t.department_id, t.designation_id, t.grade, t.location_id, t.shift_code, t.manager_id, 'Active', true
FROM generate_series(1, 5000) g JOIN tpl t ON t.n = 1 + (g % (SELECT count(*) FROM tpl));
INSERT INTO gross_history(employee_id, from_month, gross, reason)
SELECT id, to_char(doj,'YYYY-MM'), 15000 + (substr(id,5)::int % 40) * 1000, 'Load' FROM employee WHERE id >= 'EMP-3001' AND id <= 'EMP-8000';
INSERT INTO employee_personal(employee_id, mobile, email, dob, gender)
SELECT id, '98' || lpad(substr(id,5), 8, '0'), lower(id) || '@load.test', date '1990-01-01' + (substr(id,5)::int % 3000), CASE WHEN substr(id,5)::int % 3 = 0 THEN 'Female' ELSE 'Male' END
FROM employee WHERE id >= 'EMP-3001' AND id <= 'EMP-8000' ON CONFLICT DO NOTHING;
INSERT INTO employee_gov_id(employee_id, pan, aadhaar, uan, esic)
SELECT id, 'ABCDE' || lpad(substr(id,5), 4, '0') || 'F', lpad(substr(id,5), 12, '9'), '10' || lpad(substr(id,5), 10, '0'), '31' || lpad(substr(id,5), 8, '0')
FROM employee WHERE id >= 'EMP-3001' AND id <= 'EMP-8000' ON CONFLICT DO NOTHING;
INSERT INTO employee_bank(employee_id, bank_name, account_no, ifsc, from_month, created_by)
SELECT id, 'HDFC Bank', '5010' || lpad(substr(id,5), 8, '0'), 'HDFC0001234', '2020-01', 'Load' FROM employee WHERE id >= 'EMP-3001' AND id <= 'EMP-8000';
-- Last month's punches: on time most days, a late or missing out now and then, Sundays off.
INSERT INTO attendance_day(employee_id, date, in_time, out_time, source, by_name)
SELECT e.id, d::date,
  CASE WHEN (substr(e.id,5)::int + extract(day FROM d)::int) % 23 = 0 THEN '10:20' ELSE '08:55' END,
  CASE WHEN (substr(e.id,5)::int + extract(day FROM d)::int) % 29 = 0 THEN NULL ELSE '18:05' END,
  'Punch machine', 'Load'
FROM employee e, generate_series(date_trunc('month', current_date) - interval '1 month', date_trunc('month', current_date) - interval '1 day', interval '1 day') d
WHERE e.id >= 'EMP-3001' AND e.id <= 'EMP-8000' AND extract(dow FROM d) <> 0
ON CONFLICT DO NOTHING;
ANALYZE;
SELECT (SELECT count(*) FROM employee) AS employees, (SELECT count(*) FROM attendance_day) AS attendance_days;
