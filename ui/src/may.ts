// What /v1/me says the caller may do, read only to decide which buttons
// to show. It mirrors the API's rules (hub/directory.Access, hub/tasks)
// and decides nothing: the API answers every write itself, and a
// button shown here can still be refused there.
import type { Me, Task } from './api';

export function adminOf(me: Me | undefined, team: string): boolean {
  return !!me && (me.hub_admin || me.teams?.[team] === 'admin');
}

export function managesTask(me: Me | undefined, t: Task): boolean {
  return !!me && (me.email.toLowerCase() === t.submitter.toLowerCase() || adminOf(me, t.team));
}
