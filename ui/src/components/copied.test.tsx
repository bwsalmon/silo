// The components copied from grain (docs/ui.md §7) still do what they
// did there, as the hub uses them. Their safety is content.test.tsx's.
import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import Markdown from './Markdown';
import DiffView from './DiffView';

describe('Markdown', () => {
  it('renders GitHub-flavoured markdown and single line breaks', () => {
    const { container } = render(<Markdown>{'**bold**\nnext\n\n| a | b |\n| - | - |\n| 1 | 2 |\n'}</Markdown>);
    expect(container.querySelector('strong')?.textContent).toBe('bold');
    expect(container.querySelectorAll('br').length).toBe(1);
    expect(container.querySelectorAll('table th').length).toBe(2);
  });

  it('shows an image as a link to it', () => {
    const { container } = render(<Markdown>{'![chart](https://example.com/c.png)'}</Markdown>);
    expect(container.querySelector('img')).toBeNull();
    expect(screen.getByRole('link', { name: 'chart' }).getAttribute('href')).toBe('https://example.com/c.png');
  });
});

const patch = `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -1,2 +1,2 @@
 package x
-var a = 1
+var a = 2
`;

describe('DiffView', () => {
  it('draws a patch as a table per file', () => {
    const { container } = render(<DiffView diff={patch} label="diff" />);
    expect(screen.getByRole('region', { name: 'diff' })).toBeTruthy();
    expect(screen.getByText('x.go')).toBeTruthy();
    expect(container.querySelector('table.diff')).not.toBeNull();
  });

  it('shows text that is not a patch as it is', () => {
    const { container } = render(<DiffView diff="not a patch at all" label="diff" />);
    expect(container.querySelector('pre')?.textContent).toBe('not a patch at all');
  });
});
