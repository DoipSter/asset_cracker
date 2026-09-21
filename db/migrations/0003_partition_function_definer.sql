-- The service must be able to create next month's partitions without owning any table.
-- Run as its owner, with a fixed search path so it cannot be pointed at another schema.
alter function ensure_month_partitions(date) security definer set search_path = public, pg_temp;
