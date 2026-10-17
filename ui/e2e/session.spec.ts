// An interactive session (docs/ui.md U7): started from the form, its
// agent waits for a human, the page says so, a member replies, the
// reply reaches the node, and the agent goes on.
import { expect, test } from './fixtures';

test('start a session, see "waiting for you", reply, and see the agent continue', async ({ world, pageAs, grain }) => {
  const node = await grain({ owner: world.admin, repos: [world.repo], interactive: true });
  const page = await pageAs(world.member);

  await page.goto(`/teams/${world.team}/file`);
  await page.getByLabel('Title').fill('Pair on the parser');
  await page.getByLabel('Prompt').fill('Let us pair.');
  await page.getByRole('button', { name: 'Start a session' }).click();
  await expect(page).toHaveURL(/\/tasks\/[^/]+$/);
  const id = decodeURIComponent(new URL(page.url()).pathname.split('/').pop()!);
  await node.task(id);
  await expect(page.getByText('Interactive session')).toBeVisible();

  await node.emit(id, { type: 'awaiting_reply', awaiting_reply: { state: 'waiting', question: 'Which branch should I use?' } });
  await expect(page.getByText('waiting for you', { exact: true })).toBeVisible();
  await expect(page.getByText('Waiting for you.')).toBeVisible();
  await expect(page.getByText('Which branch should I use?').first()).toBeVisible();

  await page.getByLabel('Reply to the session').fill('Use **main**.');
  await page.getByRole('button', { name: 'Send' }).click();
  const m = await node.received(id, 'message');
  expect(m.message).toMatchObject({ text: 'Use **main**.', sender: world.member });

  // The node takes it in, and goes on.
  await node.emit(
    id,
    { type: 'message', message: { id: m.message.id, text: m.message.text } },
    { type: 'awaiting_reply', awaiting_reply: { state: 'resumed' } },
    { type: 'transcript', transcript: { kind: 'text', text: 'Checking out main and carrying on.' } },
  );
  await expect(page.getByText('Checking out main and carrying on.')).toBeVisible();
  await expect(page.getByText('waiting for you', { exact: true })).toHaveCount(0);
  await expect(page.getByText('resumed', { exact: true })).toBeVisible();
});
