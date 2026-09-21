#!/usr/bin/env bash
# Show or set the SUSTAINMENT ALLOCATION: what share of a bucket's gain above its high-water mark
# the platform moves into each money bucket. It is ours. "tax" below means only the part set aside
# for government taxes, into the tax reserve. Percentages; what is left stays in the bucket and compounds.
#
#   tools/skim-policy.sh                                  # show the policy in force and the pools (prod)
#   tools/skim-policy.sh set 20 10 25 0 "first guess"     # winnings, replenishment, tax reserve, fee reserve, then a note
#   AC_DB=assetcracker_dev tools/skim-policy.sh ...       # the dev database instead
#
# A change is a new dated row; past skims keep pointing at the policy that set them. The running
# service picks the new policy up at the next settlement, with no restart. Simulated money only:
# this writes the 'sim' policy. There is no 'real' policy and no real account.
set -euo pipefail
HOST="${AC_PI:-acdeploy@rpi-v5-1.local}"
DB="${AC_DB:-assetcracker}"
WHO="${AC_WHO:-brad}"
# The real database's tables belong to a no-login owner role, reached through psql as postgres.
# A _dev database belongs to the deploy user itself.
case "${DB}" in
  *_dev) ROLE=""; run() { ssh -o BatchMode=yes "${HOST}" psql -X -q -v ON_ERROR_STOP=1 -d "${DB}" -f -; } ;;
  *)     ROLE="set role assetcracker_owner;"; run() { ssh -o BatchMode=yes "${HOST}" sudo -n -u postgres psql -X -q -v ON_ERROR_STOP=1 -d "${DB}" -f -; } ;;
esac

if [ "${1:-}" = "set" ]; then
  [ $# -ge 5 ] || { echo "usage: $0 set <winnings%> <replenishment%> <tax reserve%> <fee reserve%> [note]"; exit 2; }
  for n in "$2" "$3" "$4" "$5"; do [[ "$n" =~ ^[0-9]+(\.[0-9]{1,2})?$ ]] || { echo "not a percentage: $n"; exit 2; }; done
  note="${6:-}"
  run <<SQL
${ROLE}
insert into skim_policy (mode, winnings_bps, replenish_bps, tax_bps, fees_bps, note, set_by)
select 'sim', round($2 * 100), round($3 * 100), round($4 * 100), round($5 * 100), \$note\$${note}\$note\$, id from actor where handle = '${WHO}';
SQL
fi
run <<'SQL'
\pset border 1
select effective_at::timestamp(0) as since, (winnings_bps / 100.0)::numeric(5,2) as "winnings %", (replenish_bps / 100.0)::numeric(5,2) as "replenishment %",
       (tax_bps / 100.0)::numeric(5,2) as "tax reserve %", (fees_bps / 100.0)::numeric(5,2) as "fee reserve %",
       ((10000 - winnings_bps - replenish_bps - tax_bps - fees_bps) / 100.0)::numeric(5,2) as "stays in bucket %", a.handle as set_by, note
  from skim_policy p join actor a on a.id = p.set_by where mode = 'sim' order by p.id desc limit 3;
select case kind when 'profit_pool' then 'winnings' when 'common_pool' then 'replenishment' when 'tax_reserve' then 'tax reserve'
                 when 'fee_reserve' then 'fee reserve' end as bucket, to_char(balance_cents / 100.0, 'FM$999,999,990.00') as balance
  from ledger_balance where mode = 'sim' and kind in ('profit_pool', 'common_pool', 'tax_reserve', 'fee_reserve') order by 1;
SQL
