# Google Chat Integration for Grain Hub Shared Chat Sessions

The Grain Hub (`hub/gchat`, `POST /v1/integrations/google-chat/events`, `PUT`/`DELETE /v1/chats/{id}/bindings/google-chat`) supports **bidirectional Google Chat integration** so any Hub [Shared Chat Session](shared-chats.md) can be linked to a Google Chat Space or Thread.

Participants in Google Chat and viewers in the Hub Web UI (`/chats/{id}`) share a single conversation transcript, turn-taking policy (`@mention` vs `all`), and interactive approval workflow.

---

## 1. Google Cloud & Google Workspace Chat App Setup

1. **Enable the Google Chat API:**
   - In the Google Cloud Console for your project, enable the **Google Chat API**.
2. **Create a Service Account for Outbound Delivery:**
   - Create a Google Cloud Service Account (under **IAM & Admin -> Service Accounts**) and download a JSON key file.
   - Google Chat automatically grants the service account permission to post and update messages (`https://www.googleapis.com/auth/chat.bot`) in spaces where the Chat App is installed.
3. **Configure the Google Chat App & Slash Command:**
   - Under **Google Chat API -> Configuration**:
     - **App name:** `Grain`
     - **Interactive features:** Enable **Receive 1:1 messages** and **Join spaces and group conversations**.
     - **Connection settings:** Select **HTTP endpoint URL** and set it to:
       ```text
       https://<your-hub-domain>/v1/integrations/google-chat/events
       ```
     - **Authentication Audience:** Note either your **Project Number** (for Google Chat system service account tokens issued by `chat@system.gserviceaccount.com`) or your **HTTP endpoint URL**, and pass the matching value to `--google-chat-audience`.
     - **Slash commands:** Register `/grain` (command ID `1`, **Opens a dialog**: unchecked) so users can run `/grain new`, `/grain link`, `/grain mode`, and `/grain status`.
4. **Routing Past IAP (if the Hub is behind Google Cloud IAP):**
   - Route `/v1/integrations/google-chat/events` to the non-IAP backend service (alongside `/v1/nodes/connect` and `/v1/webhooks/github`), as Google Chat signs each request with its own `Authorization: Bearer <jwt>` token verified directly by `hub/gchat`.

---

## 2. `silo hub serve` Configuration Flags

Pass the following flags (or environment variables) to `silo hub serve`:

| Flag | Environment Variable | Description |
| --- | --- | --- |
| `--google-chat-audience` | `SILO_HUB_GOOGLE_CHAT_AUDIENCE` | Expected JWT `aud` claim on `POST /v1/integrations/google-chat/events` (Cloud project number or webhook URL). When unset, all webhook requests are refused with `401`. |
| `--google-chat-certs-url` | `SILO_HUB_GOOGLE_CHAT_CERTS_URL` | Optional JWK Set URL override for verifying Google Chat JWTs (defaults to `https://www.googleapis.com/service_accounts/v1/metadata/jwk/chat@system.gserviceaccount.com`). |
| `--google-chat-service-account` | `SILO_HUB_GOOGLE_CHAT_SERVICE_ACCOUNT` | Path to the Google Cloud service account JSON key file (or inline JSON) used to mint OAuth2 `chat.bot` access tokens for outbound `POST /v1/{space}/messages` and `PATCH /v1/{message}` calls. |

---

## 3. Inbound Webhook Authentication & Accepted-User Authorization

Every request to `POST /v1/integrations/google-chat/events` undergoes two checks:

1. **Bearer JWT Verification:**
   - Verifies `Authorization: Bearer <token>` (RS256 or ES256) against Google Chat's published JWK Set (`--google-chat-certs-url` or `Config.GoogleChatKeys` in tests) and `--google-chat-audience`.
   - Missing, forged, expired, or wrong-audience tokens are rejected with `401 Unauthorized`.
2. **Accepted-User & Chat Access Authorization:**
   - Resolves `event.user.email` against the Hub's `accepted_users` allowlist (`directory.API.AccessForEmail`) and checks that the caller is authorized for the target Shared Chat Session or deployment.
   - If the sender is not in `accepted_users` or lacks access to the bound chat, the Hub returns `403 Forbidden` with a Google Chat Card v2 refusal message:
     > `"You are not in the Grain Hub accepted users list or do not have access to this chat"`

---

## 4. Slash Commands & Inbound Events

### 4.1 `ADDED_TO_SPACE` & `/grain` Slash Commands

- `/grain link <chat-id>` — Binds the current Google Chat space (and thread, when invoked inside a thread) to existing Hub Shared Chat Session `<chat-id>`, after verifying that `event.user.email` has participant/member access to `<chat-id>`.
- `/grain new <deployment> <title>` — Creates a new Hub Shared Chat Session backed by `<deployment>` (verifying `event.user.email` is a member of `<deployment>`) and binds it to the current Google Chat space/thread.
- `/grain mode mention|all` — Switches the bound Shared Chat Session's `reply_mode` between `mention` (only `@Grain` / `@agent` / `@<deployment>` wakes the agent) and `all` (every message wakes the agent).
- `/grain status` — Posts a summary Card v2 showing the bound Hub chat link (`/chats/<id>`), backing deployment online/session status, and current `reply_mode`.

### 4.2 `MESSAGE` Events

- Strips any leading `@Grain` bot mention from the message text, extracts any Google Chat file/media attachments, and appends the message (`Sender: event.user.email`) to the bound Hub Shared Chat Session.
- Broadcasts the message live over `GET /v1/chats/{id}/events` so Hub Web UI viewers see Google Chat messages immediately.
- Wakes the backing Grain agent when `reply_mode == "all"` or when the Google Chat message `@mentions` the Grain bot (`USER_MENTION` bot annotation or `@Grain`, `@agent`, `@<deployment>`).

### 4.3 `CARD_CLICKED` (Interactive Approvals in Google Chat)

- **In-run `approval_request`s (`resolve_approval`):** Clicking **Allow** or **Deny** on an interactive approval card records the authenticated clicker (`event.user.email`), delivers the decision to the running Grain session, broadcasts the update to Hub Web UI viewers, and updates the Google Chat card in-place (`actionResponse.type = "UPDATE_MESSAGE"` and `PATCH /v1/{message}`).
- **Proposed tasks (`resolve_proposal`):** Clicking **Approve** or **Reject** on a task proposal card approves (`queued`) or rejects (`rejected`) the proposed task attributed to `event.user.email` and updates the Google Chat card in-place.

---

## 5. Outbound Hub -> Google Chat Delivery

When a Shared Chat Session has a `"google_chat"` binding (`space_name` and optional `thread_name`):

- **Agent replies (`transcript`, `result`):** Posted to the bound Google Chat space/thread attributed to `[@<deployment>]`.
- **Clarifying questions (`awaiting_reply`):** Posted as a Card v2 prompt with a link to `/chats/<id>`.
- **In-run approval requests (`approval_request`) & task proposals (`task_proposed`):** Posted as interactive Cards v2 with **Allow / Deny** or **Approve / Reject** buttons and an **Open in Grain Hub** button.
- **Hub Web UI human messages (`POST /v1/chats/{id}/messages`):** Mirrored to the bound Google Chat space/thread attributed to `[<sender-email>]` so participants in Google Chat and the Hub Web UI see the exact same conversation.

---

## 6. REST API, TypeScript SDK & Web UI Controls

### REST Endpoints
- `PUT /v1/chats/{id}/bindings/google-chat` — Body: `{"space_name": "spaces/...", "thread_name": "spaces/.../threads/..."}`. Binds or updates the Google Chat space/thread for chat `{id}` and returns the updated `ChatSession`.
- `DELETE /v1/chats/{id}/bindings/google-chat` — Removes the Google Chat binding from chat `{id}` and returns the updated `ChatSession`.

### TypeScript SDK (`@silo/hub-sdk`)
```ts
await hub.bindGoogleChat(chatId, {
  space_name: 'spaces/AAA123',
  thread_name: 'spaces/AAA123/threads/t1',
});

await hub.unbindGoogleChat(chatId);
```

### Hub Web UI (`/chats/{id}`)
Each Shared Chat Session displays a **Google Chat** badge in the room header and sidebar. Clicking the badge opens the **Google Chat Integration** dialog showing:
- The linked Google Chat `space_name` and `thread_name`.
- A copyable `/grain link <chat-id>` command to paste into Google Chat.
- Direct **Bind Google Chat** and **Unbind Google Chat** controls.
