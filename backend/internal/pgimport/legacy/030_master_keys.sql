-- Names the app writes itself get a fixed key (owner's request, 11 Nov 2026), so HR can rename them in Masters like
-- any other item: the app finds them by key, not by name. Same list as store.SysKeys (seed sets them on a new
-- database; this sets them on an existing one). What they are used for still cannot change.
ALTER TABLE simple_master ADD COLUMN sys_key text;
CREATE UNIQUE INDEX simple_master_sys_key ON simple_master(sys_key) WHERE sys_key IS NOT NULL;

UPDATE simple_master m SET sys_key = k.key
FROM (VALUES
  ('reason.transfer',        'change_reason',   'Employee update', 'Transfer'),
  ('reason.excess_recovery', 'change_reason',   'Deduction',       'Excess payment recovery'),
  ('reason.absent_hold',     'change_reason',   'Hold',            'Absent without notice'),
  ('sep:Resignation',        'change_reason',   'Separation',      'Resigned'),
  ('sep:Termination',        'change_reason',   'Separation',      'Terminated'),
  ('sep:Absconding',         'change_reason',   'Separation',      'Absconded'),
  ('sep:Contract end',       'change_reason',   'Separation',      'Contract ended'),
  ('sep:Retirement',         'change_reason',   'Separation',      'Retired'),
  ('sep:Death',              'change_reason',   'Separation',      'Death or permanent disability'),
  ('mode.default',           'attendance_mode', '',                'Biometric')
) AS k(key, kind, used_for, name)
WHERE m.id = (SELECT x.id FROM simple_master x WHERE x.kind = k.kind AND COALESCE(x.used_for, '') = k.used_for AND x.name = k.name
              ORDER BY x.created_at LIMIT 1)
  AND NOT EXISTS (SELECT 1 FROM simple_master y WHERE y.sys_key = k.key);
