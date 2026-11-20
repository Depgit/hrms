import PersonPicker from './PersonPicker';

export type EmpHit = { id: string; name: string; type?: string; department?: string; location?: string; status?: string; designation?: string };

/** Search an employee in the caller's scope and pick one: the people picker (PersonPicker) in server mode, kept
 *  under its old contract (the picked row, or null when cleared). `filter` is added to the search query. */
export default function EmpSearch({ value, onPick, filter = '', placeholder = 'Search employee by name or code', label = 'Employee' }:
  { value?: EmpHit | null; onPick: (e: EmpHit | null) => void; filter?: string; placeholder?: string; label?: string }) {
  return <PersonPicker value={value?.id ?? ''} selected={value ?? null} onChange={(_, p) => onPick(p as EmpHit | null)} search={filter} placeholder={placeholder} label={label}
    detail={(p) => [p.type, p.designation, p.department, p.location].filter(Boolean).join(' · ')} style={{ maxWidth: 460 }} />;
}
