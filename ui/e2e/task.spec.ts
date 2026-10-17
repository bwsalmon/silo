// A task's whole life in the browser: filed through the form, placed on
// a node, its events and token stream followed live, a node-only
// artifact opened through the view-through (and nothing of it left in
// the browser's storage), and its end; the artifact unavailable once
// its node is offline; and a reload mid-stream picking up from where it
// was without showing an event twice.
import { expect, storage, test } from './fixtures';

const diff = `diff --git a/greet.go b/greet.go
--- a/greet.go
+++ b/greet.go
@@ -1,3 +1,3 @@
 package greet
-const Hello = "hello"
+const Hello = "SECRET-ARTIFACT-LINE-4240"
`;

test('file a task, follow it live, view its artifact, see it finish', async ({ world, pageAs, grain }) => {
  const node = await grain({ owner: world.admin, repos: [world.repo] });
  const page = await pageAs(world.member);

  await page.goto('/file');
  await page.getByLabel('Team').click();
  await page.getByRole('option', { name: new RegExp(world.team) }).click();
  await page.getByLabel('Repository').click();
  await page.getByRole('option', { name: world.repo }).click();
  await page.getByLabel('Title').fill('Rename the greeting');
  await page.getByLabel('Prompt').fill('Change **Hello**.');
  await page.getByRole('button', { name: 'File task' }).click();

  await expect(page).toHaveURL(/\/tasks\/[^/]+$/);
  const id = decodeURIComponent(new URL(page.url()).pathname.split('/').pop()!);
  await expect(page.getByRole('heading', { name: 'Rename the greeting' })).toBeVisible();
  const placed = await node.task(id);
  await expect(page.getByText(`${node.id} (${world.admin}), as ${placed.node_task}`)).toBeVisible();
  await expect(page.locator('.MuiChip-label', { hasText: /^running$/ }).first()).toBeVisible();
  await expect(page.getByText('live', { exact: true })).toBeVisible();

  // Lifecycle, then the token stream, as the node sends them.
  await node.emit(id, { type: 'status', status: { phase: 'running', state: 'info', message: 'reading the tree' } });
  await expect(page.getByText('running · info: reading the tree')).toBeVisible();
  await node.emit(id, { type: 'transcript', transcript: { kind: 'text', text: 'I will rename' } });
  await node.emit(id, { type: 'transcript', transcript: { kind: 'text', text: 'the constant now.' } });
  await expect(page.getByText('I will rename')).toBeVisible();
  await expect(page.getByText('the constant now.')).toBeVisible();

  // A node-only artifact: the hub has its reference, the node its bytes.
  await node.artifact('diff-1', diff);
  await node.emit(id, { type: 'artifact', artifact: { kind: 'diff', ref: 'diff-1', size: diff.length } });
  await page.getByRole('button', { name: 'View diff' }).click();
  await expect(page.getByText('SECRET-ARTIFACT-LINE-4240')).toBeVisible();

  // Nothing of it is kept by the browser: no Web Storage, IndexedDB,
  // cache or service worker holds it, while it is open or after.
  const assertNothingStored = async () => {
    const s = await storage(page);
    expect(JSON.stringify(s)).not.toContain('SECRET-ARTIFACT');
    expect(s.indexedDB).toEqual([]);
    expect(s.caches).toEqual({});
    expect(s.serviceWorkers).toEqual([]);
    expect(Object.keys(s.sessionStorage)).toEqual([]);
    expect(Object.keys(s.localStorage)).toEqual([]);
  };
  await assertNothingStored();
  await page.getByRole('button', { name: 'Hide diff' }).click();
  await expect(page.getByText('SECRET-ARTIFACT-LINE-4240')).toHaveCount(0);
  await assertNothingStored();

  await node.emit(id, { type: 'result', result: { exit: { code: 0, reason: 'completed' }, final_text: 'Renamed the greeting.' } });
  await node.finish(id, 'done');
  await expect(page.getByText('completed, exit 0')).toBeVisible();
  await expect(page.getByText('Renamed the greeting.')).toBeVisible();
  await expect(page.locator('.MuiChip-label', { hasText: /^done$/ }).first()).toBeVisible();
  await expect(page.getByText('ended', { exact: true })).toBeVisible();
  await assertNothingStored();
});

test('an artifact whose node is offline is shown as unavailable', async ({ world, pageAs, grain, fileTask }) => {
  const node = await grain({ owner: world.admin, repos: [world.repo] });
  const t = await fileTask(world.member, { repo: world.repo, title: 'Offline artifact', prompt: 'x' });
  await node.task(t.id);
  await node.artifact('log-1', 'the tool log');
  await node.emit(t.id, { type: 'artifact', artifact: { kind: 'tool_log', ref: 'log-1' } });

  const page = await pageAs(world.member);
  await page.goto(`/tasks/${t.id}`);
  await page.getByRole('button', { name: 'View tool_log' }).click();
  await expect(page.getByText('the tool log')).toBeVisible();
  await page.getByRole('button', { name: 'Hide tool_log' }).click();

  await node.offline();
  await page.getByRole('button', { name: 'View tool_log' }).click();
  await expect(page.getByText('Unavailable: the node that holds this tool_log is offline. The hub keeps no copy.')).toBeVisible();
  await expect(page.getByText('the tool log')).toHaveCount(0);
});

test('a reload mid-stream resumes without duplicates', async ({ world, pageAs, grain, fileTask }) => {
  const node = await grain({ owner: world.admin, repos: [world.repo] });
  const t = await fileTask(world.member, { repo: world.repo, title: 'Reload', prompt: 'x' });
  await node.task(t.id);
  const page = await pageAs(world.member);
  await page.goto(`/tasks/${t.id}`);
  for (let i = 1; i <= 3; i++) await node.emit(t.id, { type: 'status', status: { phase: 'running', state: 'info', message: `step ${i}` } });
  await expect(page.getByText('running · info: step 3')).toBeVisible();

  await page.reload();
  await expect(page.getByText('running · info: step 3')).toBeVisible();
  for (let i = 4; i <= 5; i++) await node.emit(t.id, { type: 'status', status: { phase: 'running', state: 'info', message: `step ${i}` } });
  await expect(page.getByText('running · info: step 5')).toBeVisible();

  // A second reload, with the stream's events stored and in the ring.
  await page.reload();
  await node.emit(t.id, { type: 'status', status: { phase: 'running', state: 'info', message: 'step 6' } });
  await expect(page.getByText('running · info: step 6')).toBeVisible();
  await expect(page.getByText('live', { exact: true })).toBeVisible();
  for (let i = 1; i <= 6; i++) await expect(page.getByText(`running · info: step ${i}`, { exact: true })).toHaveCount(1);
});
