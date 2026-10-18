// An accessibility smoke check with axe on each page, against WCAG 2.x
// A and AA: each page loaded with something on it, as a team admin, so
// that every control the page has is drawn.
import AxeBuilder from '@axe-core/playwright';
import type { Page } from '@playwright/test';
import { expect, test } from './fixtures';

async function check(page: Page, what: string): Promise<string[]> {
  // A label or chip mid-transition has a colour of neither end.
  await page.waitForFunction(() => document.getAnimations().length === 0);
  const r = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
  // WCAG 1.4.3 exempts an inactive control from contrast, which axe
  // cannot tell of a disabled field's label: MUI marks those.
  for (const v of r.violations) if (v.id === 'color-contrast') v.nodes = v.nodes.filter((n) => !n.html.includes('Mui-disabled'));
  return r.violations
    .filter((v) => v.nodes.length > 0)
    .map((v) => `${what}: ${v.id} (${v.impact}): ${v.help}\n    ${v.nodes.map((n) => `${n.target.join(' ')}: ${n.failureSummary}`).join('\n    ')}`);
}

test('every page passes axe', async ({ world, newWorld, hub, pageAs, grain, fileTask }) => {
  const recipient = `https://github.com/e2e/grain-a11y-${world.n}`;
  const folder = `https://github.com/${world.repo}/tree/main/tasks`;
  const processUrl = `https://github.com/${world.repo}/blob/main/.grain/processes/ci.process.grain.md`;
  const node = await grain({
    owner: world.admin,
    repos: [world.repo],
    interactive: true,
    nodeui: true,
    recipients: [recipient],
    folders: [folder],
    grainfiles: [processUrl],
  });
  await hub.pushGitHub(world.repo, 'main', {
    'docs/spec.md': '# Spec\n',
    'tasks/01-a11y.task.grain.md': [
      '---',
      'grainfile: v1',
      'title: Accessible Grainfile task',
      `recipient: ${recipient}`,
      `requestor: Alice <${world.admin}>`,
      'draft: false',
      'not_before: 2020-01-01T00:00:00Z',
      'references:',
      '  - /docs/spec.md',
      '  - https://github.com/external/other/blob/main/notes.md',
      '---',
      '# Accessible task',
    ].join('\n'),
    '.grain/processes/ci.process.grain.md': [
      '---',
      'grainfile: v1',
      'title: Accessible CI process',
      `recipient: ${recipient}`,
      `requestor: Alice <${world.admin}>`,
      'draft: true',
      'gate: true',
      'trigger_with:',
      '  - /tasks/01-a11y.task.grain.md',
      'when:',
      '  schedule:',
      '    cron: "0 * * * *"',
      '    timezone: UTC',
      '  push:',
      '    branches: [main]',
      '---',
      '# Accessible process',
    ].join('\n'),
  });
  const t = await fileTask(world.member, { repo: world.repo, title: 'Accessible', prompt: 'A **prompt** with `code`.' });
  await node.task(t.id);
  await node.artifact('diff-a11y', 'diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n');
  await node.emit(
    t.id,
    { type: 'status', status: { phase: 'running', state: 'info', message: 'working' } },
    { type: 'transcript', transcript: { kind: 'text', text: 'Some *text*.' } },
    { type: 'approval_request', approval_request: { id: 'req-a11y', tool: 'git_push', message: 'push' } },
    { type: 'artifact', artifact: { kind: 'diff', ref: 'diff-a11y' } },
  );
  const session = await fileTask(world.member, { repo: world.repo, kind: 'interactive', title: 'Session', prompt: 'pair' });
  await node.task(session.id);
  await node.emit(session.id, { type: 'awaiting_reply', awaiting_reply: { state: 'waiting', question: 'Which?' } });
  const other = await newWorld();
  await fileTask(other.member, { repo: world.repo, title: 'A proposal', prompt: 'please' });

  const page = await pageAs(world.admin);
  const pages: [string, string, () => Promise<unknown>][] = [
    ['the backlog', '/', () => page.getByRole('link', { name: 'Accessible' }).waitFor()],
    ["a team's backlog", `/teams/${world.team}`, () => page.getByRole('link', { name: 'Accessible' }).waitFor()],
    ['the teams', '/teams', () => page.getByText(world.team).first().waitFor()],
    ["a team's members", `/teams/${world.team}/members`, () => page.getByText(world.member).first().waitFor()],
    ['the file form', `/teams/${world.team}/file`, () => expect(page.getByRole('combobox', { name: 'Repository' })).not.toHaveAttribute('aria-disabled', 'true')],
    ["a team's proposals", `/teams/${world.team}/proposals`, () => page.getByText('A proposal').waitFor()],
    ['the grainfiles', '/grainfiles', () => page.getByTestId('grainfile-detail').getByRole('heading', { name: 'Accessible CI process' }).waitFor()],
    ['the nodes', '/nodes', () => page.getByRole('row', { name: new RegExp(node.id) }).getByRole('link', { name: recipient }).waitFor()],
    ['a task', `/tasks/${t.id}`, () => page.getByRole('button', { name: 'Allow' }).waitFor()],
    ['a session', `/tasks/${session.id}`, () => page.getByLabel('Reply to the session').waitFor()],
    ['a task not found', '/tasks/nope', () => page.getByText('No task nope in your teams.').waitFor()],
    ['no page', '/no/such/page', () => page.getByText('No page at').waitFor()],
  ];
  const found: string[] = [];
  for (const [what, path, ready] of pages) {
    await page.goto(path);
    await ready();
    found.push(...(await check(page, what)));
  }
  // A task Grainfile selected in the detail view on /grainfiles.
  await page.goto('/grainfiles');
  await page.locator('table').first().getByText('Accessible Grainfile task').click();
  await page.getByTestId('grainfile-detail').getByRole('heading', { name: 'Accessible Grainfile task' }).waitFor();
  found.push(...(await check(page, 'the grainfiles with a task selected')));
  // A task with its artifact open.
  await page.goto(`/tasks/${t.id}`);
  await page.getByRole('button', { name: 'View diff' }).click();
  await page.getByRole('button', { name: 'Hide diff' }).waitFor();
  found.push(...(await check(page, 'a task with its diff open')));
  expect(found).toEqual([]);
});
