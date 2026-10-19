# Shared Chat Sessions in the Grain Hub

Shared Chat Sessions (`/v1/chats`, `hub/chats`, `hub/tasks`, `sdk/ts`, and `ui/src/pages/Chats.tsx`) let teams of engineers, PMs, and operators collaborate with one or more Grain deployments in a shared multi-party chat room.

---

## 1. Model & Backing Session

Each `ChatSession` (`c-<16hex>`) is backed by a standing interactive Hub task (`t-<16hex>`, `kind: "interactive"`) routed to the room's primary `deployment` over the Hub's rendezvous tunnel:

- `id`: Unique chat session ID (`c-<16hex>`).
- `task_id`: Backing interactive task ID (`t-<16hex>`).
- `title`: Human-readable room title (e.g. `"Payments Ops Room"`, `"Checkout v2 Design"`).
- `deployment`: Primary backing Grain deployment ID (e.g. `ops-agent`, `design-agent`).
- `linked_deployments`: Secondary deployment IDs that can be `@mentioned` or targeted when the backing agent proposes follow-up tasks (`Filing.OriginTask`) from the room.
- `reply_mode`:
  - `"mention"` (default): Human participants message one another in real time without waking the agent on every message. The backing agent is woken only when a message mentions `@agent`, `@<deployment>`, or `@<linked-deployment>`, or when the sender sets `trigger_agent: true` (the **"Ask `@<deployment>`"** button in the UI).
  - `"all"`: Every human message posted to the room wakes the backing agent.
- `participants`:
  - **Deployment members (`source: "deployment"`):** Members (`admin`, `member`) of the primary `deployment` automatically have participant access.
  - **Invited participants (`source: "invited"`):** Explicit list of invited Hub users (`email`, `role: "participant" | "viewer"`). Every invited user must be registered in the Hub's `accepted_users` allowlist (`GET /v1/users`).
- `repos` & `references`: Attached repository slugs (`owner/name`) and reference URLs (`https://...`) included in the backing session's context header.
- `external_bindings`: Extensible external chat binding records (`provider`: e.g. `"google_chat"`, `external_id`: e.g. `"spaces/AAA..."`, `thread_id`, `metadata`) so external chat spaces can bind to the same Hub `ChatSession`.

---

## 2. Sender Attribution & Transcript Privacy

- When the backing agent is woken (via `@mention`, `trigger_agent: true`, or `reply_mode: "all"`), the Hub formats all human messages posted since `last_agent_seq` with explicit sender attribution:
  ```text
  [alice@example.com]: Should we increase the checkout retry backoff to 500ms?
  [pm@example.com]: Yes, let's also cap max retries at 3. @ops-agent please draft the change.
  ```
- In compliance with the Hub's no-code-at-rest invariant (`hub/security/atrest_test.go`), agent `transcript` events (`ClassToken`) are never persisted in SQLite/PostgreSQL tables at rest; they are streamed live from `hub/live` and merged into the session view in memory.

---

## 3. HTTP REST & SSE API

| Method & Path | Description |
| --- | --- |
| `GET /v1/chats` | List shared chat sessions the caller has access to (optional `?deployment=` filter). |
| `POST /v1/chats` | Create a shared chat session and its backing interactive task. Requires membership in `deployment`. |
| `GET /v1/chats/{id}` | Get session metadata, merged participants, external bindings, proposed tasks, and transcript. |
| `PATCH /v1/chats/{id}` | Update `title`, `reply_mode`, `linked_deployments`, `participants`, `repos`, `references`, or `external_bindings`. |
| `POST /v1/chats/{id}/messages` | Post `{id, text, attachments, trigger_agent}` as the authenticated caller (`acc.Email`), broadcast over SSE, and wake the backing agent when triggered. |
| `GET /v1/chats/{id}/events` | Live SSE stream (`message`, `transcript`, `awaiting_reply`, `approval_request`, `task_proposed`, `chat_updated`). |
| `GET /v1/inbox` | Surfaces `awaiting_reply`, `approval_request`, and `chat_mention` items (with `chat_id`) in participants' Unified Inbox. |
