# @silo/hub-sdk

A TypeScript client for the Grain Hub public API
([docs/hub.md](../../docs/hub.md) §5-§7): typed models for Grainfiles
(`.task.md` and `.process.md`), subscriptions, tasks, events, artifacts,
nodes, node tokens, teams, proposals and approvals; a live event stream
that resumes from a cursor; and the API's refusals as typed errors. It
has no runtime dependencies and runs in browsers and in Node 20 or later.

The hub's own web UI (`ui/`, [docs/ui.md](../../docs/ui.md)) is its
first client and reaches the API through nothing else
(`ui/src/sdkonly.test.ts`). The SDK calls only routes docs/hub.md
documents (`test/routes.test.ts`): there are no private endpoints for
the UI, so anything the hub's page does, a page of your own can.

## Signing in

**In a browser, on the hub's own origin,** a client needs no options.
Its requests are same-origin and carry the browser's own credentials
(IAP's cookie, or the hub's OIDC session), and the SDK never reads,
stores or forwards a token:

```ts
import { HubClient } from '@silo/hub-sdk';

const hub = new HubClient();
```

**Anywhere else** (Node, a script, a CI job), pass the hub's URL and a
token. The token goes in `Authorization: Bearer ...`. It is the hub's
OIDC ID token for a hub that signs in with OIDC (`-oidc-issuer`), or a
Google-signed OIDC token whose audience is IAP's OAuth client ID for a
hub behind IAP. A function is called before every request, so it can
refresh the token:

```ts
import { HubClient } from '@silo/hub-sdk';

const hub = new HubClient({
  baseURL: 'https://hub.example.com',
  // Your own code: for IAP, e.g. google-auth-library's
  // (await new GoogleAuth().getIdTokenClient(iapClientID)).idTokenProvider.fetchIdToken(iapClientID).
  token: () => getIdToken(),
});
```

If `Authorization` is already taken by something else behind IAP, set
`tokenHeader: 'Proxy-Authorization'` and IAP reads its token from that
header instead. `headers` adds headers to every request, such as a local
development hub's `-dev-user-header`. `fetch` replaces the global fetch.

The hub accepts a write only from its own origin (`identity.SameOrigin`)
or from a client that is not a browser. A page on another origin can
read with `credentials: 'include'` but cannot write.

## Examples

### List and inspect Grainfiles and subscriptions

```ts
const grainfiles = await hub.listGrainfiles({
  kind: 'process',
  recipient: 'https://github.com/acme/grain-eu',
  repo: 'acme/app',
  draft: false,
});
for (const g of grainfiles) console.log(g.kind, g.title, g.url);

const detail = await hub.inspectGrainfile('https://github.com/acme/app/blob/main/.grain/processes/ci.process.md');
console.log(detail.grainfile?.on, detail.events);
```

### List a team's backlog

```ts
const tasks = await hub.listTasks({ team: 'infra', states: ['queued', 'running'], limit: 50 });
for (const t of tasks) console.log(t.id, t.state, t.title);
```

Leave `team` out to list every team you are in. Results are newest
first.

### File a task

```ts
const task = await hub.submitTask({
  repo: 'acme/app', // or team: 'infra'; exactly one of the two
  title: 'Rename the greeting',
  prompt: 'Rename HELLO to GREETING everywhere.',
});
console.log(task.id, task.state); // queued, or proposed if the team asks for approval
```

A proposal is approved or rejected by another member of its team with
`hub.approveTask(id)` and `hub.rejectTask(id, reason)`. An approval
request inside a run is answered with
`hub.answerApproval(id, requestID, 'allow' | 'deny', reason)`.

### Follow its events

`events` is an async iterable that ends when the task's events are over:

```ts
for await (const e of hub.events(task.id)) {
  if (e.status) console.log(e.seq, e.status.phase, e.status.message);
  if (e.result) console.log('exit', e.result.exit.code, e.result.final_text);
}
```

`follow` is the same stream with callbacks, for a UI:

```ts
const stream = hub.follow(task.id, {
  from: lastSeenSeq, // resume: only events after this seq; 0, the start, by default
  onEvent: (e) => render(e),
  onState: (s) => showState(s), // connecting, live, reconnecting, ended, closed
  onError: (err) => console.warn(err),
});
// later
stream.close();
saveCursor(stream.cursor);
```

A dropped stream reconnects from its cursor. Each seq is delivered once
and in order, and seqs have gaps, so the cursor is always a seq you
have seen. A refusal, such as a task you may not see (404), closes the
stream. `events` throws it.

In a browser with no token, the stream uses the browser's
`EventSource`, which carries the browser's cookie. Otherwise it uses a
streamed `fetch`, which carries the token. Pass `transport: 'fetch'` or
`'eventsource'` to choose yourself. The API also serves the same stream
over a WebSocket (docs/hub.md §7.1), which this SDK does not use.

### Fetch an artifact without persisting it

An event that names a node-only artifact (a diff, a tool log) carries
only its reference. The hub reads the content through from the node
that holds it and never stores it (docs/hub.md §7.2):

```ts
for await (const e of hub.events(task.id)) {
  if (e.artifact?.ref) {
    const r = await hub.fetchArtifact(task.id, e.artifact.ref);
    if (r.kind === 'text') show(r.text); // in memory only
    else if (r.kind === 'unavailable') show(`not available: ${r.message}`); // its node is offline
    else show(`error ${r.status}: ${r.message}`);
  }
}
```

The request is made with `cache: 'no-store'`, which matches the hub's
`Cache-Control`, and the SDK keeps no copy. Keep the text in memory and
let it go when you are done with it. Do not write it to storage.

## Errors

A refusal is an `ApiError` with the HTTP `status` and the API's own
message. The common statuses have their own subclasses, so you can
catch only the ones you handle:

| Status | Class | Means |
| --- | --- | --- |
| 400 | `BadRequestError` | a bad field or cursor |
| 401 | `UnauthenticatedError` | not signed in, or the token was refused |
| 403 | `ForbiddenError` | signed in, and not allowed to do this |
| 404 | `NotFoundError` | no such thing, or you may not see it (the hub answers both alike) |
| 409 | `ConflictError` | not allowed in its current state, such as cancelling a finished task |
| 503 | `UnavailableError` | the hub, or the node behind it, cannot answer now |

```ts
import { ConflictError } from '@silo/hub-sdk';

try {
  await hub.cancelTask(id, 'superseded');
} catch (e) {
  if (!(e instanceof ConflictError)) throw e; // already over
}
```

## Development

```sh
cd sdk/ts
npm ci
npm run typecheck && npm run build   # dist/: ES modules and .d.ts
npm test                             # vitest unit tests
npm run test:integration             # against a real `silo hub serve` and a fake Grain; needs Go
```

Grain CI runs all four (`.grain/workflows/ui.yml`, the `sdk` job). The
hub UI does not use `dist/`. It builds `src/` itself as `@silo/hub-sdk`,
through `ui/tsconfig.json`'s `paths` and `ui/vite.config.ts`'s alias, so
a change to the SDK and to the UI is one commit and one build.

## Publishing

**Not published yet, on purpose.** `package.json` is `private: true`,
so `npm publish` refuses to run. The package is used from this
repository, by the hub UI from source and by anyone else from a build
of `sdk/ts`:

```sh
cd sdk/ts && npm ci && npm run build && npm pack   # silo-hub-sdk-0.1.0.tgz
```

Publishing waits until a client outside this repository needs it, and
until the owner has chosen a registry and a scope (`@silo` is a
placeholder that nobody has registered). When it is published:

- **Versions follow the hub API's, not silo's releases.** The package
  is `0.x` while `/v1` is still changing, and a minor version bump is a
  breaking change, as semver allows below 1.0. It becomes `1.0.0` when
  docs/hub.md declares `/v1` stable. Each major version after that
  matches an API version (`/v2` is `2.x`).
- **It is released from this repository**, from a tag `sdk-ts/vX.Y.Z`
  on a commit whose grain CI `sdk` job is green, by a workflow that has
  the publish credential. No task branch publishes.
- **The version is bumped in the change that alters the client.** The
  routes test already ties every method to a documented route, so a
  change to the API and the client that follows it land together.
