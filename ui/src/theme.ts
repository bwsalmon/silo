// The UI's theme. Light only, and set in code: the UI keeps no
// preference in the browser's storage (docs/ui.md §2).
import { createTheme } from '@mui/material';

export const theme = createTheme({
  palette: {
    mode: 'light',
    primary: { main: '#1b5e20' },
    secondary: { main: '#5d4037' },
    background: { default: '#f7f7f5' },
  },
  typography: {
    fontFamily: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
  },
  shape: { borderRadius: 6 },
  components: {
    MuiPaper: { defaultProps: { variant: 'outlined' } },
  },
});

export const mono = 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace';
