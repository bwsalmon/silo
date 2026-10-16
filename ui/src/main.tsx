import { StrictMode, useMemo, type ReactNode } from 'react';
import { createRoot } from 'react-dom/client';
import createCache from '@emotion/cache';
import { CacheProvider } from '@emotion/react';
import { CssBaseline, ThemeProvider, useMediaQuery } from '@mui/material';
import { App } from './App';
import { darkTheme, darkThemeReducedMotion, lightTheme, lightThemeReducedMotion } from './theme';

// MUI's styles are <style> elements inserted at run time. The hub's CSP
// allows no inline style but one carrying the nonce it wrote into this
// page (hub/ui), so they carry it too. Under `npm run dev` the page has
// the placeholder, and Vite's server sends no CSP.
const nonce = document.querySelector<HTMLMetaElement>('meta[name="csp-nonce"]')?.content;
const cache = createCache({ key: 'css', nonce: nonce || undefined });

// Theme follows the browser's own light/dark and reduced-motion
// settings, read live; there is no in-app switch, since remembering one
// would need the browser's storage (docs/ui.md §2).
function Theme({ children }: { children: ReactNode }) {
  const dark = useMediaQuery('(prefers-color-scheme: dark)', { noSsr: true });
  const reduceMotion = useMediaQuery('(prefers-reduced-motion: reduce)', { noSsr: true });
  const theme = useMemo(
    () => (dark ? (reduceMotion ? darkThemeReducedMotion : darkTheme) : reduceMotion ? lightThemeReducedMotion : lightTheme),
    [dark, reduceMotion],
  );
  return (
    <ThemeProvider theme={theme}>
      <CssBaseline />
      {children}
    </ThemeProvider>
  );
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <CacheProvider value={cache}>
      <Theme>
        <App />
      </Theme>
    </CacheProvider>
  </StrictMode>,
);
