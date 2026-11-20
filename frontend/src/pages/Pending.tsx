// Modules from later sprints (spec 22.1). Shown honestly as not built, with what they will hold.

export const MODULES: Record<string, { sprint: number; items: string[] }> = {
};

export default function Pending({ name, card = true }: { name: string; card?: boolean }) {
  const m = MODULES[name];
  const body = (
    <div className="empty">
      <h3>{name} is not built yet</h3>
      <p>It arrives in sprint {m?.sprint ?? '—'} of the build plan. Nothing here uses sample data, so this page stays empty until then.</p>
      {m && <ul>{m.items.map((i) => <li key={i}>{i}</li>)}</ul>}
    </div>
  );
  return card ? <div className="card">{body}</div> : body;
}
