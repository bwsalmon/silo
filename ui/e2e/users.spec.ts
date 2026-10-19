// Browser E2E tests for the Accepted Users allowlist, dynamic sys_admin
// promotion/demotion, the last-sys-admin guard, and 403 denial for
// unaccepted users (CUJ-1).
import { expect, hubAdmin, test } from './fixtures';

test('accepted users allowlist, dynamic sys_admin promotion/demotion, last-sys-admin guard, and 403 denial for unaccepted users (CUJ-1)', async ({
  world,
  pageAs,
}) => {
  const newUserEmail = `newhire-${world.n}@e2e.test`;
  const strangerEmail = `stranger-${world.n}@e2e.test`;

  // 1. An unaccepted user (stranger@e2e.test) is denied access (403) in the browser.
  const strangerPage = await pageAs(strangerEmail);
  await strangerPage.goto('/deployments');
  await expect(strangerPage.getByRole('alert').first()).toContainText(/not on the (hub )?accepted users list|403|forbidden/i);

  // 2. Sign in as hubAdmin, toggle Admin mode on, and navigate to /users (and /deployments).
  const adminPage = await pageAs(hubAdmin);
  await adminPage.goto('/users');
  await adminPage.getByRole('switch', { name: 'Admin mode' }).check();
  await expect(adminPage.getByTestId('admin-mode-marker')).toBeVisible();

  const usersSection = adminPage.getByTestId('accepted-users-section');
  await expect(usersSection).toBeVisible();

  // Add a new accepted user email.
  await usersSection.getByLabel('User email').fill(newUserEmail);
  await usersSection.getByRole('button', { name: 'Add accepted user' }).click();

  const newUserRow = usersSection.getByTestId(`user-row-${newUserEmail}`);
  await expect(newUserRow).toBeVisible();
  const sysAdminCheckbox = usersSection.getByRole('checkbox', { name: `Sys admin for ${newUserEmail}` });
  await expect(sysAdminCheckbox).not.toBeChecked();

  // Verify the newly added user can now sign in and access the Hub UI.
  const newHirePage = await pageAs(newUserEmail);
  await newHirePage.goto('/deployments');
  await expect(newHirePage.getByRole('heading', { name: 'Deployments' })).toBeVisible();

  // Promote the user to sys_admin and verify the checkbox updates.
  await sysAdminCheckbox.click();
  await expect(sysAdminCheckbox).toBeChecked();

  // Demote the user back to regular accepted user.
  await sysAdminCheckbox.click();
  await expect(sysAdminCheckbox).not.toBeChecked();

  // Verify the last-sys-admin guard when the sole remaining sys_admin attempts to demote or remove themselves.
  const selfSysAdminCheckbox = usersSection.getByRole('checkbox', { name: `Sys admin for ${hubAdmin}` });
  await expect(selfSysAdminCheckbox).toBeChecked();
  await selfSysAdminCheckbox.click();
  await expect(usersSection.getByRole('alert')).toContainText(/cannot remove or demote the last hub sys admin/i);
  await expect(selfSysAdminCheckbox).toBeChecked();

  const selfRow = usersSection.getByTestId(`user-row-${hubAdmin}`);
  await selfRow.getByRole('button', { name: 'Remove user' }).click();
  await selfRow.getByRole('button', { name: `Remove ${hubAdmin}` }).click();
  await expect(usersSection.getByRole('alert').last()).toContainText(/cannot remove or demote the last hub sys admin/i);
  await expect(selfRow).toBeVisible();
});
