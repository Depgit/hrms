// Approximate people matching for the people picker over a list the page already has. The same scoring as the
// server's people search (backend/internal/api/peoplesearch.go), so a name is found the same way everywhere:
//   - every word of the query fits one of the person's words, in any order (name, code with or without its prefix,
//     designation, department, location, city, email);
//   - a word fits exactly, as the start of a word, inside a word (3 letters or more), or within a small typo
//     (1 edit for 4 to 7 letters, 2 from 8; swapping two neighbours is one edit), ranked in that order;
//   - a single word may instead be the initials of the name ("rk" → Rakesh Kumar).

export type Matchable = { id: string; name: string; code?: string; designation?: string; department?: string; location?: string; city?: string; email?: string };

const EXACT = 400, PREFIX = 300, CONTAINS = 200, TYPO = 100, NAME_BUMP = 20;

/** Lower-case words of letters and digits ("ravi.k@saltstayz.com" → ravi, k, saltstayz, com). */
export const words = (s?: string | null) => (s ?? '').toLowerCase().split(/[^\p{L}\p{N}]+/u).filter(Boolean);

type Prepared = { nameWords: string[]; otherWords: string[]; initials: string[]; nameText: string; code: string };
const cache = new WeakMap<object, Prepared>();

function prepare(p: Matchable): Prepared {
  const hit = cache.get(p);
  if (hit) return hit;
  const nw = words(p.name), code = words(p.code ?? p.id);
  const nameWords = [...nw, ...code, ...(code.length > 1 ? [code.join('')] : [])];
  const otherWords = [p.designation, p.department, p.location, p.city, p.email].flatMap(words);
  const ini = nw.map((w) => w[0]).filter((c) => /\p{L}/u.test(c)).join('');
  const initials = ini.length >= 2 ? [ini, ...(ini.length > 2 ? [ini[0] + ini[ini.length - 1]] : [])] : [];
  const out = { nameWords, otherWords, initials, nameText: nw.join(' '), code: code.join('') };
  cache.set(p, out);
  return out;
}

/** Optimal string alignment distance (insert, delete, change, swap two neighbours), max + 1 once above max. */
function dist(a: string, b: string, max: number) {
  if (Math.abs(a.length - b.length) > max) return max + 1;
  let prev2 = new Array(b.length + 1).fill(0), prev = Array.from({ length: b.length + 1 }, (_, j) => j), cur = new Array(b.length + 1).fill(0);
  for (let i = 1; i <= a.length; i++) {
    cur[0] = i;
    let best = cur[0];
    for (let j = 1; j <= b.length; j++) {
      let v = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1));
      if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) v = Math.min(v, prev2[j - 2] + 1);
      cur[j] = v;
      best = Math.min(best, v);
    }
    if (best > max) return max + 1;
    [prev2, prev, cur] = [prev, cur, prev2];
  }
  return prev[b.length];
}

const typos = (n: number) => (n >= 8 ? 2 : n >= 4 ? 1 : 0);

/** How well query word t fits word w (0: not at all). */
export function wordScore(t: string, w: string) {
  if (t === w) return EXACT;
  if (w.startsWith(t)) return PREFIX;
  if (t.length >= 3 && w.includes(t)) return CONTAINS;
  const k = typos(t.length);
  if (!k) return 0;
  let best = k + 1;
  for (let l = t.length - k; l <= t.length + k; l++) if (l >= 1 && l <= w.length) best = Math.min(best, dist(t, w.slice(0, l), k));
  return best > k ? 0 : TYPO - 10 * best;
}

/** The person's score for a query: 0 when they do not fit. */
export function personScore(query: string, p: Matchable) {
  const toks = words(query);
  if (!toks.length) return 0;
  const x = prepare(p);
  let total = 0;
  for (const t of toks) {
    let best = 0;
    for (const w of x.nameWords) { const s = wordScore(t, w); if (s && s + NAME_BUMP > best) best = s + NAME_BUMP; }
    for (const w of x.otherWords) { const s = wordScore(t, w); if (s > best) best = s; }
    if (!best) { total = 0; break; }
    total += best;
  }
  if (toks.length === 1 && toks[0].length >= 2) {
    x.initials.forEach((ini, i) => {
      const s = ini === toks[0] ? EXACT - 50 : i === 0 && ini.startsWith(toks[0]) ? PREFIX - 50 : 0;
      if (s > total) total = s;
    });
  }
  if (!total) return 0;
  const q = toks.join(' ');
  if (x.nameText.startsWith(q)) total += 50;
  if (x.code === toks.join('')) total += 500;
  return total;
}

const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

/** The people that fit the query, best first (then by name and code). */
export function rankPeople<T extends Matchable>(query: string, people: T[]): T[] {
  return people.map((p) => ({ p, s: personScore(query, p) })).filter((x) => x.s > 0)
    .sort((a, b) => b.s - a.s || cmp(a.p.name.toLowerCase(), b.p.name.toLowerCase()) || cmp(a.p.id, b.p.id))
    .map((x) => x.p);
}
