# ledger

## Replica lag alert

1. Check `pg_stat_replication` on the primary.
2. If lag is over 5 minutes, page the database on-call.
