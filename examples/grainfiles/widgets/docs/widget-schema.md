# Widget Binary Schema

## Wire framing

Every encoded `Widget` payload begins with a fixed framing header followed by the length-prefixed widget name.

## Schema version header

Version 2 prefixes every encoded widget with the two-byte magic `0x57 0x02` before the length-prefixed `Name` field. Decoders must continue to accept version 1 payloads that lack the magic prefix during the migration window.
