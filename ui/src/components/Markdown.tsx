// Copied from bwsalmon/grain ui/src/components/Markdown.jsx at commit
// 7781f615c603f43e9170cc8bb329a29ad0343c42 (docs/ui.md §2, §7), as
// TypeScript. The hub renders a task's prompt and an agent's words with
// it: text a submitter, a node or an agent wrote, and so never HTML or
// script in the hub's origin (docs/threat-model.md T-TB4-1, INV-15).
// Changes from grain's, each to keep that: raw HTML is dropped
// (skipHtml) rather than shown escaped; safeUrl also refuses a URL with
// whitespace or a control character in it; a refused link is left as
// text; an image is shown as a link rather than loaded; and the element styles are MUI sx rather than
// grain's style.css. Grain's own comments follow, with the task and
// file names they cite being grain's.

import { memo, type ReactNode } from "react";
import { Box, type SxProps, type Theme } from "@mui/material";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkBreaks from "remark-breaks";
import remarkGfm from "remark-gfm";
import { mono } from "../theme";

// Markdown renders a body of prose as the markdown it was almost
// certainly written in. Every agent grain dispatches writes markdown by
// default -- a relayed question comes back with headings, bullet lists,
// backticked paths and fenced diffs in it -- and rendering that as one
// flat run of text is what made a long answer hard to read
// (grain/task-93). It is used for the two places a task carries somebody
// else's words: a comment body and the task description, both of which a
// human writes in the same syntax when replying, so both get the same
// treatment rather than one rule for agents and another for people.
//
// Deliberately *not* used for an attempt's transcript
// (AttemptTranscriptOverlay): that is a log of thinking, text and tool
// calls interleaved, whose alignment is the whole of its legibility, and
// it stays in its <pre>.
//
// Nothing here renders raw HTML. react-markdown skips embedded HTML
// unless rehype-raw is added, which it is not, so a comment body -- a
// string that reaches the page straight off an agent or a GitHub user --
// cannot inject markup. In the hub an embedded tag is dropped outright
// (skipHtml) rather than shown as escaped text. That is the whole
// reason this goes through a parser rather than a hand-rolled regex
// pass. Do not add rehype-raw here.
//
// remarkBreaks alongside remarkGfm because these bodies are read as
// comments, not as documents: GitHub renders a single newline in a
// comment as a line break, and so does the plain-text rendering this
// replaces (.description's own white-space: pre-wrap). Without it, a
// hand-typed reply's line breaks would silently collapse into one
// paragraph -- a regression dressed up as a feature.
const REMARK_PLUGINS = [remarkGfm, remarkBreaks];

// The schemes a link or image in a body may carry. This is owned here,
// not left to react-markdown's defaultUrlTransform, so the policy that
// keeps a `javascript:`, `data:` or `vbscript:` URL out of an href
// cannot widen under us with a dependency bump, and it is
// narrower than the library's: nothing in a comment needs irc: or xmpp:.
const SAFE_PROTOCOL = /^(https?|mailto)$/i;

// safeUrl returns url unchanged when it is relative or carries an
// allowed scheme, and "" otherwise. The parser has already decoded
// character references and trimmed the destination by the time this
// sees it, so `jav&#x61;script:` arrives here as `javascript:`. A colon
// only names a scheme when it comes before any `/`, `?` or `#`;
// one after them is part of a relative path, query or fragment.
//
// The hub's copy also refuses whole any URL with whitespace or a
// control character left in it, since a browser drops some of them
// ("java\tscript:") before it reads the scheme.
export function safeUrl(url: string): string {
  const value = String(url ?? "");
  if (/[\u0000-\u0020\u007f-\u009f]/.test(value)) {
    return "";
  }
  if (value.startsWith("//") || value.startsWith("/\\") || value.startsWith("\\")) {
    return "";
  }
  const colon = value.indexOf(":");
  if (colon === -1) {
    return value;
  }
  for (const delimiter of ["/", "?", "#"]) {
    const at = value.indexOf(delimiter);
    if (at !== -1 && at < colon) {
      return value;
    }
  }
  return SAFE_PROTOCOL.test(value.slice(0, colon)) ? value : "";
}

// Links go to a new tab, the same as every other outbound link in the
// UI (DetailOverlay's own pull request links): the overlay they are read
// in is a transient thing, and navigating it away loses whatever the
// operator was in the middle of. A fragment is the exception: it goes
// nowhere outside this page, and the ones grain writes itself name a task
// the timeline opens in place (taskRefs.js).
//
// A link safeUrl refused is left as its text, not an <a href=""> that
// would reload the page. An image is never loaded: a body is somebody else's words, and an
// image in it is a request from the viewer's browser to wherever its
// author chose. The hub shows it as a link to the image instead.
const COMPONENTS: Components = {
  a: ({ node: _node, ...props }) =>
    !props.href ? (
      <span>{props.children}</span>
    ) : props.href.startsWith("#") ? (
      <a {...props} />
    ) : (
      <a {...props} target="_blank" rel="noopener noreferrer" />
    ),
  img: ({ src, alt }) =>
    typeof src === "string" && src ? (
      <a href={src} target="_blank" rel="noopener noreferrer">
        {alt || src}
      </a>
    ) : (
      <span>{alt}</span>
    ),
};

// The element styles for everything the parser can emit: grain's
// style.css `.markdown` rules, with its custom properties read from
// the MUI theme instead, since the hub has no stylesheet of its own.
const MARKDOWN_SX: SxProps<Theme> = {
  textTransform: "none",
  wordBreak: "break-word",
  "& *": { textTransform: "none" },
  "& > :first-child": { mt: 0 },
  "& > :last-child": { mb: 0 },
  "& p": { m: "0 0 0.6rem" },
  "& h1, & h2, & h3, & h4, & h5, & h6": {
    m: "0.9rem 0 0.4rem",
    fontWeight: 600,
    lineHeight: 1.3,
    color: "text.primary",
  },
  "& h1": { fontSize: "1.15em" },
  "& h2": { fontSize: "1.08em" },
  "& h3, & h4, & h5, & h6": { fontSize: "1em" },
  "& ul, & ol": { m: "0 0 0.6rem", pl: "1.4rem" },
  "& li": { my: "0.15rem" },
  "& li > ul, & li > ol": { mb: 0 },
  '& li:has(> input[type="checkbox"])': { listStyle: "none", ml: "-1.1rem" },
  '& li > input[type="checkbox"]': { mr: "0.4rem" },
  "& a": { color: "primary.main" },
  "& code": {
    fontFamily: mono,
    fontSize: "0.88em",
    bgcolor: "action.hover",
    borderRadius: "4px",
    p: "0.1em 0.3em",
  },
  "& pre": {
    m: "0 0 0.6rem",
    p: "0.6rem 0.75rem",
    bgcolor: "action.hover",
    border: 1,
    borderColor: "divider",
    borderRadius: "6px",
    overflowX: "auto",
  },
  "& pre code": { bgcolor: "transparent", borderRadius: 0, p: 0, fontSize: "0.85em" },
  "& blockquote": {
    m: "0 0 0.6rem",
    pl: "0.75rem",
    borderLeft: 3,
    borderColor: "divider",
    color: "text.secondary",
  },
  "& hr": { border: "none", borderTop: 1, borderColor: "divider", my: "0.9rem" },
  "& table": { display: "block", overflowX: "auto", borderCollapse: "collapse", m: "0 0 0.6rem" },
  "& th, & td": { border: 1, borderColor: "divider", p: "0.25rem 0.5rem", textAlign: "left" },
  "& th": { bgcolor: "action.hover", fontWeight: 600 },
};

// className is the caller's own styling hook, kept on the wrapper so
// the markdown block sits exactly where the plain <div> it replaced
// did; MARKDOWN_SX carries the element styles.
function Markdown({ children, className }: { children: string; className?: string }): ReactNode {
  return (
    <Box className={className ? `markdown ${className}` : "markdown"} sx={MARKDOWN_SX}>
      <ReactMarkdown
        remarkPlugins={REMARK_PLUGINS}
        components={COMPONENTS}
        urlTransform={safeUrl}
        skipHtml
      >
        {children}
      </ReactMarkdown>
    </Box>
  );
}

// memo, because this is the most expensive component in the UI and the
// cheapest one to compare: two props, both strings, so React's own
// shallow compare is two `===` against a remark/rehype pass over a body
// that can be kilobytes of prose (grain/task-174).
//
// What that buys is not a micro-optimisation, it is the difference
// between a screen that redraws and one that does not. Every place that
// renders many of these at once renders them *inside* something with
// state of its own that changes for unrelated reasons -- a task's
// timeline under an overlay that polls every three seconds, a chat
// transcript above a composer that sets state on every keystroke -- and
// without this, one character typed re-parsed every message in the
// conversation. Measured at 112ms of script per keystroke on a 41-message
// chat before this, growing linearly with the conversation:
// docs/hand-runs/2026-09-17-chat-keystroke.md.
//
// It is safe in the way memo usually is not, because there is nothing
// else in the props to get stale: no children beyond the string, no
// callbacks, no objects. A caller that passes a new body string gets a
// new render, which is the whole of what this component depends on.
export default memo(Markdown);
