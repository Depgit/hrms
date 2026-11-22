// The light / dark switch in the top bar: a sun while the page is dark (click for light), a moon while it is light.
import { useState } from 'react';
import { Icon } from './icons';
import { isDark, toggleTheme } from './theme';

export function ThemeToggle() {
  const [dark, setDark] = useState(isDark);
  const label = dark ? 'Switch to light theme' : 'Switch to dark theme';
  return (
    <button className="iconbtn" aria-label={label} title={label} onClick={() => { toggleTheme(); setDark(isDark()); }}>
      <Icon name={dark ? 'sun' : 'moon'} />
    </button>
  );
}
