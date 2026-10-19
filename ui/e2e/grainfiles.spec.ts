// Grainfiles discovery, filtering, and detail inspection in the browser
// against `silo hub serve`, a local Git + GitHub simulator, and a
// connected fake Grain node with active subscriptions.
import { expect, hubAdmin, storage, test } from './fixtures';

test('navigate to /grainfiles from the Grain Hub header, filter indexed Grainfiles, and inspect .task.grain.md and .process.grain.md details', async ({
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
    grainfiles: [`https://github.com/${world.repo}/blob/main/.grain/processes/gate.process.grain.md`],
  });

  await hub.pushGitHub(repo2, 'main', {
    'processes/lint.process.grain.md': [
      '---',
      'grainfile: v1',
      `title: Lint documentation ${world.n}`,
      'draft: false',
      'gate: false',
      'when:',
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
    'tasks/01-migrate.task.grain.md': [
      '---',
      'grainfile: v1',
      `title: Migrate billing schema ${world.n}`,
      `recipient: ${recipientAlpha}`,
      `requestor: Alice <${world.admin}>`,
      'draft: false',
      'not_before: 2020-01-01T00:00:00Z',
      'references:',
      '  - /docs/spec.md',
      '  - https://github.com/external/other/blob/main/external-notes.md',
      'depends_on:',
      '  - ./02-draft.task.grain.md',
      '---',
      '# Migrate billing schema',
      '',
      'Run the billing database migration.',
    ].join('\n'),
    'tasks/02-draft.task.grain.md': [
      '---',
      'grainfile: v1',
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
    '.grain/processes/gate.process.grain.md': [
      '---',
      'grainfile: v1',
      `title: Continuous verification gate ${world.n}`,
      'draft: false',
      'gate: true',
      'references:',
      '  - /docs/spec.md',
      '  - https://github.com/external/other/blob/main/runbook.md',
      `  - https://github.com/${repo2}/blob/main/processes/lint.process.grain.md`,
      'when:',
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

  // Push a commit touching src/** so the process is triggered with a process_triggered event,
  // and push a commit to repo2 so lint.process.grain.md co-triggers gate.process.grain.md via depends_on.
  await hub.pushGitHub(world.repo, 'main', {
    'src/service.go': 'package src\n',
  });
  await hub.pushGitHub(repo2, 'main', {
    'docs/guide.md': '# Guide\n',
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
  await page.getByRole('option', { name: 'Tasks (.task.grain.md)' }).click();
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toHaveCount(0);
  await expect(table.getByText(lintTitle)).toHaveCount(0);

  await page.getByRole('combobox', { name: 'Kind' }).click();
  await page.getByRole('option', { name: 'Processes (.process.grain.md)' }).click();
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

  // 3. Search & filtering: Recipient deployment URL (tasks only; processes have no recipient)
  await page.getByLabel('Recipient deployment').fill(recipientBeta);
  await expect(table.getByText(migrateTitle)).toHaveCount(0);
  await expect(table.getByText(gateTitle)).toHaveCount(0);
  await expect(table.getByText(lintTitle)).toHaveCount(0);

  await page.getByLabel('Recipient deployment').fill(recipientAlpha);
  await expect(table.getByText(migrateTitle)).toBeVisible();
  await expect(table.getByText(draftTitle)).toBeVisible();
  await expect(table.getByText(gateTitle)).toHaveCount(0);
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

  // depends_on link on task
  const taskDepUrl = `https://github.com/${world.repo}/blob/main/tasks/02-draft.task.grain.md`;
  const depUrl = `https://github.com/${repo2}/blob/main/processes/lint.process.grain.md`;
  await expect(detail.getByRole('link', { name: taskDepUrl })).toHaveAttribute('href', taskDepUrl);

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

  // Verify all 6 `on` trigger summaries in the detail view
  await expect(detail.getByText('schedule: 0 * * * * (UTC)')).toBeVisible();
  await expect(detail.getByText('push (branches: main; paths: src/**)')).toBeVisible();
  await expect(detail.getByText('pull_request (branches: main)')).toBeVisible();
  await expect(detail.getByText('merge (branches: main)')).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^manual$/ })).toBeVisible();
  await expect(detail.getByText('scenario: Verify staging canary')).toBeVisible();

  // Verify Authorized (same-repo) vs External references, depends_on, subscribed node, and trigger events
  const externalRunbook = 'https://github.com/external/other/blob/main/runbook.md';
  await expect(detail.getByRole('link', { name: sameRepoRef }).first()).toHaveAttribute('href', sameRepoRef);
  await expect(detail.getByRole('link', { name: externalRunbook })).toHaveAttribute('href', externalRunbook);
  await expect(detail.getByText('Authorized (same-repo)')).toBeVisible();
  await expect(detail.getByText('External', { exact: true }).first()).toBeVisible();
  await expect(detail.getByRole('link', { name: depUrl }).first()).toHaveAttribute('href', depUrl);
  await expect(detail.getByText(new RegExp(node.id))).toBeVisible();
  await expect(detail.getByText('process_updated').first()).toBeVisible();
  await expect(detail.getByText('process_triggered').first()).toBeVisible();
  await expect(detail.getByText('trigger: push')).toBeVisible();

  // 7. Push and inspect a Jinja2 Grainfile template (.task.grain.md.jinja2)
  await hub.pushGitHub(world.repo, 'main', {
    'tasks/03-rollout.task.grain.md.jinja2': [
      '---',
      'grainfile: v1',
      `title: "Rollout {{ service_name | default('billing') }} to {{ region | default('eu-west-1') }} ${world.n}"`,
      `recipient: "{{ recipient_url | default('${recipientAlpha}') }}"`,
      '---',
      '# Rollout {{ service_name | default("billing") }}',
      '',
      'Deploy {{ service_name | default("billing") }} in {{ region | default("eu-west-1") }}.',
    ].join('\n'),
  });
  await page.getByRole('button', { name: 'Refresh' }).click();
  const rolloutTitle = `Rollout billing to eu-west-1 ${world.n}`;
  await expect(table.getByText(rolloutTitle)).toBeVisible();
  await expect(table.getByText('jinja2').first()).toBeVisible();
  await expect(table.getByText('vars: recipient_url, region, service_name')).toBeVisible();
  await table.getByText(rolloutTitle).click();
  await expect(detail.getByRole('heading', { name: rolloutTitle })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^jinja2$/ })).toBeVisible();
  await expect(detail.getByText('Template variables (template_vars):')).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^service_name$/ })).toBeVisible();

  // Browsing and inspecting Grainfiles leaves nothing in browser storage.
  const s = await storage(page);
  expect(s.indexedDB).toEqual([]);
  expect(s.caches).toEqual({});
  expect(s.serviceWorkers).toEqual([]);
  expect(Object.keys(s.sessionStorage)).toEqual([]);
  expect(Object.keys(s.localStorage)).toEqual([]);
});

test('Grainfile v1 .process.grain.md.jinja2 templates, RFC 3339 offset not_before, America/Los_Angeles schedule, when.workflow triggers, and Hub-hosted Grainfile editor modal (CUJ-3, CUJ-4, CUJ-6, CUJ-7)', async ({
  world,
  hub,
  pageAs,
  grain,
}) => {
  const deployId = `gf-deploy-${world.n}`;
  await hub.must(world.admin, 'POST', '/v1/deployments', { id: deployId, name: `GF Deploy ${world.n}` });
  const canonicalDeployUrl = `${hub.url}/deployments/${deployId}`;

  await grain({
    owner: world.admin,
    repos: [world.repo],
    id: deployId,
    recipients: [canonicalDeployUrl],
  });

  const taskTemplateTitle = `Canary check {{ env | default('prod') }} ${world.n}`;
  const resolvedTaskTemplateTitle = `Canary check prod ${world.n}`;
  const procTemplateTitle = `Audit {{ target_service | default('ledger') }} ${world.n}`;
  const resolvedProcTemplateTitle = `Audit ledger ${world.n}`;
  const offsetTaskTitle = `Cutover with Pacific offset ${world.n}`;
  const workflowProcTitle = `CI Workflow Watcher ${world.n}`;

  const headSha = await hub.pushGitHub(world.repo, 'main', {
    'templates/canary.task.grain.md.jinja2': [
      '---',
      'grainfile: v1',
      `title: "${taskTemplateTitle}"`,
      `recipient: "${canonicalDeployUrl}"`,
      '---',
      'Run canary checks for {{ env | default("prod") }} in {{ cluster | default("us-west") }}.',
    ].join('\n'),
    'templates/audit.process.grain.md.jinja2': [
      '---',
      'grainfile: v1',
      `title: "${procTemplateTitle}"`,
      'when:',
      '  schedule:',
      '    cron: "0 9 * * 1"',
      '    timezone: "America/Los_Angeles"',
      '---',
      'Audit {{ target_service | default("ledger") }} with threshold {{ max_findings | default("0") }}.',
    ].join('\n'),
    'tasks/offset.task.grain.md': [
      '---',
      'grainfile: v1',
      `title: "${offsetTaskTitle}"`,
      `recipient: "${canonicalDeployUrl}"`,
      'not_before: "2026-10-25T09:00:00-07:00"',
      '---',
      'Execute cutover at 9am Pacific.',
    ].join('\n'),
    'processes/ci-watch.process.grain.md': [
      '---',
      'grainfile: v1',
      `title: "${workflowProcTitle}"`,
      'when:',
      '  schedule:',
      '    cron: "30 8 * * 1-5"',
      '    timezone: "America/Los_Angeles"',
      '  workflow:',
      '    workflow: "CI"',
      '    branches: ["main"]',
      '    events: ["triggered", "succeeded", "failed"]',
      '---',
      'Triage CI workflow runs.',
    ].join('\n'),
  });

  // Fire a simulated GitHub workflow_run webhook matching when.workflow.
  await hub.workflowRunGitHub(world.repo, {
    action: 'completed',
    name: 'CI',
    path: '.github/workflows/ci.yml',
    headBranch: 'main',
    sha: headSha,
    conclusion: 'failure',
    htmlUrl: `https://github.com/${world.repo}/actions/runs/9001`,
  });

  const page = await pageAs(world.admin);
  await page.goto('/grainfiles');

  const table = page.getByRole('table').first();
  const detail = page.getByTestId('grainfile-detail');

  // 1. Inspect .task.grain.md.jinja2 and .process.grain.md.jinja2 templates (CUJ-7).
  await expect(table.getByText(resolvedTaskTemplateTitle)).toBeVisible();
  await table.getByText(resolvedTaskTemplateTitle).click();
  await expect(detail.getByRole('heading', { name: resolvedTaskTemplateTitle })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^jinja2$/ })).toBeVisible();
  await expect(detail.getByText('Template variables (template_vars):')).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^cluster$/ })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^env$/ })).toBeVisible();

  await expect(table.getByText(resolvedProcTemplateTitle)).toBeVisible();
  await table.getByText(resolvedProcTemplateTitle).click();
  await expect(detail.getByRole('heading', { name: resolvedProcTemplateTitle })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^jinja2$/ })).toBeVisible();
  await expect(detail.getByText('Template variables (template_vars):')).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^max_findings$/ })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^target_service$/ })).toBeVisible();

  // 2. Inspect RFC 3339 offset not_before task and when.schedule.timezone + when.workflow process (CUJ-6).
  await table.getByText(offsetTaskTitle).click();
  await expect(detail.getByRole('heading', { name: offsetTaskTitle })).toBeVisible();
  await expect(detail.getByText('Not before 2026-10-25T16:00:00Z').first()).toBeVisible();

  await table.getByText(workflowProcTitle).click();
  await expect(detail.getByRole('heading', { name: workflowProcTitle })).toBeVisible();
  await expect(detail.getByText('schedule: 30 8 * * 1-5 (America/Los_Angeles)')).toBeVisible();
  await expect(
    detail.getByText('workflow (CI; events: triggered, succeeded, failed; branches: main)'),
  ).toBeVisible();
  await expect(detail.getByText('process_triggered').first()).toBeVisible();
  await expect(detail.getByText('trigger: workflow (main)')).toBeVisible();

  // 3. Author a deployment-scoped empty-body .task.grain.md in the Hub-hosted Grainfile editor modal targeting canonicalDeployUrl (CUJ-3 & CUJ-4).
  const hostedTitle = `Empty-body spec task ${world.n}`;
  await page.getByTestId('open-grainfile-editor-btn').click();
  const modal = page.getByTestId('grainfile-editor-modal');
  await expect(modal).toBeVisible();

  await modal.getByLabel('Grainfile scope').click();
  await page.getByRole('option', { name: 'Deployment', exact: true }).click();
  await modal.getByTestId('grainfile-editor-team').fill(deployId);
  await modal.getByTestId('grainfile-editor-path').fill('tasks/spec-only.task.grain.md');
  await modal.getByTestId('grainfile-editor-content').fill(
    [
      '---',
      'grainfile: v1',
      `title: "${hostedTitle}"`,
      `recipient: "${canonicalDeployUrl}"`,
      'references:',
      '  - "specs/rollout.md"',
      '---',
    ].join('\n'),
  );
  await modal.getByTestId('grainfile-editor-ref-path').fill('specs/rollout.md');
  await modal.getByTestId('grainfile-editor-ref-content').fill('# Rollout Spec\n\nVerify canary metrics before full rollout.\n');
  await modal.getByTestId('save-grainfile-btn').click();

  await expect(modal).toHaveCount(0);
  await expect(table.getByText(hostedTitle)).toBeVisible();
  await table.getByText(hostedTitle).click();
  await expect(detail.getByRole('heading', { name: hostedTitle })).toBeVisible();
  await expect(detail.getByRole('link', { name: canonicalDeployUrl })).toHaveAttribute('href', canonicalDeployUrl);
  await expect(detail.getByText('Authorized (same-repo)')).toBeVisible();
  await expect(detail.getByText('task_ready').first()).toBeVisible();

  // 4. Author a .capability.grain.md file in the Hub-hosted Grainfile editor modal and filter by Capabilities.
  const capTitle = `GCP key capability ${world.n}`;
  await page.getByTestId('open-grainfile-editor-btn').click();
  await expect(modal).toBeVisible();
  await modal.getByLabel('Grainfile kind').click();
  await page.getByRole('option', { name: 'Capability (.capability.grain.md)' }).click();
  await modal.getByTestId('grainfile-editor-path').fill('capabilities/gcp-key.capability.grain.md');
  await modal.getByTestId('grainfile-editor-content').fill(
    [
      '---',
      'grainfile: v1',
      `title: "${capTitle}"`,
      '---',
      'Provides short-lived GCP credentials.',
    ].join('\n'),
  );
  await modal.getByTestId('grainfile-editor-ref-path').fill('');
  await modal.getByTestId('grainfile-editor-ref-content').fill('');
  await modal.getByTestId('save-grainfile-btn').click();

  await expect(modal).toHaveCount(0);
  await page.getByLabel('Kind').click();
  await page.getByRole('option', { name: 'Capabilities (.capability.grain.md)' }).click();
  await expect(table.getByText(capTitle)).toBeVisible();
  await table.getByText(capTitle).click();
  await expect(detail.getByRole('heading', { name: capTitle })).toBeVisible();
  await expect(detail.locator('.MuiChip-label', { hasText: /^capability$/ })).toBeVisible();
  await expect(detail.getByText('capability_updated').first()).toBeVisible();
});

