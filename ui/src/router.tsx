// A small path router over the History API: the UI has a few pages,
// which do not need a routing library. The hub serves index.html for
// every path that is not its API's (hub/ui), so each route below is a
// URL a person can reload or share.
import { useEffect, useState, type MouseEvent, type ReactNode } from 'react';
import { Link as MuiLink, type LinkProps } from '@mui/material';

export type Route =
  | { page: 'inbox'; path?: '/' | '/inbox' }
  | { page: 'backlog'; team?: string }
  | { page: 'task'; id: string }
  | { page: 'grainfiles' }
  | { page: 'nodes' }
  | { page: 'file'; team?: string }
  | { page: 'teams' }
  | { page: 'team'; team: string }
  | { page: 'deployments' }
  | { page: 'deployment'; id: string }
  | { page: 'proposals'; team: string }
  | { page: 'notfound'; path: string };

export function parseRoute(path: string): Route {
  const parts = path.split('/').filter(Boolean).map(decodeURIComponent);
  if (parts.length === 0) return { page: 'inbox' };
  if (parts[0] === 'inbox' && parts.length === 1) return { page: 'inbox', path: '/inbox' };
  if (parts[0] === 'backlog' && parts.length === 1) return { page: 'backlog' };
  if (parts[0] === 'teams' && parts.length === 1) return { page: 'teams' };
  if (parts[0] === 'teams' && parts.length === 2) return { page: 'backlog', team: parts[1] };
  if (parts[0] === 'teams' && parts.length === 3 && parts[2] === 'members') return { page: 'team', team: parts[1] };
  if (parts[0] === 'teams' && parts.length === 3 && parts[2] === 'file') return { page: 'file', team: parts[1] };
  if (parts[0] === 'teams' && parts.length === 3 && parts[2] === 'proposals') return { page: 'proposals', team: parts[1] };
  if (parts[0] === 'deployments' && parts.length === 1) return { page: 'deployments' };
  if (parts[0] === 'deployments' && parts.length === 2) return { page: 'deployment', id: parts[1] };
  if (parts[0] === 'file' && parts.length === 1) return { page: 'file' };
  if (parts[0] === 'tasks' && parts.length === 2) return { page: 'task', id: parts[1] };
  if (parts[0] === 'grainfiles' && parts.length === 1) return { page: 'grainfiles' };
  if (parts[0] === 'nodes' && parts.length === 1) return { page: 'nodes' };
  return { page: 'notfound', path };
}

export function routePath(r: Route): string {
  switch (r.page) {
    case 'inbox':
      return r.path ?? '/';
    case 'backlog':
      return r.team ? `/teams/${encodeURIComponent(r.team)}` : '/backlog';
    case 'task':
      return `/tasks/${encodeURIComponent(r.id)}`;
    case 'grainfiles':
      return '/grainfiles';
    case 'nodes':
      return '/nodes';
    case 'file':
      return r.team ? `/teams/${encodeURIComponent(r.team)}/file` : '/file';
    case 'teams':
      return '/teams';
    case 'team':
      return `/teams/${encodeURIComponent(r.team)}/members`;
    case 'deployments':
      return '/deployments';
    case 'deployment':
      return `/deployments/${encodeURIComponent(r.id)}`;
    case 'proposals':
      return `/teams/${encodeURIComponent(r.team)}/proposals`;
    case 'notfound':
      return r.path;
  }
}

const listeners = new Set<() => void>();

export function navigate(path: string) {
  if (path === window.location.pathname) return;
  window.history.pushState(null, '', path);
  listeners.forEach((l) => l());
}

export function useRoute(): Route {
  const [path, setPath] = useState(window.location.pathname);
  useEffect(() => {
    const update = () => setPath(window.location.pathname);
    listeners.add(update);
    window.addEventListener('popstate', update);
    return () => {
      listeners.delete(update);
      window.removeEventListener('popstate', update);
    };
  }, []);
  return parseRoute(path);
}

// Link is an in-app link: a plain left click navigates without a load,
// and anything else (a new tab, a copy) is an ordinary link.
export function Link({ to, children, ...rest }: { to: string; children: ReactNode } & Omit<LinkProps, 'href'>) {
  const onClick = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  };
  return (
    <MuiLink href={to} onClick={onClick} {...rest}>
      {children}
    </MuiLink>
  );
}
