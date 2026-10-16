// A unified diff, rendered with react-diff-view. Text that does not
// parse as a diff is shown as it is.
import { Box } from '@mui/material';
import { Diff, Hunk, parseDiff, type FileData } from 'react-diff-view';
import 'react-diff-view/style/index.css';
import { mono } from '../theme';

export function parseFiles(text: string): FileData[] {
  try {
    return parseDiff(text).filter((f) => f.hunks.length > 0);
  } catch {
    return [];
  }
}

export function DiffPane({ text }: { text: string }) {
  const files = parseFiles(text);
  if (files.length === 0) return <PlainText text={text} />;
  return (
    <Box sx={{ '& .diff': { fontFamily: mono, fontSize: 12 }, overflowX: 'auto' }}>
      {files.map((f, i) => (
        <Box key={`${f.oldPath}-${f.newPath}-${i}`} sx={{ mb: 2 }}>
          <Box sx={{ fontFamily: mono, fontSize: 13, fontWeight: 600, py: 0.5 }}>
            {f.type === 'delete' ? f.oldPath : f.newPath}
          </Box>
          <Diff viewType="unified" diffType={f.type} hunks={f.hunks}>
            {(hunks) => hunks.map((h) => <Hunk key={h.content} hunk={h} />)}
          </Diff>
        </Box>
      ))}
    </Box>
  );
}

export function PlainText({ text }: { text: string }) {
  return (
    <Box component="pre" sx={{ fontFamily: mono, fontSize: 12, whiteSpace: 'pre-wrap', wordBreak: 'break-word', m: 0 }}>
      {text}
    </Box>
  );
}
