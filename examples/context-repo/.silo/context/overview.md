# Payments systems overview

- **ledger**: the system of record for balances. Postgres, primary in
  us-east1.
- **psp-gateway**: talks to payment service providers (PSPs). Retries are
  idempotent on the `Idempotency-Key` header.
- Runbooks live in `runbooks/`, one Markdown file per system.
