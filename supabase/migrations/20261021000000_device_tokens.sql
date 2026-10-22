-- Device tokens: the credentials IoT devices use to reach the API (HTTP Basic
-- auth, or user/password on NATS). Only lcars-api reads and writes this table,
-- with the service role (the secret key); browsers never see it, so it gets no
-- grants or policies for anon or authenticated.
--
-- The secret itself is never stored: only its SHA-256, which is enough to
-- check it because secrets are 256-bit random values.

create table public.device_tokens (
  -- The Basic auth username, such as "dev_7f3k2m9q4x8w". Not secret.
  id text primary key check (id ~ '^dev_[a-z0-9]{12}$'),
  -- A label chosen by the admin, such as "Kitchen display".
  name text not null check (char_length(name) between 1 and 100),
  -- Hex SHA-256 of the secret (the Basic auth password).
  secret_hash text not null check (secret_hash ~ '^[0-9a-f]{64}$'),
  created_at timestamptz not null default now(),
  -- The admin who created it.
  created_by uuid references auth.users (id) on delete set null
);

comment on table public.device_tokens is
  'Credentials for lcars-api devices. Managed by lcars-api; see README.md.';

alter table public.device_tokens enable row level security;

revoke all on table public.device_tokens from anon, authenticated;
grant select, insert, delete on table public.device_tokens to service_role;
