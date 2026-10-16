// Markdown, for a task's prompt and the assistant's words: text a
// submitter, a node or an agent wrote, and so never HTML or script in
// the hub's origin (docs/threat-model.md T-TB4-1, INV-15). Raw HTML in
// the text is dropped (skipHtml), a link keeps only an http, https or
// mailto URL or a relative one, opening away from the UI without a
// referrer, and an image is shown as a link to it rather than loaded.
import ReactMarkdown from 'react-markdown';
import { Box } from '@mui/material';
import { mono } from '../theme';

// safeURL is url if it is one a link may follow, or '' if not: a URL
// with whitespace or a control character in it is refused whole, since
// a browser drops some of them ("java\tscript:") before it reads the
// scheme.
export function safeURL(url: string): string {
  if (/[\u0000-\u0020\u007f-\u009f]/.test(url)) return '';
  const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(url);
  if (!scheme) return url;
  return ['http', 'https', 'mailto'].includes(scheme[1].toLowerCase()) ? url : '';
}

export function Markdown({ text }: { text: string }) {
  return (
    <Box
      sx={{
        '& p': { my: 0.75 },
        '& pre': { fontFamily: mono, fontSize: 13, bgcolor: 'grey.100', p: 1, overflowX: 'auto', borderRadius: 1 },
        '& code': { fontFamily: mono, fontSize: 13 },
        wordBreak: 'break-word',
      }}
    >
      <ReactMarkdown
        skipHtml
        urlTransform={safeURL}
        components={{
          a: ({ href, children }) =>
            href ? (
              <a href={href} target="_blank" rel="noopener noreferrer">
                {children}
              </a>
            ) : (
              <span>{children}</span>
            ),
          img: ({ src, alt }) =>
            typeof src === 'string' && src ? (
              <a href={src} target="_blank" rel="noopener noreferrer">
                {alt || src}
              </a>
            ) : (
              <span>{alt}</span>
            ),
        }}
      >
        {text}
      </ReactMarkdown>
    </Box>
  );
}
