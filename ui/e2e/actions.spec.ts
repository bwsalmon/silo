// What a human does to a task: cancel and requeue it; approve or reject
// a non-member's proposal; answer its agent's approval requests.
import { expect, test } from './fixtures';

const stateChip = (page: import('@playwright/test').Page, state: string) =>
  page.locator('.MuiChip-label', { hasText: new RegExp(`^${state}$`) }).first();

test('cancel a running task, and requeue a lost one', async ({ world, pageAs, grain, fileTask }) => {
  const node = await grain({ owner: world.admin, repos: [world.repo] });
  const doomed = await fileTask(world.member, { repo: world.repo, title: 'To cancel', prompt: 'x' });
  await node.task(doomed.id);

  const page = await pageAs(world.member);
  await page.goto(`/tasks/${doomed.id}`);
  await expect(stateChip(page, 'running')).toBeVisible();
  await page.getByLabel('Reason to cancel (optional)').fill('changed my mind');
  await page.getByRole('button', { name: 'Cancel task' }).click();
  await page.getByRole('button', { name: 'Confirm cancel' }).click();
  await expect(stateChip(page, 'cancelled')).toBeVisible();
  await expect(page.getByText('changed my mind')).toBeVisible();
  expect((await node.received(doomed.id, 'cancel')).type).toBe('cancel');

  // A task whose node goes away is lost; requeued, another node takes it.
  const lost = await fileTask(world.member, { repo: world.repo, title: 'To requeue', prompt: 'x' });
  await node.task(lost.id);
  await node.offline();
  await page.goto(`/tasks/${lost.id}`);
  await expect(stateChip(page, 'lost')).toBeVisible({ timeout: 30_000 });
  const second = await grain({ owner: world.admin, repos: [world.repo] });
  await page.getByRole('button', { name: 'Requeue' }).click();
  await second.task(lost.id);
  await page.reload();
  await expect(stateChip(page, 'running')).toBeVisible();
  await expect(page.getByText(`${second.id} (${world.admin})`)).toBeVisible();
});

test("a non-member's proposal is approved, and another rejected, by the owning team", async ({ world, newWorld, pageAs, fileTask, hub }) => {
  // The outsider is in a team of their own, not the one owning the
  // repository. The form offers only the filer's own teams' repositories,
  // so the outsider proposes through the API, as the CLI would.
  const theirs = await newWorld();
  const outsider = theirs.member;
  const a = await fileTask(outsider, { repo: world.repo, title: `proposal A ${world.n}`, prompt: 'please' });
  const b = await fileTask(outsider, { repo: world.repo, title: `proposal B ${world.n}`, prompt: 'please' });
  expect(a.state).toBe('proposed');

  const out = await pageAs(outsider);
  await out.goto(`/tasks/${a.id}`);
  await expect(stateChip(out, 'proposed')).toBeVisible();
  await expect(out.getByRole('button', { name: 'Approve' })).toHaveCount(0);

  const page = await pageAs(world.member);
  await page.goto(`/teams/${world.team}/proposals`);
  await expect(page.getByText(`proposal A ${world.n}`)).toBeVisible();
  await page.getByRole('link', { name: `proposal A ${world.n}` }).click();
  await page.getByRole('button', { name: 'Approve' }).click();
  await expect(stateChip(page, 'queued')).toBeVisible();
  await expect(page.getByText('Approved by', { exact: true }).locator('..')).toContainText(world.member);

  await page.goto(`/tasks/${b.id}`);
  await page.getByLabel(`Reason to reject proposal B ${world.n}`).fill('not now');
  await page.getByRole('button', { name: 'Reject' }).click();
  await page.getByRole('button', { name: 'Confirm reject' }).click();
  await expect(stateChip(page, 'rejected')).toBeVisible();
  expect((await hub.must(outsider, 'GET', `/v1/tasks/${b.id}`)).state).toBe('rejected');
  expect((await hub.must(outsider, 'GET', `/v1/tasks/${a.id}`)).state).toBe('queued');
});

test('an approval request answered allow, and another deny', async ({ world, pageAs, grain, fileTask }) => {
  const node = await grain({ owner: world.admin, repos: [world.repo] });
  const t = await fileTask(world.member, { repo: world.repo, title: 'Asks', prompt: 'x' });
  await node.task(t.id);
  const page = await pageAs(world.admin);
  await page.goto(`/tasks/${t.id}`);

  await node.emit(t.id, { type: 'approval_request', approval_request: { id: 'req-1', tool: 'git_push', message: 'push to main' } });
  await expect(page.getByText('push to main')).toBeVisible();
  await page.getByRole('button', { name: 'Allow' }).click();
  const allow = await node.received(t.id, 'approve');
  expect(allow).toMatchObject({ request_id: 'req-1', decision: 'allow', answered_by: world.admin });
  await expect(page.getByRole('button', { name: 'Allow' })).toHaveCount(0);

  await node.emit(t.id, { type: 'approval_request', approval_request: { id: 'req-2', tool: 'rm', message: 'remove the tree' } });
  await expect(page.getByText('remove the tree')).toBeVisible();
  await page.getByLabel('Reason for req-2').fill('too risky');
  await page.getByRole('button', { name: 'Deny' }).click();
  await node.waitFor('the deny', (s) => s.tasks.find((x) => x.id === t.id)!.received.some((m) => m.request_id === 'req-2'));
  const deny = (await node.state()).tasks.find((x) => x.id === t.id)!.received.find((m) => m.request_id === 'req-2');
  expect(deny).toMatchObject({ decision: 'deny', reason: 'too risky' });
});
