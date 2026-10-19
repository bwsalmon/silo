// @silo/hub-sdk: a client for the Silo hub's public API (docs/hub.md),
// for browsers and Node. README.md has examples.
export { HubClient, type HubClientOptions, type TokenSource } from './client.js';
export {
  ApiError,
  BadRequestError,
  ConflictError,
  ForbiddenError,
  NotFoundError,
  UnauthenticatedError,
  UnavailableError,
  errorFor,
} from './errors.js';
export {
  addEvent,
  SSEParser,
  type EventStream,
  type FollowOptions,
  type SSEMessage,
  type StreamState,
  type SubscribeChatEventsOptions,
  type Transport,
} from './events.js';
export * from './models.js';
