// A refusal from the hub's API is an ApiError carrying the HTTP status
// and the API's own words. The common statuses have subclasses, so a
// caller can catch the one it handles; every one is an ApiError.

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

// 400: the request itself is wrong (a bad field, a bad cursor).
export class BadRequestError extends ApiError {
  constructor(message: string) {
    super(400, message);
    this.name = 'BadRequestError';
  }
}

// 401: no one is signed in, or the token was not accepted.
export class UnauthenticatedError extends ApiError {
  constructor(message: string) {
    super(401, message);
    this.name = 'UnauthenticatedError';
  }
}

// 403: the caller is signed in and may not do this.
export class ForbiddenError extends ApiError {
  constructor(message: string) {
    super(403, message);
    this.name = 'ForbiddenError';
  }
}

// 404: there is no such thing, or the caller may not see it: the hub
// answers the two alike.
export class NotFoundError extends ApiError {
  constructor(message: string) {
    super(404, message);
    this.name = 'NotFoundError';
  }
}

// 409: not allowed in the thing's current state (cancelling a finished
// task, approving one that is not proposed).
export class ConflictError extends ApiError {
  constructor(message: string) {
    super(409, message);
    this.name = 'ConflictError';
  }
}

// 503: the hub, or the node behind it, cannot answer now (a node-only
// artifact whose node is offline).
export class UnavailableError extends ApiError {
  constructor(message: string) {
    super(503, message);
    this.name = 'UnavailableError';
  }
}

// errorFor is the ApiError for status, the subclass where there is one.
export function errorFor(status: number, message: string): ApiError {
  switch (status) {
    case 400:
      return new BadRequestError(message);
    case 401:
      return new UnauthenticatedError(message);
    case 403:
      return new ForbiddenError(message);
    case 404:
      return new NotFoundError(message);
    case 409:
      return new ConflictError(message);
    case 503:
      return new UnavailableError(message);
    default:
      return new ApiError(status, message);
  }
}

// check answers r when it is a success, and throws its ApiError when
// it is not.
export async function check(r: Response): Promise<Response> {
  if (!r.ok) {
    const text = (await r.text().catch(() => '')).trim();
    throw errorFor(r.status, text || `${r.status} ${r.statusText}`);
  }
  return r;
}
