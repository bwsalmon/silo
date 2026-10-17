---
kind: ref
schema: grain/ref@v1
to: "widgets:.grain/tasks/2026-10-16-widget-schema-v2.task.md"
spec_rev: 7e2d4a1c3b5f6e8d9a0b1c2d3e4f5a6b7c8d9e0f   # the widgets commit the promotion merged as
rank: "w"
added_by: silo
added_at: "2026-10-16T17:55:00Z"
attestation: eyJhbGciOiJFZERTQSIsImtpZCI6ImNsYWltcy0yMDI2LTEwIiwidHlwIjoiSldUIn0.eyJpc3MiOiJodHRwczovL3NpbG8uZXhhbXBsZS5jb20iLCJhdWQiOiJncmFpbjpldSIsInRlYW0iOiJodWIiLCJhcHByb3ZlZF9ieSI6InVzZXI6YXBwcm92ZXJAZXhhbXBsZS5jb20iLCJ0YXNrIjoiaHR0cHM6Ly9zaWxvLmV4YW1wbGUuY29tL2dpdC9odWIvaW50YWtlLmdpdC8vdGFza3MvMjAyNi0xMC0xNi13aWRnZXQtc2NoZW1hLXYyL3Rhc2subWQiLCJzcGVjX3JldiI6IjlhOGI3YzZkNWU0ZjMwMjExZjJlM2Q0YzViNmE3OTg4MTIyMzM0NDUifQ.ZXhhbXBsZS1zaWduYXR1cmUtbm90LXZhbGlk
---

A reference into a code repo. Silo placed the task at 17:55, from its
intake address, while the promotion was still in review. When the
promotion merged and the intake path became a tombstone, Silo rewrote
`to:` to the end of the chain and `spec_rev` to the commit the task
landed in there. The attestation is unchanged: it still names the task's
intake identity, which redirects to `to:`, and the spec at the new
address is the one its `spec_digest` covers (docs/task-repos.md §5.5,
§11.7). Its neighbours in this folder
point into the intake; a backlog does not care where a task lives.
