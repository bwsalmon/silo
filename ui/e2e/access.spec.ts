// What a member sees: their own team's backlog and nothing of another
// team's, whose task URLs answer not found.
import { expect, test } from './fixtures';

test("a member sees only their team's backlog", async ({ world, newWorld, pageAs, fileTask }) => {
  const other = await newWorld();
  await fileTask(world.member, { repo: world.repo, title: `ours ${world.n}`, prompt: 'x' });
  await fileTask(other.member, { repo: other.repo, title: `theirs ${other.n}`, prompt: 'x' });

  const page = await pageAs(world.member);
  await page.goto('/');
  await expect(page.getByRole('banner').getByText(world.member)).toBeVisible();
  await expect(page.getByRole('link', { name: `ours ${world.n}` })).toBeVisible();
  await expect(page.getByRole('link', { name: `theirs ${other.n}` })).toHaveCount(0);

  // The team selector offers only their team; the team's page lists the
  // same task.
  await page.getByLabel('Team').click();
  await expect(page.getByRole('option', { name: world.team })).toBeVisible();
  await expect(page.getByRole('option', { name: other.team })).toHaveCount(0);
  await page.getByRole('option', { name: world.team }).click();
  await expect(page).toHaveURL(new RegExp(`/teams/${world.team}$`));
  await expect(page.getByRole('link', { name: `ours ${world.n}` })).toBeVisible();

  // Asking for the other team's backlog by URL shows none of it.
  await page.goto(`/teams/${other.team}`);
  await expect(page.getByRole('link', { name: `theirs ${other.n}` })).toHaveCount(0);
});

test("another team's task URL gives not found", async ({ world, newWorld, pageAs, fileTask, hub }) => {
  const other = await newWorld();
  const theirs = await fileTask(other.member, { repo: other.repo, title: 'Not yours', prompt: 'the secret prompt' });
  expect((await hub.api(world.member, 'GET', `/v1/tasks/${theirs.id}`)).status).toBe(404);

  const page = await pageAs(world.member);
  await page.goto(`/tasks/${theirs.id}`);
  await expect(page.getByText(`No task ${theirs.id} in your teams.`)).toBeVisible();
  await expect(page.getByText('the secret prompt')).toHaveCount(0);
  await expect(page.getByText('Not yours')).toHaveCount(0);
  // And the same for one that does not exist at all.
  await page.goto('/tasks/no-such-task');
  await expect(page.getByText('No task no-such-task in your teams.')).toBeVisible();
});
