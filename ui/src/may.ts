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

// memberOf is whether the caller is in team, in either role: who the
// API lets approve or reject its proposals and answer its tasks'
// approval requests. A hub admin outside the team is not.
export function memberOf(me: Me | undefined, team: string): boolean {
  return !!me?.teams?.[team];
}

export function mayApprove(me: Me | undefined, t: Task): boolean {
  return memberOf(me, t.team) && me!.email.toLowerCase() !== t.submitter.toLowerCase();
}

// mayMessage is whether the caller may send an interactive task a
// message: its submitter, or a member of its team in either role. A hub
// admin outside the team may watch the session but not talk to it.
export function mayMessage(me: Me | undefined, t: Task): boolean {
  return t.kind === 'interactive' && !!me && (me.email.toLowerCase() === t.submitter.toLowerCase() || memberOf(me, t.team));
}
