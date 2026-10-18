// Grainfiles discovery, filtering, and detail inspection in the browser
// against `silo hub serve`, a local Git + GitHub simulator, and a
// connected fake Grain node with active subscriptions.
import { expect, hubAdmin, storage, test } from './fixtures';

test('navigate to /grainfiles from the Grain Hub header, filter indexed Grainfiles, and inspect .task.md and .process.md details', async ({
  world,
  hub,
  pageAs,
  grain,
}) => {
  const repo2 = `e2e/docs-${world.n}`;
  await hub.must(hubAdmin, 'PUT', `/v1/teams/${world.team}/repos/${repo2}`);

  const recipientAlpha = `https://github.com/e2e/grain-alpha-${world.n}`;
  const recipientBeta = `https://github.com/e2e/grain-beta-${world.n}`;

  const node = await grain({
    owner: world.admin,
    repos: [world.repo, repo2],
    recipients: [recipientAlpha],
    folders: [`https://github.com/${world.repo}/tree/main/tasks`],
    grainfiles: [`https://github.com/${world.repo}/blob/main/.grain/processes/gate.process.md`],
  });

  await hub.pushGitHub(repo2, 'main', {
    'processes/lint.process.md': [
      '---',
      `title: Lint documentation ${world.n}`,
      `recipient: ${recipientBeta}`,
      `requestor: Bob <${world.member}>`,
      'draft: false',
      'gate: false',
      'on:',
      '  push:',
      '    branches: [main]',
      '---',
      '# Lint documentation',
      '',
      'Verify markdown formatting.',
    ].join('\n'),
  });

  await hub.pushGitHub(world.repo, 'main', {
    'docs/spec.md': '# Architecture Spec\n',
    'tasks/01-migrate.task.md': [
      '---',
      `title: Migrate billing schema ${world.n}`,
      `recipient: ${recipientAlpha}`,
      `requestor: Alice <${world.admin}>`,
      'draft: false',
      'not_before: 2020-01-01T00:00:00Z',
      'references:',
      '  - /docs/spec.md',
      '  - https://github.com/external/other/blob/main/external-notes.md',
      'depends_on:',
      `  - https://github.com/${repo2}/blob/main/processes/lint.process.md`,
      'trigger_with:',
      '  - /.grain/processes/gate.process.md',
      '---',
      '# Migrate billing schema',
      '',
      'Run the billing database migration.',
    ].join('\n'),
    'tasks/02-draft.task.md': [
      '---',
      `title: Draft rollout plan ${world.n}`,
      `recipient: ${recipientAlpha}`,
      `requestor: ${recipientBeta}`,
      'draft: true',
      'not_before: 2099-01-01T00:00:00Z',
      '---',
      '# Draft rollout plan',
      '',
      'Work in progress rollout plan.',
    ].join('\n'),
    '.grain/processes/gate.process.md': [
      '---',
      `title: Continuous verification gate ${world.n}`,
      `recipient: ${recipientAlpha}`,
      `requestor: Alice <${world.admin}>`,
      'draft: false',
      'gate: true',
      'references:',
      '  - /docs/spec.md',
      '  - https://github.com/external/other/blob/main/runbook.md',
      'depends_on:',
      `  - https://github.com/${repo2}/blob/main/processes/lint.process.md`,
      'trigger_with:',
      `  - https://github.com/${repo2}/blob/main/processes/lint.process.md`,
      'on:',
      '  schedule:',
      '    cron: "0 * * * *"',
      '    timezone: UTC',
      '  push:',
      '    branches: [main]',
      '    paths: ["src/**"]',
      '  pull_request:',
      '    branches: [main]',
      '  merge:',
      '    branches: [main]',
      '  manual: {}',
      '  scenario:',
      '    description: Verify staging canary',
      '---',
      '# Continuous verification gate',
      '',
      'Verify every push and PR.',
    ].join('\n'),
  });

  // Push a commit touching src/** so the process is triggered with a process_triggered event.
  await hub.pushGitHub(world.repo, 'main', {
    'src/service.go': 'package src\n',
  });

  const page = await pageAs(world.admin);
  await page.goto('/');
  await expect(page.getByRole('banner').getByText('Grain Hub')).toBeVisible();
  await page.getByRole('banner').getByRole('button', { name: 'Grainfiles' }).click();
  await expect(page).toHaveURL(/\/grainfiles$/);
  await expect(page.getByRole('heading', { name: 'Grainfiles & Processes' })).toBeVisible();

  const migrateTitle = `Migrate billing schema ${world.n}`;
  const draftTitle = `Draft rollout plan ${world.n}`;
  const gateTitle = `Continuous verification gate ${world.n}`;
  const lintTitle = `Lint documentation ${world.n}`;

  const table = page.locator('table').first();
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toBeVisible();
  await expect(table.getByText(lintTitle)).toBeVisible();

  // 1. Search & filtering: Kind filter (task vs process)
  await page.getByRole('combobox', { name: 'Kind' }).click();
  await page.getByRole('option', { name: 'Tasks (.task.md)' }).click();
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toHaveCount(0);
  await expect(table.getByText(lintTitle)).toHaveCount(0);

  await page.getByRole('combobox', { name: 'Kind' }).click();
  await page.getByRole('option', { name: 'Processes (.process.md)' }).click();
  await expect(table.getByText(gateTitle)).toBeVisible();
  await expect(table.getByText(lintTitle)).toBeVisible();
  await expect(table.getByText(migrateTitle)).toHaveCount(0);
  await expect(table.getByText(draftTitle)).toHaveCount(0);

  await page.getByRole('combobox', { name: 'Kind' }).click();
  await page.getByRole('option', { name: 'All kinds' }).click();
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toBeVisible();

  // 2. Search & filtering: Draft toggle (All, Active only, Drafts only)
  await page.getByRole('combobox', { name: 'Draft status' }).click();
  await page.getByRole('option', { name: 'Active only' }).click();
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toBeVisible();
  await expect(table.getByText(lintTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toHaveCount(0);

  await page.getByRole('combobox', { name: 'Draft status' }).click();
  await page.getByRole('option', { name: 'Drafts only' }).click();
  await expect(table.getByText(draftTitle)).toBeVisible();
  await expect(table.getByText(migrateTitle)).toHaveCount(0);
  await expect(table.getByText(gateTitle)).toHaveCount(0);
  await expect(table.getByText(lintTitle)).toHaveCount(0);

  await page.getByRole('combobox', { name: 'Draft status' }).click();
  await page.getByRole('option', { name: 'All', exact: true }).click();
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toBeVisible();

  // 3. Search & filtering: Recipient deployment URL
  await page.getByLabel('Recipient deployment').fill(recipientBeta);
  await expect(table.getByText(lintTitle)).toBeVisible();
  await expect(table.getByText(migrateTitle)).toHaveCount(0);
  await expect(table.getByText(gateTitle)).toHaveCount(0);

  await page.getByLabel('Recipient deployment').fill(recipientAlpha);
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toBeVisible();
  await expect(table.getByText(lintTitle)).toHaveCount(0);

  await page.getByLabel('Recipient deployment').fill('');
  await expect(table.getByText(lintTitle)).toBeVisible();

  // 4. Search & filtering: Source repository
  await page.getByLabel('Repository').fill(repo2);
  await expect(table.getByText(lintTitle)).toBeVisible();
  await expect(table.getByText(migrateTitle)).toHaveCount(0);

  await page.getByLabel('Repository').fill(world.repo);
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toBeVisible();
  await expect(table.getByText(lintTitle)).toHaveCount(0);

  await page.getByLabel('Repository').fill('');
  await expect(table.getByText(lintTitle)).toBeVisible();

  // 5. Detail inspection: click the active .task.md row
  const detail = page.getByTestId('grainfile-detail');
  await table.getByText(migrateTitle).click();
  await expect(detail.getByRole('heading', { name: migrateTitle })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^task$/ })).toBeVisible();
  await expect(detail.getByRole('link', { name: recipientAlpha })).toHaveAttribute('href', recipientAlpha);
  await expect(detail.getByText(`Alice <${world.admin}>`)).toBeVisible();
  await expect(detail.getByText('Not before 2020-01-01T00:00:00Z').first()).toBeVisible();

  // Authorized (same-repo) vs External references
  const sameRepoRef = `https://github.com/${world.repo}/blob/main/docs/spec.md`;
  const externalRef = 'https://github.com/external/other/blob/main/external-notes.md';
  await expect(detail.getByRole('link', { name: sameRepoRef }).first()).toHaveAttribute('href', sameRepoRef);
  await expect(detail.getByRole('link', { name: externalRef })).toHaveAttribute('href', externalRef);
  await expect(detail.getByText('Authorized (same-repo)')).toBeVisible();
  await expect(detail.getByText('External', { exact: true })).toBeVisible();

  // depends_on and trigger_with links
  const depUrl = `https://github.com/${repo2}/blob/main/processes/lint.process.md`;
  const twUrl = `https://github.com/${world.repo}/blob/main/.grain/processes/gate.process.md`;
  await expect(detail.getByRole('link', { name: depUrl })).toHaveAttribute('href', depUrl);
  await expect(detail.getByRole('link', { name: twUrl })).toHaveAttribute('href', twUrl);

  // Subscribed nodes and recent events
  await expect(detail.getByText(new RegExp(node.id))).toBeVisible();
  await expect(detail.getByText('task_ready')).toBeVisible();

  // Also click the draft .task.md row to verify the Draft and Not before chips in detail view
  await table.getByText(draftTitle).click();
  await expect(detail.getByRole('heading', { name: draftTitle })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^Draft$/i })).toBeVisible();
  await expect(detail.getByText('Not before 2099-01-01T00:00:00Z').first()).toBeVisible();

  // 6. Detail inspection: click the .process.md row
  await table.getByText(gateTitle).click();
  await expect(detail.getByRole('heading', { name: gateTitle })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^process$/ })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^Gate$/i })).toBeVisible();
  await expect(detail.getByRole('link', { name: recipientAlpha })).toHaveAttribute('href', recipientAlpha);
  await expect(detail.getByText(`Alice <${world.admin}>`)).toBeVisible();

  // Verify all 6 `on` trigger summaries in the detail view
  await expect(detail.getByText('schedule: 0 * * * * (UTC)')).toBeVisible();
  await expect(detail.getByText('push (branches: main; paths: src/**)')).toBeVisible();
  await expect(detail.getByText('pull_request (branches: main)')).toBeVisible();
  await expect(detail.getByText('merge (branches: main)')).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^manual$/ })).toBeVisible();
  await expect(detail.getByText('scenario: Verify staging canary')).toBeVisible();

  // Verify Authorized (same-repo) vs External references, depends_on, trigger_with, subscribed node, and trigger events
  const externalRunbook = 'https://github.com/external/other/blob/main/runbook.md';
  await expect(detail.getByRole('link', { name: sameRepoRef }).first()).toHaveAttribute('href', sameRepoRef);
  await expect(detail.getByRole('link', { name: externalRunbook })).toHaveAttribute('href', externalRunbook);
  await expect(detail.getByText('Authorized (same-repo)')).toBeVisible();
  await expect(detail.getByText('External', { exact: true })).toBeVisible();
  await expect(detail.getByRole('link', { name: depUrl }).first()).toHaveAttribute('href', depUrl);
  await expect(detail.getByText(new RegExp(node.id))).toBeVisible();
  await expect(detail.getByText('process_updated').first()).toBeVisible();
  await expect(detail.getByText('process_triggered').first()).toBeVisible();
  await expect(detail.getByText('trigger: push')).toBeVisible();
  await expect(detail.getByText('trigger: trigger_with')).toBeVisible();

  // Browsing and inspecting Grainfiles leaves nothing in browser storage.
  const s = await storage(page);
  expect(s.indexedDB).toEqual([]);
  expect(s.caches).toEqual({});
  expect(s.serviceWorkers).toEqual([]);
  expect(Object.keys(s.sessionStorage)).toEqual([]);
  expect(Object.keys(s.localStorage)).toEqual([]);
});
