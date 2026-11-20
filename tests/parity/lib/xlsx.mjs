// A minimal .xlsx reader (Node built-ins only): enough to compare the cell contents of the API's Excel
// downloads. Styles, widths and document properties (which carry a creation time) are ignored on purpose.
import { inflateRawSync } from 'node:zlib';

/** unzip returns a Map of entry name → Buffer for the entries `want` accepts. */
export function unzip(buf, want = () => true) {
  // End of central directory: the last 0x06054b50 signature (the comment is at most 64 KiB).
  let eocd = -1;
  for (let i = buf.length - 22; i >= Math.max(0, buf.length - 22 - 65536); i--) {
    if (buf.readUInt32LE(i) === 0x06054b50) { eocd = i; break; }
  }
  if (eocd < 0) throw new Error('not a zip file');
  const count = buf.readUInt16LE(eocd + 10);
  let p = buf.readUInt32LE(eocd + 16);
  const out = new Map();
  for (let n = 0; n < count; n++) {
    if (buf.readUInt32LE(p) !== 0x02014b50) throw new Error('bad central directory');
    const method = buf.readUInt16LE(p + 10);
    const csize = buf.readUInt32LE(p + 20);
    const nameLen = buf.readUInt16LE(p + 28), extraLen = buf.readUInt16LE(p + 30), commentLen = buf.readUInt16LE(p + 32);
    const local = buf.readUInt32LE(p + 42);
    const name = buf.toString('utf8', p + 46, p + 46 + nameLen);
    p += 46 + nameLen + extraLen + commentLen;
    if (!want(name)) continue;
    const lnameLen = buf.readUInt16LE(local + 26), lextraLen = buf.readUInt16LE(local + 28);
    const start = local + 30 + lnameLen + lextraLen;
    const data = buf.subarray(start, start + csize);
    out.set(name, method === 0 ? Buffer.from(data) : method === 8 ? inflateRawSync(data) : Buffer.alloc(0));
  }
  return out;
}

const unescapeXml = (s) => s.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&apos;/g, "'")
  .replace(/&#(\d+);/g, (_, d) => String.fromCodePoint(+d)).replace(/&#x([0-9a-f]+);/gi, (_, h) => String.fromCodePoint(parseInt(h, 16))).replace(/&amp;/g, '&');

// Text of an <si> or <is> element: every <t> run joined (rich text keeps its runs).
const textOf = (xml) => [...xml.matchAll(/<t(?:\s[^>]*)?>([\s\S]*?)<\/t>|<t(?:\s[^>]*)?\/>/g)].map((m) => unescapeXml(m[1] ?? '')).join('');

const colIndex = (ref) => {
  const letters = ref.replace(/\d+$/, '');
  let n = 0;
  for (const ch of letters) n = n * 26 + (ch.charCodeAt(0) - 64);
  return n - 1;
};

/** readXlsx returns { sheets: { <name>: [[cell, …], …] } } with every cell as its text ('' when empty). */
export function readXlsx(buf) {
  const files = unzip(buf, (n) => n === 'xl/workbook.xml' || n === 'xl/sharedStrings.xml' || n === 'xl/_rels/workbook.xml.rels' || /^xl\/worksheets\/[^/]+\.xml$/.test(n));
  const shared = [];
  const ss = files.get('xl/sharedStrings.xml');
  if (ss) for (const m of ss.toString('utf8').matchAll(/<si>([\s\S]*?)<\/si>|<si\/>/g)) shared.push(textOf(m[1] ?? ''));
  // sheet name → worksheet file, through the workbook relationships
  const rels = new Map();
  const relXml = files.get('xl/_rels/workbook.xml.rels')?.toString('utf8') ?? '';
  for (const m of relXml.matchAll(/<Relationship\b([^>]*)\/?>/g)) {
    const id = /\bId="([^"]+)"/.exec(m[1])?.[1], target = /\bTarget="([^"]+)"/.exec(m[1])?.[1];
    if (id && target) rels.set(id, target.replace(/^\/?(xl\/)?/, 'xl/'));
  }
  const sheets = {};
  const wb = files.get('xl/workbook.xml')?.toString('utf8') ?? '';
  for (const m of wb.matchAll(/<sheet\b([^>]*)\/?>/g)) {
    const name = unescapeXml(/\bname="([^"]*)"/.exec(m[1])?.[1] ?? '');
    const rid = /\br:id="([^"]+)"/.exec(m[1])?.[1];
    const xml = files.get(rels.get(rid) ?? '')?.toString('utf8');
    if (xml == null) { sheets[name] = null; continue; }
    const rows = [];
    for (const r of xml.matchAll(/<row\b([^>]*?)(?:\/>|>([\s\S]*?)<\/row>)/g)) {
      const rn = +(/\br="(\d+)"/.exec(r[1])?.[1] ?? rows.length + 1);
      const cells = [];
      for (const c of (r[2] ?? '').matchAll(/<c\b([^>]*?)(?:\/>|>([\s\S]*?)<\/c>)/g)) {
        const ref = /\br="([A-Z]+\d+)"/.exec(c[1])?.[1];
        const t = /\bt="([^"]+)"/.exec(c[1])?.[1];
        const inner = c[2] ?? '';
        const v = /<v>([\s\S]*?)<\/v>/.exec(inner)?.[1];
        let text = '';
        if (t === 's') text = shared[+v] ?? '';
        else if (t === 'inlineStr') text = textOf(inner);
        else if (v != null) text = unescapeXml(v);
        const ci = ref ? colIndex(ref) : cells.length;
        while (cells.length < ci) cells.push('');
        cells[ci] = text;
      }
      while (rows.length < rn - 1) rows.push([]);
      rows[rn - 1] = cells;
    }
    sheets[name] = rows;
  }
  return { sheets };
}
