// Copied from bwsalmon/grain ui/src/phone.js at commit
// 7781f615c603f43e9170cc8bb329a29ad0343c42 (docs/ui.md §2, §7), as
// TypeScript. Used by the copied DiffView to pick its default layout; the hub has no
// phone shell or stylesheet of its own to share the query with. Grain's own comments follow, with the task and
// file names they cite being grain's.

import { useMediaQuery } from "@mui/material";

// Is this a phone? The whole UI is built around a permanent 232px nav
// rail beside a pane (see the shell in style.css), which a tablet has
// the width for and a phone does not: on a 390px screen the rail is most
// of the window, and what is left is too narrow to read a task in. So
// one question is asked here, once, and both the layout (App.jsx's
// PhoneNav, Overlay.jsx's pane width) and the stylesheet answer it the
// same way.
//
// Deliberately a question about the *viewport*, not about the device:
// there is no honest way to ask a browser what it is running on, and a
// narrow window on a desktop wants the narrow layout anyway. The two
// clauses are the two shapes a phone actually presents:
//
//   - under 600px wide. MUI's own `sm` breakpoint, and the width of
//     every phone in portrait; the smallest tablet in portrait (768px)
//     is comfortably above it.
//   - short and landscape. A phone turned sideways is wide (a modern
//     one is 850px or more) but only ~400px tall, which is less room
//     than the rail and a task's own header need between them. A tablet
//     in landscape is 768px tall or more, so it never matches this.
//
// style.css carries the same two clauses verbatim for the rules that are
// pure CSS -- the two are edited together, the way theme.js and
// style.css's own tokens are.
export const PHONE_QUERY: string =
  "(max-width: 599.95px), (max-height: 480px) and (orientation: landscape)";

// useIsPhone re-renders its caller when the answer changes, so rotating
// a phone or dragging a desktop window narrow switches layouts live
// rather than at the next reload.
//
// jsdom has no matchMedia, and MUI's useMediaQuery answers `false` when
// the browser cannot be asked -- so every existing test, and any server
// render, gets the desktop shell rather than throwing. A test that wants
// the phone shell stubs window.matchMedia (see PhoneNav.test.jsx).
export function useIsPhone(): boolean {
  return useMediaQuery(PHONE_QUERY);
}
