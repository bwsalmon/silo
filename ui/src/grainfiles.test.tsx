// Grainfiles and multi-tenant deployment views tests: verifies Grain Hub
// branding, /grainfiles listing, filtering, metadata/events inspection,
// and /nodes recipient identities, subscriptions, and tunneled local UI
// links.
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { App } from './App';

function open(path: string) {
  window.history.replaceState(null, '', path);
  render(<App />);
}

const sampleOverview = {
  grainfiles: [
    {
      grainfile: 'v1',
      spec_summary_path: 'docs/grainfile-v1.md',
      spec_path: 'docs/grainfiles-spec.md',
      repo: 'https://github.com/acme/app',
      path: 'tasks/01-deploy.task.grain.md',
      commit_sha: '1111111111111111111111111111111111111111',
      ref: 'main',
      url: 'https://github.com/acme/app/blob/main/tasks/01-deploy.task.grain.md',
      kind: 'task',
      title: 'Deploy service to staging',
      from_template: 'https://github.com/acme/app/blob/main/.grain/templates/deploy.task.grain.md.jinja2',
      recipient: 'https://github.com/acme/grain-eu',
      requestor: 'Alice <alice@example.com>',
      draft: false,
      not_before: '2026-10-19T00:00:00Z',
      references: ['/docs/arch.md'],
      authorized_references: ['https://github.com/acme/app/blob/main/docs/arch.md'],
      depends_on: ['https://github.com/acme/app/blob/main/tasks/00-prep.task.grain.md'],
      capabilities: [
        { capability: 'ghcr.io/bwsalmon/grain/capabilities/self-debug:latest' },
        {
          capability: 'ghcr.io/bwsalmon/grain/capabilities/gcp:latest#gcp-key',
          with: { account: 'infra-staging' },
        },
      ],
      updated_at: '2026-10-18T10:05:00Z',
    },
    {
      grainfile: 'v1',
      spec_summary_path: 'docs/grainfile-v1.md',
      spec_path: 'docs/grainfiles-spec.md',
      repo: 'https://github.com/acme/infra',
      path: '.grain/processes/ci.process.grain.md.jinja2',
      commit_sha: '2222222222222222222222222222222222222222',
      ref: 'main',
      url: 'https://github.com/acme/infra/blob/main/.grain/processes/ci.process.grain.md.jinja2',
      kind: 'process',
      title: 'Continuous verification gate',
      is_template: true,
      template_vars: ['branch', 'region'],
      draft: true,
      references: ['https://github.com/acme/infra/blob/main/.grain/processes/smoke.process.grain.md'],
      when: {
        push: { repos: ['acme/infra', 'acme/app'], branches: ['main'], paths: ['src/auth/'] },
        schedule: [
          { cron: '0 * * * *' },
          { cron: '0 9 * * 1-5', timezone: 'Europe/London' },
        ],
        workflow: [
          {
            workflow: 'CI',
            workflows: ['Nightly *'],
            events: ['failed', 'succeeded'],
            branches: ['main'],
          },
        ],
      },
      gate: true,
      updated_at: '2026-10-18T10:06:00Z',
    },
  ],
  subscriptions: [
    {
      node_id: 'ws-eu-1',
      owner: 'alice@example.com',
      recipients: ['https://github.com/acme/grain-eu'],
      repos: ['https://github.com/acme/app'],
      folders: ['https://github.com/acme/app/tree/main/tasks'],
      grainfiles: ['https://github.com/acme/app/blob/main/tasks/01-deploy.task.grain.md'],
      cursor: 5,
      revision: 1,
      updated_at: '2026-10-18T10:05:00Z',
    },
  ],
  events: [
    {
      seq: 1,
      type: 'task_ready',
      repo: 'https://github.com/acme/app',
      path: 'tasks/01-deploy.task.grain.md',
      grainfile_url: 'https://github.com/acme/app/blob/main/tasks/01-deploy.task.grain.md',
      kind: 'task',
      title: 'Deploy service to staging',
      from_template: 'https://github.com/acme/app/blob/main/.grain/templates/deploy.task.grain.md.jinja2',
      recipient: 'https://github.com/acme/grain-eu',
      created_at: '2026-10-18T10:05:00Z',
    },
    {
      seq: 2,
      type: 'process_triggered',
      repo: 'https://github.com/acme/infra',
      path: '.grain/processes/ci.process.grain.md.jinja2',
      grainfile_url: 'https://github.com/acme/infra/blob/main/.grain/processes/ci.process.grain.md.jinja2',
      kind: 'process',
      title: 'Continuous verification gate',
      is_template: true,
      template_vars: ['branch', 'region'],
      recipient: 'https://github.com/acme/grain-us',
      trigger_source: 'push',
      gate: true,
      reason: 'commit 2222222 on main',
      created_at: '2026-10-18T10:06:00Z',
    },
    {
      seq: 3,
      type: 'process_triggered',
      repo: 'https://github.com/acme/infra',
      path: '.grain/processes/ci.process.grain.md.jinja2',
      grainfile_url: 'https://github.com/acme/infra/blob/main/.grain/processes/ci.process.grain.md.jinja2',
      kind: 'process',
      title: 'Continuous verification gate',
      trigger_source: 'workflow',
      branch: 'main',
      workflow_name: 'CI',
      workflow_event: 'failed',
      workflow_url: 'https://github.com/acme/infra/actions/runs/9001',
      created_at: '2026-10-18T10:07:00Z',
    },
  ],
};

describe('Grain Hub branding, /grainfiles, and /nodes deployment views', () => {
  it('renders Grain Hub branding and displays indexed Grainfiles, triggers, dependencies, metadata, and events', async () => {
    const seenUrls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        seenUrls.push(url);
        if (url === '/v1/me') {
          return Response.json({ email: 'alice@example.com', hub_admin: false, teams: { infra: 'admin' } });
        }
        if (url.startsWith('/v1/grainfiles')) {
          return Response.json(sampleOverview);
        }
        return new Response('not found', { status: 404 });
      }),
    );

    open('/grainfiles');

    expect(screen.getByText('Grain Hub')).toBeTruthy();
    expect(await screen.findByText('Grainfiles & Processes')).toBeTruthy();
    expect((await screen.findAllByText('Deploy service to staging')).length).toBeGreaterThan(0);
    expect(screen.getByText('Continuous verification gate')).toBeTruthy();

    // Badges and links on rows
    expect(screen.getAllByText('Not before 2026-10-19T00:00:00Z').length).toBeGreaterThan(0);
    expect(screen.getByText('Draft')).toBeTruthy();
    expect(screen.getByText('Gate')).toBeTruthy();
    expect(screen.getAllByText('jinja2').length).toBeGreaterThan(0);
    expect(screen.getByText('vars: branch, region')).toBeTruthy();
    expect(screen.getByText('from_template: acme/app:.grain/templates/deploy.task.grain.md.jinja2')).toBeTruthy();
    expect(screen.getByText('push (repos: acme/infra, acme/app; branches: main; paths: src/auth/)')).toBeTruthy();
    expect(screen.getByText('schedule: 0 * * * *')).toBeTruthy();
    expect(screen.getByText('schedule: 0 9 * * 1-5 (Europe/London)')).toBeTruthy();
    expect(screen.getByText('workflow (CI, Nightly *; events: failed, succeeded; branches: main)')).toBeTruthy();
    expect(screen.getByText('acme/app:tasks/00-prep.task.grain.md').getAttribute('href')).toBe(
      'https://github.com/acme/app/blob/main/tasks/00-prep.task.grain.md',
    );
    expect(screen.getAllByText('ghcr.io/bwsalmon/grain/capabilities/self-debug:latest').length).toBeGreaterThanOrEqual(2);
    expect(
      screen.getAllByText('ghcr.io/bwsalmon/grain/capabilities/gcp:latest#gcp-key (account=infra-staging)').length,
    ).toBeGreaterThanOrEqual(2);

    // Inspection panel for first Grainfile (Deploy service to staging)
    expect(screen.getByText('docs/grainfile-v1.md')).toBeTruthy();
    expect(
      screen.getByText('https://github.com/acme/app/blob/main/.grain/templates/deploy.task.grain.md.jinja2').getAttribute('href'),
    ).toBe('https://github.com/acme/app/blob/main/.grain/templates/deploy.task.grain.md.jinja2');
    expect(screen.getByText('https://github.com/acme/app/blob/main/docs/arch.md').getAttribute('href')).toBe(
      'https://github.com/acme/app/blob/main/docs/arch.md',
    );
    expect(screen.getByText('task_ready')).toBeTruthy();
    expect(screen.getByText(/ws-eu-1/)).toBeTruthy();

    // Click the second row to inspect the process Grainfile template and its trigger event
    fireEvent.click(screen.getByText('Continuous verification gate'));
    expect((await screen.findAllByText('process_triggered')).length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText('Template variables (template_vars):')).toBeTruthy();
    expect(screen.getByText('branch')).toBeTruthy();
    expect(screen.getByText('region')).toBeTruthy();
    expect(screen.getByText('https://github.com/acme/infra/blob/main/.grain/processes/smoke.process.grain.md').getAttribute('href')).toBe(
      'https://github.com/acme/infra/blob/main/.grain/processes/smoke.process.grain.md',
    );
    expect(screen.getByText('trigger: push')).toBeTruthy();
    expect(screen.getByText('commit 2222222 on main')).toBeTruthy();
    expect(screen.getByText('trigger: workflow (main)')).toBeTruthy();
    expect(screen.getByText('CI').getAttribute('href')).toBe('https://github.com/acme/infra/actions/runs/9001');
    expect(screen.getByText(/\[failed\]/)).toBeTruthy();

    // Filter by repository and recipient
    fireEvent.change(screen.getByLabelText('Repository'), { target: { value: 'acme/app' } });
    await waitFor(() => {
      expect(seenUrls.some((u) => u.includes('repo=acme%2Fapp'))).toBe(true);
    });
    expect(screen.queryByText('Continuous verification gate')).toBeNull();
    expect(screen.getAllByText('Deploy service to staging').length).toBeGreaterThan(0);

    // Filter by kind = process
    fireEvent.mouseDown(screen.getByLabelText('Kind'));
    fireEvent.click(await screen.findByRole('option', { name: 'Processes (.process.grain.md)' }));
    await waitFor(() => {
      expect(seenUrls.some((u) => u.includes('kind=process'))).toBe(true);
    });
  });

  it('shows node recipient URLs, subscribed repositories/folders/processes, and tunneled local UI links on /nodes', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        if (url === '/v1/me') {
          return Response.json({ email: 'alice@example.com', hub_admin: false, teams: { infra: 'admin' } });
        }
        if (url === '/v1/nodes') {
          return Response.json({
            nodes: [
              {
                id: 'ws-eu-1',
                repos: ['acme/app'],
                slots: 4,
                owner: 'alice@example.com',
                status: 'online',
                free: 3,
                claimed: 0,
                version: '0.2.0',
                connected_at: '2026-10-18T10:00:00Z',
                last_seen: '2026-10-18T10:05:00Z',
                features: ['grainfiles', 'nodeui'],
                recipients: ['https://github.com/acme/grain-eu'],
                folders: ['https://github.com/acme/app/tree/main/.grain/processes'],
                grainfiles: ['https://github.com/acme/app/blob/main/.grain/processes/ci.process.md'],
                ui: { offered: true, url: 'https://ws-eu-1.nodes.hub.example.com/' },
                ui_url: 'https://ws-eu-1.nodes.hub.example.com/',
              },
              {
                id: 'ws-us-offline',
                repos: ['acme/infra'],
                slots: 2,
                owner: 'alice@example.com',
                status: 'offline',
                free: 0,
                claimed: 0,
                connected_at: '2026-10-18T09:00:00Z',
                last_seen: '2026-10-18T09:30:00Z',
                features: ['nodeui'],
                recipients: ['https://github.com/acme/grain-us'],
                ui: { offered: true },
              },
            ],
          });
        }
        if (url === '/v1/node-tokens') {
          return Response.json({ node_tokens: [] });
        }
        return new Response('not found', { status: 404 });
      }),
    );

    open('/nodes');

    const euRow = (await screen.findByText('ws-eu-1')).closest('tr')!;
    expect(within(euRow).getByText('3 of 4')).toBeTruthy();
    expect(within(euRow).getByText('https://github.com/acme/grain-eu').getAttribute('href')).toBe(
      'https://github.com/acme/grain-eu',
    );
    expect(within(euRow).getByText('folder: acme/app/tree/main/.grain/processes')).toBeTruthy();
    expect(
      within(euRow).getByText('acme/app/blob/main/.grain/processes/ci.process.md').getAttribute('href'),
    ).toBe('https://github.com/acme/app/blob/main/.grain/processes/ci.process.md');
    const uiLink = within(euRow).getByRole('link', { name: 'Open Grain UI' });
    expect(uiLink.getAttribute('href')).toBe('https://ws-eu-1.nodes.hub.example.com/');

    const usRow = screen.getByText('ws-us-offline').closest('tr')!;
    expect(within(usRow).getByText('Offline (nodeui)')).toBeTruthy();
  });
});
