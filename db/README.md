# db

The platform's PostgreSQL schema. See `docs/platform-brief.md` for what it is for.

    db/migrate.sh     # apply migrations/*.sql, in order, to assetcracker_dev on the Pi
    db/test.sh        # run tests/*.sql there; every test rolls itself back
    db/reset-dev.sh   # empty a _dev database (refuses anything else, or one holding ledger entries)

Postgres only listens on the Pi, so both scripts pipe SQL over SSH as `acdeploy`. Nothing
runs on, or is installed on, the machine you launch them from.

## What the database enforces by itself

These hold even if the service has a bug, because they are constraints and triggers:

- **A transfer balances.** Its entries sum to zero cents, and it has at least two. Checked at
  commit.
- **Sim and real never meet.** An entry's mode must equal its transfer's and its account's;
  a bucket, its venue account and its cash must share one mode. Composite foreign keys, no
  application logic.
- **A bucket is one virtual subdivision of one venue account.** It has exactly one ledger
  account of kind `bucket`, which no other bucket can share.
- **The record is append-only.** Ledger, fills, settlements, commentary, weights and bucket
  events refuse UPDATE and DELETE. A correction is a new row.
- **One common pool and one profit pool per mode.**

`tests/0001_ledger_rules.sql` proves each of these by attempting the violation.

Limit to know about: a table's owner can disable its triggers. The dev database is owned by
the deploy user, which is fine for development. For the real database, the service should
connect as a role that does not own the tables. Not set up yet.

## Shape

| Area | Tables |
|---|---|
| Who acts | `actor` |
| Where things trade | `source`, `instrument`, `market` |
| Strategies and the trials registry | `strategy`, `strategy_version` |
| Money | `ledger_account`, `ledger_transfer`, `ledger_entry`, view `ledger_balance` |
| Capital | `venue_account`, `bucket`, `bucket_event`, view `venue_account_virtual_cash` |
| Time series, partitioned by month | `price_tick`, `evaluation`, `decision` |
| Trading | `trade_order`, `fill`, `settlement` |
| People's input | `commentary`, `human_weight`, view `current_human_weight` |
| Agents propose, people approve | `proposal` |
| Scores and gate decisions | `metric_snapshot` |

The decision journal is two tables. `evaluation` holds what was known about a market at one
moment, once. `decision` holds what each strategy made of it, whether or not it acted, with the
size it would have chosen alone next to the size after any human weight.
