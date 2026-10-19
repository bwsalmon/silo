// Browser E2E tests for self-service deployments, canonical Hub URLs,
// 5-role member management, repository claims, submission policies, and
// host replacement / blank install cutover (CUJ-1, CUJ-2, CUJ-12).
import { expect, hubAdmin, test } from './fixtures';

test('self-service deployment creation, canonical Hub URL, and conflict error on duplicate creation (CUJ-1)', async ({
  world,
  hub,
  pageAs,
}) => {
  const deployId = `payments-prod-${world.n}`;
  const page = await pageAs(world.member);

  await page.goto('/deployments');
  await page.getByLabel('Deployment ID').fill(deployId);
  await page.getByLabel('Display name (optional)').fill('Payments Production');
  await page.getByRole('button', { name: 'Create deployment' }).click();

  await expect(page).toHaveURL(new RegExp(`/deployments/${deployId}$`));
  await expect(page.getByRole('heading', { name: `Payments Production (${deployId})` })).toBeVisible();
  await expect(page.getByTestId('deployment-viewer-role')).toHaveText(/owner \/ admin/);
  await expect(page.getByTestId('canonical-deployment-url')).toHaveText(`${hub.url}/deployments/${deployId}`);

  // Attempting to create a second deployment with the same name shows a conflict error in the UI.
  await page.goto('/deployments');
  await page.getByLabel('Deployment ID').fill(deployId);
  await page.getByRole('button', { name: 'Create deployment' }).click();
  await expect(page.getByRole('alert')).toContainText(/already exists/i);
});

test('deployment member roles (admin, member, proposer, viewer), repo claims, submission policies, and proposal approval (CUJ-1, CUJ-12)', async ({
  world,
  hub,
  pageAs,
  grain,
  fileTask,
}) => {
  const deployId = `payments-roles-${world.n}`;
  const adminUser = `admin2-${world.n}@e2e.test`;
  const devUser = `dev-${world.n}@e2e.test`;
  const proposerUser = `prop-${world.n}@e2e.test`;
  const viewerUser = `view-${world.n}@e2e.test`;
  const claimedRepo = `e2e/payments-${world.n}`;

  for (const u of [adminUser, devUser, proposerUser, viewerUser]) {
    await hub.must(hubAdmin, 'PUT', `/v1/users/${encodeURIComponent(u)}`, { sys_admin: false });
  }

  const ownerPage = await pageAs(world.member);
  await ownerPage.goto('/deployments');
  await ownerPage.getByLabel('Deployment ID').fill(deployId);
  await ownerPage.getByRole('button', { name: 'Create deployment' }).click();
  await expect(ownerPage).toHaveURL(new RegExp(`/deployments/${deployId}$`));

  // Add users with admin, member, proposer, and viewer roles through the UI.
  for (const [email, role] of [
    [adminUser, 'admin'],
    [devUser, 'member'],
    [proposerUser, 'proposer'],
    [viewerUser, 'viewer'],
  ] as const) {
    await ownerPage.getByLabel('Principal').fill(`user:${email}`);
    await ownerPage.getByLabel('New member role').click();
    await ownerPage.getByRole('option', { name: role, exact: true }).click();
    await ownerPage.getByRole('button', { name: 'Add member' }).click();
    await expect(ownerPage.getByLabel(`Role of user:${email}`).first()).toBeVisible();
  }

  // Change a member's role via the role selector (viewer -> proposer -> viewer).
  await ownerPage.getByLabel(`Role of user:${viewerUser}`).first().click();
  await ownerPage.getByRole('option', { name: 'proposer', exact: true }).click();
  await expect(ownerPage.getByLabel(`Role of user:${viewerUser}`).first()).toContainText('proposer');
  await ownerPage.getByLabel(`Role of user:${viewerUser}`).first().click();
  await ownerPage.getByRole('option', { name: 'viewer', exact: true }).click();
  await expect(ownerPage.getByLabel(`Role of user:${viewerUser}`).first()).toContainText('viewer');

  // Claim a repository for the deployment (requires .grain/hub-claim proof on default branch for non-sys-admin).
  await ownerPage.getByLabel('Repository').fill(claimedRepo);
  await ownerPage.getByRole('button', { name: 'Assign' }).click();
  await expect(ownerPage.getByRole('alert')).toContainText(/\.grain\/hub-claim/i);

  await hub.pushGitHub(claimedRepo, 'main', { '.grain/hub-claim': `${deployId}\n` });
  await ownerPage.getByRole('button', { name: 'Assign' }).click();
  await expect(ownerPage.getByRole('cell', { name: claimedRepo })).toBeVisible();

  // Toggle anyone_may_propose and require_approval in Settings and save.
  const anyoneCheckbox = ownerPage.getByLabel('Anyone may propose');
  const requireApprovalCheckbox = ownerPage.getByLabel('Require two-person approval for member tasks');
  await expect(anyoneCheckbox).toBeChecked();
  await anyoneCheckbox.uncheck();
  await requireApprovalCheckbox.check();
  await ownerPage.getByRole('button', { name: 'Save' }).click();

  await ownerPage.reload();
  await expect(ownerPage.getByLabel('Anyone may propose')).not.toBeChecked();
  await expect(ownerPage.getByLabel('Require two-person approval for member tasks')).toBeChecked();

  // Re-enable anyone_may_propose and disable require_approval so members can approve proposals directly.
  await ownerPage.getByLabel('Anyone may propose').check();
  await ownerPage.getByLabel('Require two-person approval for member tasks').uncheck();
  await ownerPage.getByRole('button', { name: 'Save' }).click();

  const node = await grain({ owner: world.member, repos: [claimedRepo], id: deployId });

  // A proposer submits a proposal in the browser that lands in proposed state.
  const proposerPage = await pageAs(proposerUser);
  await proposerPage.goto(`/teams/${deployId}/file`);
  await proposerPage.getByLabel('Title').fill('Audit webhook retries');
  await proposerPage.getByLabel('Prompt').fill('Verify exponential backoff in webhook delivery.');
  await proposerPage.getByRole('button', { name: 'File task' }).click();
  await expect(proposerPage).toHaveURL(/\/tasks\/.+/);
  await expect(proposerPage.getByText('proposed', { exact: true }).first()).toBeVisible();

  // An accepted non-member (world.outsider) can also submit a proposal against the claimed repo when anyone_may_propose is enabled.
  const outsiderProposal = await fileTask(world.outsider, {
    repo: claimedRepo,
    title: 'Update SDK docs',
    prompt: 'Document retry headers.',
  });
  const outsiderPage = await pageAs(world.outsider);
  await outsiderPage.goto(`/tasks/${outsiderProposal.id}`);
  await expect(outsiderPage.getByText('proposed', { exact: true }).first()).toBeVisible();

  // A deployment member approves the proposer's task from the Proposals UI.
  const devPage = await pageAs(devUser);
  await devPage.goto(`/teams/${deployId}/proposals`);
  const proposalRow = devPage.getByRole('row', { name: /Audit webhook retries/ });
  await expect(proposalRow).toBeVisible();
  await proposalRow.getByRole('button', { name: 'Approve' }).click();
  await expect(proposalRow).toHaveCount(0);

  // Verify the approved task is dispatched to the connected fakegrain host.
  await node.waitFor('approved proposal dispatched to host', (s) => s.tasks.length >= 1);
});

test('host replacement / blank install cutover (replace_host: true) disconnects old host and reconnects fresh host (CUJ-2)', async ({
  world,
  hub,
  pageAs,
  grain,
  fileTask,
}) => {
  const deployId = `payments-cutover-${world.n}`;
  const repo = `e2e/cutover-${world.n}`;

  const page = await pageAs(world.member);
  await page.goto('/deployments');
  await page.getByLabel('Deployment ID').fill(deployId);
  await page.getByRole('button', { name: 'Create deployment' }).click();
  await expect(page).toHaveURL(new RegExp(`/deployments/${deployId}$`));

  await hub.pushGitHub(repo, 'main', { '.grain/hub-claim': `${deployId}\n` });
  await page.getByLabel('Repository').fill(repo);
  await page.getByRole('button', { name: 'Assign' }).click();
  await expect(page.getByRole('cell', { name: repo })).toBeVisible();

  // Connect initial fakegrain host with an active task.
  const oldHost = await grain({ owner: world.member, repos: [repo], id: deployId });
  const activeTask = await fileTask(world.member, {
    repo,
    title: 'In-flight migration task',
    prompt: 'Run schema migration step 1',
  });
  await oldHost.task(activeTask.id);

  await page.reload();
  await expect(page.getByText('online', { exact: true }).first()).toBeVisible();

  // Mint a replacement token via the Replace / Reconnect Host UI flow (replace_host: true).
  const hostSection = page.getByTestId('deployment-host-section');
  await hostSection.getByLabel('Replace existing host and reclaim in-flight tasks').check();
  await hostSection.getByRole('button', { name: 'Mint connection token' }).click();

  await expect(hostSection.getByText(/shown only this once/i)).toBeVisible();
  await expect(hostSection.getByTestId('replace-host-summary')).toContainText(/1 in-flight task\(s\) reclaimed/i);
  const newToken = (await hostSection.getByTestId('minted-token').textContent())!.trim();
  expect(newToken.length).toBeGreaterThan(20);

  // Token is shown once: clicking Done hides it.
  await hostSection.getByRole('button', { name: 'Done' }).click();
  await expect(hostSection.getByTestId('minted-token')).toHaveCount(0);

  // Verify old fakegrain host is disconnected and rejected on reconnect.
  await oldHost.waitFor(
    `${deployId} old host disconnected and rejected`,
    (s) => !s.online && s.failures >= 1 && /401|bad status/.test(s.last_error),
  );

  // Connect a fresh fakegrain host with the newly minted replacement token.
  const newHost = await grain({ owner: world.member, repos: [repo], id: deployId, token: newToken });
  await newHost.task(activeTask.id);

  await page.reload();
  await expect(page.getByText('online', { exact: true }).first()).toBeVisible();
});
