// Theme: follows the system until the sun / moon icon picks light or dark; the choice is remembered.
const KEY = 'hrms.theme';

export function applySavedTheme() {
  try {
    const t = localStorage.getItem(KEY);
    if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  } catch { /* storage unavailable: follow the system */ }
}

export function toggleTheme() {
  const r = document.documentElement;
  const dark = r.dataset.theme ? r.dataset.theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
  r.dataset.theme = dark ? 'light' : 'dark';
  try { localStorage.setItem(KEY, r.dataset.theme); } catch { /* not remembered */ }
}

/** Whether the page is dark now: the chosen theme, else the system's. */
export function isDark() {
  const t = document.documentElement.dataset.theme;
  return t ? t === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
}
