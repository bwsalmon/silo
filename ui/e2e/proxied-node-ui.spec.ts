// Browser E2E tests for opening a deployment's single-tenant Grain UI via
// /deployments/<name> and the Deployment Switcher, theme propagation
// (X-Grain-Hub-UI-Theme: dark), role propagation (owner/admin, member,
// proposer, viewer), and 404 for unauthorized users on private deployments
// (CUJ-11).
import { expect, hubAdmin, test } from './fixtures';

test('proxied single-tenant Grain UI renders via Deployment Switcher and /deployments/<name> with dark theme and 5-role access control (CUJ-11)', async ({
  world,
  hub,
  pageAs,
  grain,
}) => {
  const id = `ws-ui-${world.n}`;
  const uiRepo = `e2e/ws-ui-repo-${world.n}`;
  const adminRoleUser = `admin-ui-${world.n}@e2e.test`;
  const memberUser = world.member;
  const proposerUser = `proposer-ui-${world.n}@e2e.test`;
  const viewerUser = `viewer-ui-${world.n}@e2e.test`;
  const unauthorizedUser = world.outsider;

  for (const u of [adminRoleUser, proposerUser, viewerUser]) {
    await hub.must(hubAdmin, 'PUT', `/v1/users/${encodeURIComponent(u)}`, { sys_admin: false });
  }

  // Create deployment ws-ui-${world.n} as world.admin (owner), configure admin/member/proposer/viewer roles, claim uiRepo, and make it private (anyone_may_propose: false).
  await hub.must(world.admin, 'POST', '/v1/deployments', { id, name: `Workspace UI ${world.n}` });
  await hub.must(world.admin, 'PUT', `/v1/deployments/${id}/repos/${uiRepo}`);
  await hub.must(world.admin, 'PUT', `/v1/deployments/${id}/members/user:${adminRoleUser}`, { role: 'admin' });
  await hub.must(world.admin, 'PUT', `/v1/deployments/${id}/members/user:${memberUser}`, { role: 'member' });
  await hub.must(world.admin, 'PUT', `/v1/deployments/${id}/members/user:${proposerUser}`, { role: 'proposer' });
  await hub.must(world.admin, 'PUT', `/v1/deployments/${id}/members/user:${viewerUser}`, { role: 'viewer' });
  await hub.must(world.admin, 'PUT', `/v1/deployments/${id}/settings`, {
    lost_requeue_after_seconds: 0,
    anyone_may_propose: false,
    require_approval: false,
  });

  // Connect fakegrain with nodeui: true for ws-ui-${world.n}.
  await grain({ owner: world.admin, repos: [uiRepo], id, nodeui: true });

  // 1. As owner, switch Hub theme to dark via theme-mode-select, then open via /deployments/<id> and Deployment Switcher.
  const adminPage = await pageAs(world.admin);
  await adminPage.goto(`/deployments/${id}`);
  await expect(adminPage.getByTestId('deployment-grain-ui-section')).toBeVisible();

  const themeSync = adminPage.waitForResponse(
    (r) => r.url().endsWith('/v1/me/theme') && r.request().method() === 'PUT' && r.status() === 200,
  );
  await adminPage.getByTestId('theme-mode-select').click();
  await adminPage.getByRole('option', { name: 'dark', exact: true }).click();
  await themeSync;

  // Embed Grain UI on /deployments/<id> and verify the embedded frame URL and Open link.
  const expectedNodeUrl = `https://${id.toLowerCase()}.nodes.hub.e2e.test/`;
  await expect(adminPage.getByTestId('open-deployment-grain-ui')).toHaveAttribute('href', expectedNodeUrl);
  await adminPage.getByRole('button', { name: 'Embed Grain UI' }).click();
  await expect(adminPage.getByTestId('deployment-grain-ui-frame')).toHaveAttribute('src', expectedNodeUrl);

  // Use the Deployment Switcher to navigate to the proxied single-tenant Grain UI and verify X-Grain-Hub-UI-Theme header + body.
  const switcherResponsePromise = adminPage.waitForResponse(
    (r) => r.url().startsWith(expectedNodeUrl),
  );
  await adminPage.getByTestId('deployment-switcher').click();
  await adminPage.getByTestId(`deployment-option-${id}`).click();
  const switcherRes = await switcherResponsePromise;
  expect(switcherRes.status()).toBe(200);
  expect(switcherRes.headers()['x-grain-hub-ui-theme']).toBe('dark');
  expect(switcherRes.headers()['x-grain-hub-ui-role']).toBe('owner');
  await expect(adminPage.locator('body')).toContainText(`"node":"${id}"`);
  await expect(adminPage.locator('body')).toContainText(`"viewer":"${world.admin}"`);
  await expect(adminPage.locator('body')).toContainText('"theme":"dark"');
  await expect(adminPage.locator('body')).toContainText('"role":"owner"');
  await expect(adminPage.locator('body')).toContainText('"access":"admin"');

  // 2. Verify admin, member, proposer, and viewer roles through the Deployment Switcher and /deployments/<name> with dark theme.
  for (const [email, expectedViewerBadge, expectedRole, expectedAccess] of [
    [adminRoleUser, 'owner / admin', 'admin', 'admin'],
    [memberUser, 'member', 'member', 'read-write'],
    [proposerUser, 'proposer', 'proposer', 'propose'],
    [viewerUser, 'viewer', 'viewer', 'read-only'],
  ] as const) {
    const rolePage = await pageAs(email);
    await rolePage.goto(`/deployments/${id}`);
    await expect(rolePage.getByTestId('deployment-viewer-role')).toHaveText(expectedViewerBadge);

    const roleThemeSync = rolePage.waitForResponse(
      (r) => r.url().endsWith('/v1/me/theme') && r.request().method() === 'PUT' && r.status() === 200,
    );
    await rolePage.getByTestId('theme-mode-select').click();
    await rolePage.getByRole('option', { name: 'dark', exact: true }).click();
    await roleThemeSync;

    const navResPromise = rolePage.waitForResponse(
      (r) => r.url().startsWith(expectedNodeUrl),
    );
    await rolePage.getByTestId('deployment-switcher').click();
    await rolePage.getByTestId(`deployment-option-${id}`).click();
    const navRes = await navResPromise;
    expect(navRes.status()).toBe(200);
    expect(navRes.headers()['x-grain-hub-ui-theme']).toBe('dark');
    expect(navRes.headers()['x-grain-hub-ui-role']).toBe(expectedRole);
    await expect(rolePage.locator('body')).toContainText(`"viewer":"${email}"`);
    await expect(rolePage.locator('body')).toContainText(`"role":"${expectedRole}"`);
    await expect(rolePage.locator('body')).toContainText(`"access":"${expectedAccess}"`);
    await expect(rolePage.locator('body')).toContainText('"theme":"dark"');
  }

  // 3. An unauthorized user with no role on the private deployment gets 404 on both /deployments/<id> and the proxied node UI.
  const outsiderPage = await pageAs(unauthorizedUser);
  await outsiderPage.goto(`/deployments/${id}`);
  await expect(outsiderPage.getByRole('alert')).toContainText(`No deployment ${id} in your deployments.`);

  const directRes = await outsiderPage.goto(`https://${id.toLowerCase()}.nodes.hub.e2e.test/`);
  expect(directRes?.status()).toBe(404);
});
