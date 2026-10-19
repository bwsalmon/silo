// Node tokens: a team admin mints one, which is shown once, connects a
// node with it, and revokes it, and the node is disconnected and stays
// out.
import { expect, test } from './fixtures';

test('a team admin mints a node token, shown once, and revoking it disconnects the node', async ({ world, pageAs, grain }) => {
  const id = `ws-mint-${world.n}`;
  const page = await pageAs(world.admin);
  await page.goto('/nodes');
  await page.getByLabel('Node ID').fill(id);
  await page.getByRole('button', { name: 'Mint token' }).click();
  await expect(page.getByText('it is shown only this once')).toBeVisible();
  const token = (await page.getByTestId('minted-token').textContent())!.trim();
  expect(token.length).toBeGreaterThan(20);

  const node = await grain({ owner: world.admin, repos: [world.repo], id, token });
  const row = page.getByRole('row', { name: new RegExp(id) });

  // Shown once: gone once dismissed, and not on a fresh load, where the
  // node is listed online and its token by ID only.
  await page.getByRole('button', { name: 'Done' }).click();
  await expect(page.getByTestId('minted-token')).toHaveCount(0);
  await page.reload();
  await expect(page.getByText(token)).toHaveCount(0);
  await expect(page.getByTestId('minted-token')).toHaveCount(0);
  await expect(row.filter({ hasText: 'online' })).toBeVisible();

  const tokenRow = page.getByRole('row').filter({ has: page.getByRole('button', { name: 'Revoke' }) }).filter({ hasText: id });
  await tokenRow.getByRole('button', { name: 'Revoke' }).click();
  await page.getByRole('button', { name: `Revoke ${id}’s token` }).click();
  await expect(tokenRow).toHaveCount(0);

  // The hub drops the node at once, and turns every reconnect away.
  const before = (await node.state()).connects;
  await node.waitFor(`${id} disconnected and turned away`, (s) => !s.online && s.failures >= 3 && /401|bad status/.test(s.last_error));
  expect((await node.state()).connects).toBe(before);
  await page.reload();
  await expect(page.getByRole('row', { name: new RegExp(id) }).filter({ hasText: 'online' })).toHaveCount(0);
});

test('connected Grain deployments display their recipient URLs, subscribed repositories/folders/processes, and tunneled local UI links on /nodes', async ({
  world,
  pageAs,
  grain,
}) => {
  const id = `ws-sub-${world.n}`;
  const recipient = `https://github.com/e2e/grain-deploy-${world.n}`;
  const folder = `https://github.com/${world.repo}/tree/main/.grain/processes`;
  const processUrl = `https://github.com/${world.repo}/blob/main/.grain/processes/ci.process.md`;

  await grain({
    owner: world.admin,
    repos: [world.repo],
    id,
    nodeui: true,
    recipients: [recipient],
    folders: [folder],
    grainfiles: [processUrl],
  });

  const page = await pageAs(world.admin);
  await page.goto('/nodes');

  const row = page.getByRole('row', { name: new RegExp(id) });
  await expect(row.filter({ hasText: 'online' })).toBeVisible();
  await expect(row.getByRole('link', { name: recipient })).toHaveAttribute('href', recipient);
  await expect(row.getByText(world.repo, { exact: true })).toBeVisible();
  await expect(row.getByText(`folder: ${world.repo}/tree/main/.grain/processes`)).toBeVisible();
  await expect(row.getByRole('link', { name: `${world.repo}/blob/main/.grain/processes/ci.process.md` })).toHaveAttribute(
    'href',
    processUrl,
  );
  await expect(row.getByRole('link', { name: 'Open Grain UI' })).toHaveAttribute(
    'href',
    `https://${id.toLowerCase()}.nodes.hub.e2e.test/`,
  );
});

test('deployment switcher, theme switcher, and unified inbox across deployments', async ({
  world,
  newWorld,
  pageAs,
  grain,
  fileTask,
  hub,
}) => {
  const id = `ws-inbox-${world.n}`;
  const node = await grain({
    owner: world.admin,
    repos: [world.repo],
    id,
    interactive: true,
    nodeui: true,
  });

  // File a session awaiting a reply, and a proposal from another team's member.
  const session = await fileTask(world.member, {
    repo: world.repo,
    kind: 'interactive',
    title: `Inbox session ${world.n}`,
    prompt: 'Pair with me.',
  });
  await node.task(session.id);
  await node.emit(session.id, {
    type: 'awaiting_reply',
    awaiting_reply: { state: 'waiting', question: 'Should we ship this?' },
  });

  const other = await newWorld();
  const proposal = await fileTask(other.member, {
    repo: world.repo,
    title: `Inbox proposal ${world.n}`,
    prompt: 'Proposed change.',
  });

  const page = await pageAs(world.member);
  await page.goto('/');

  // Unified Inbox is the default landing page and shows the actionable count badge.
  await expect(page.getByRole('heading', { name: 'Unified Inbox' })).toBeVisible();
  await expect(page.getByTestId('inbox-count-badge')).toBeVisible();
  await expect(page.getByTestId('inbox-deployment-selector')).toContainText(id);

  // Open in <node> Grain UI link is shown on the inbox card and on /tasks/{id}.
  const sessionCard = page.getByTestId(`inbox-item-${session.id}`);
  await expect(sessionCard.getByTestId('open-in-node-ui')).toHaveAttribute(
    'href',
    `https://${id.toLowerCase()}.nodes.hub.e2e.test/`,
  );

  // Inline quick action: approve the proposal directly from the Unified Inbox.
  const proposalCard = page.getByTestId(`inbox-item-${proposal.id}`);
  await proposalCard.getByRole('button', { name: 'Approve' }).click();
  await expect
    .poll(async () => (await hub.must(world.member, 'GET', `/v1/tasks/${proposal.id}`)).state)
    .toBe('queued');

  // Deployment Switcher lists the deployment with its inferred team and role chip.
  await page.getByTestId('deployment-switcher').click();
  const opt = page.getByTestId(`deployment-option-${id}`);
  await expect(opt).toContainText(id);
  await expect(opt).toContainText(world.team);
  await expect(opt).toContainText('member');
  await page.getByRole('option', { name: 'Grain Hub (All deployments)' }).click();

  // Theme Switcher switches to dark and syncs resolved theme to /v1/me.
  await page.getByTestId('theme-mode-select').click();
  await page.getByRole('option', { name: 'dark' }).click();
  await expect.poll(async () => (await hub.must(world.member, 'GET', '/v1/me')).theme).toBe('dark');

  // TaskPage also renders the Open in <node> Grain UI link.
  await page.goto(`/tasks/${session.id}`);
  await expect(page.getByTestId('open-in-node-ui')).toHaveAttribute(
    'href',
    `https://${id.toLowerCase()}.nodes.hub.e2e.test/`,
  );
});
