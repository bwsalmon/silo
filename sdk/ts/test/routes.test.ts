// The SDK calls the hub's public API and nothing else (docs/ui.md §4):
// every method's request is a route docs/hub.md documents, method and
// path. A method added to HubClient without a case here fails, and so
// does a case whose route the doc does not name: an endpoint a client
// needs goes into the documented API, not into the SDK alone.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it, vi } from 'vitest';
import { HubClient } from '../src/index.js';

const doc = readFileSync(fileURLToPath(new URL('../../../docs/hub.md', import.meta.url)), 'utf8')
  .replace(/`/g, '')
  .replace(/\s+/g, ' ');

type Case = [method: string, template: string, call: (h: HubClient) => unknown];

const cases: Record<string, Case> = {
  getMe: ['GET', '/v1/me', (h) => h.getMe()],
  setTheme: ['PUT', '/v1/me/theme', (h) => h.setTheme('dark')],
  getInboxPreferences: ['GET', '/v1/me/inbox', (h) => h.getInboxPreferences()],
  setInboxPreferences: ['PUT', '/v1/me/inbox', (h) => h.setInboxPreferences(['ws-1'])],
  listInbox: ['GET', '/v1/inbox', (h) => h.listInbox({ nodes: ['ws-1'], kind: 'proposal' })],
  listTasks: ['GET', '/v1/tasks', (h) => h.listTasks({ team: 'infra' })],
  getTask: ['GET', '/v1/tasks/{id}', (h) => h.getTask('t-1')],
  submitTask: ['POST', '/v1/tasks', (h) => h.submitTask({ team: 'infra', title: 't', prompt: 'p' })],
  cancelTask: ['POST', '/v1/tasks/{id}/cancel', (h) => h.cancelTask('t-1')],
  requeueTask: ['POST', '/v1/tasks/{id}/requeue', (h) => h.requeueTask('t-1')],
  approveTask: ['POST', '/v1/tasks/{id}/approve', (h) => h.approveTask('t-1')],
  rejectTask: ['POST', '/v1/tasks/{id}/reject', (h) => h.rejectTask('t-1')],
  listApprovals: ['GET', '/v1/tasks/{id}/approvals', (h) => h.listApprovals('t-1')],
  answerApproval: ['POST', '/v1/tasks/{id}/approvals/{request_id}', (h) => h.answerApproval('t-1', 'r-1', 'allow')],
  listMessages: ['GET', '/v1/tasks/{id}/messages', (h) => h.listMessages('t-1')],
  sendMessage: ['POST', '/v1/tasks/{id}/messages', (h) => h.sendMessage('t-1', 'm-1', 'hi')],
  follow: ['GET', '/v1/tasks/{id}/events', (h) => h.follow('t-1', { onEvent: () => {} }).done],
  events: ['GET', '/v1/tasks/{id}/events', async (h) => { for await (const _ of h.events('t-1')) void _; }],
  eventsURL: ['GET', '/v1/tasks/{id}/events', (h) => h.eventsURL('t-1')],
  fetchArtifact: ['GET', '/v1/tasks/{id}/artifacts', (h) => h.fetchArtifact('t-1', 'diff-1')],
  listNodes: ['GET', '/v1/nodes', (h) => h.listNodes()],
  listNodeTokens: ['GET', '/v1/node-tokens', (h) => h.listNodeTokens()],
  mintNodeToken: ['POST', '/v1/node-tokens', (h) => h.mintNodeToken('ws-1')],
  revokeNodeToken: ['DELETE', '/v1/node-tokens/{id}', (h) => h.revokeNodeToken('k-1')],
  listTeams: ['GET', '/v1/teams', (h) => h.listTeams()],
  getTeam: ['GET', '/v1/teams/{team}', (h) => h.getTeam('infra')],
  createTeam: ['POST', '/v1/teams', (h) => h.createTeam('infra', 'Infra')],
  deleteTeam: ['DELETE', '/v1/teams/{team}', (h) => h.deleteTeam('infra')],
  setMember: ['PUT', '/v1/teams/{team}/members/{principal}', (h) => h.setMember('infra', 'user:a@example.com', 'member')],
  removeMember: ['DELETE', '/v1/teams/{team}/members/{principal}', (h) => h.removeMember('infra', 'group:ops')],
  assignRepo: ['PUT', '/v1/teams/{team}/repos/{owner}/{name}', (h) => h.assignRepo('infra', 'acme/app')],
  releaseRepo: ['DELETE', '/v1/teams/{team}/repos/{owner}/{name}', (h) => h.releaseRepo('infra', 'acme/app')],
  getTeamSettings: ['GET', '/v1/teams/{team}/settings', (h) => h.getTeamSettings('infra')],
  setTeamSettings: ['PUT', '/v1/teams/{team}/settings', (h) => h.setTeamSettings('infra', { lost_requeue_after_seconds: 0 })],
  getGrainfilesOverview: ['GET', '/v1/grainfiles', (h) => h.getGrainfilesOverview({ kind: 'process' })],
  listGrainfiles: ['GET', '/v1/grainfiles', (h) => h.listGrainfiles({ kind: 'task', repo: 'acme/app', draft: false })],
  inspectGrainfile: ['GET', '/v1/grainfiles', (h) => h.inspectGrainfile('https://github.com/acme/app/blob/main/tasks/deploy.task.md')],
  listGrainfileSubscriptions: ['GET', '/v1/grainfiles', (h) => h.listGrainfileSubscriptions('node-1')],
  listGrainfileEvents: ['GET', '/v1/grainfiles', (h) => h.listGrainfileEvents({ recipient: 'https://github.com/acme/grain-eu' })],
};

const pattern = (template: string) => new RegExp(`^${template.replace(/\{[a-z_]+\}/g, '[^/?]+')}(\\?|$)`);

describe('the SDK’s routes', () => {
  it('cover every HubClient method', () => {
    const methods = Object.getOwnPropertyNames(HubClient.prototype).filter((m) => m !== 'constructor' && !isPrivate(m));
    expect(methods.sort()).toEqual(Object.keys(cases).sort());
  });

  it.each(Object.entries(cases))('%s is a documented route', async (_name, [method, template, call]) => {
    expect(doc.includes(`${method} ${template}`), `docs/hub.md names ${method} ${template}`).toBe(true);
    const seen: { method: string; url: string }[] = [];
    const h = new HubClient({
      baseURL: 'https://hub.test',
      fetch: vi.fn(async (url: string, init?: RequestInit) => {
        seen.push({ method: init?.method ?? 'GET', url });
        const sse = url.includes('/events');
        return new Response(sse ? 'event: end\ndata: {"type":"end"}\n\n' : '{}', { status: 200 });
      }),
    });
    const r = await call(h);
    const urls = typeof r === 'string' ? [{ method: 'GET', url: r }] : seen;
    expect(urls.length).toBeGreaterThan(0);
    for (const u of urls) {
      expect(u.method).toBe(method);
      expect(new URL(u.url).pathname + new URL(u.url).search).toMatch(pattern(template));
    }
  });
});

// isPrivate is whether m is one of HubClient's own helpers, which
// TypeScript's private does not hide from the prototype.
function isPrivate(m: string): boolean {
  return ['request', 'send', 'headers', 'getJSON', 'write', 'defaultTransport', 'connection', 'setAdminMode', 'getAdminMode'].includes(m);
}
