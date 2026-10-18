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
