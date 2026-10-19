import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import createCache from '@emotion/cache';
import { CacheProvider } from '@emotion/react';
import { App } from './App';

// MUI's styles are <style> elements inserted at run time. The hub's CSP
// allows no inline style but one carrying the nonce it wrote into this
// page (hub/ui), so they carry it too. Under `npm run dev` the page has
// the placeholder, and Vite's server sends no CSP.
const nonce = document.querySelector<HTMLMetaElement>('meta[name="csp-nonce"]')?.content;
const cache = createCache({ key: 'css', nonce: nonce || undefined });

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <CacheProvider value={cache}>
      <App />
    </CacheProvider>
  </StrictMode>,
);
