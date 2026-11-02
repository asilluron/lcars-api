-- Reminders: the to-do list the display shows. Only lcars-api reads and
-- writes this table, with the service role (the secret key); browsers never
-- see it, so it gets no grants or policies for anon or authenticated.
--
-- A device may only change `done` on an existing row. The id is a lookup
-- key, never a filesystem path.

create table public.reminders (
  -- "rem_" and 12 lowercase letters or digits, such as "rem_7f3k2m9q4x8w".
  id text primary key check (id ~ '^rem_[a-z0-9]{12}$'),
  title text not null check (
    char_length(title) between 1 and 80
    and title !~ '[[:cntrl:]]'
  ),
  detail text not null default '' check (
    char_length(detail) <= 120
    and detail !~ '[[:cntrl:]]'
  ),
  accent text not null check (accent in ('purple', 'blue', 'orange', 'peach')),
  done boolean not null default false,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

comment on table public.reminders is
  'To-do rows for the LCARS display. Managed by lcars-api; see README.md.';

alter table public.reminders enable row level security;

revoke all on table public.reminders from anon, authenticated;
grant select, insert, update, delete on table public.reminders to service_role;

-- The display shows six rows. Reject a seventh even if two creates race.
create function public.reminders_before_insert()
returns trigger
language plpgsql
set search_path = ''
as $$
begin
  if (select count(*) from public.reminders) >= 6 then
    raise exception 'the reminder list is full (6)' using errcode = '23514';
  end if;
  return new;
end;
$$;

create trigger reminders_limit
  before insert on public.reminders
  for each row
  execute function public.reminders_before_insert();

-- A done/open change is a change to the list, so bump updated_at.
create function public.reminders_before_update()
returns trigger
language plpgsql
set search_path = ''
as $$
begin
  new.updated_at := pg_catalog.now();
  return new;
end;
$$;

create trigger reminders_touch
  before update on public.reminders
  for each row
  execute function public.reminders_before_update();

revoke all on function public.reminders_before_insert() from public, anon, authenticated;
revoke all on function public.reminders_before_update() from public, anon, authenticated;
