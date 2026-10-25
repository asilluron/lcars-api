-- Display names and lanes for calendars read from .ics files. Only lcars-api
-- reads and writes this table, with the service role (the secret key);
-- browsers never see it, so it gets no grants or policies for anon or
-- authenticated.
--
-- id is the calendar's stable key: "mail:" plus the mailbox in a Google
-- Calendar link, or "file:" plus the file's path under calendar.dir. name is
-- the label shown on the panel; empty means use the source derived from the
-- file. category is the panel lane.

create table public.calendar_labels (
  id text primary key check (char_length(id) between 1 and 200 and id !~ '[[:cntrl:]]'),
  name text not null default '' check (char_length(name) <= 40 and name !~ '[[:cntrl:]]'),
  category text not null default '' check (category in ('', 'work', 'band', 'personal', 'house')),
  updated_at timestamptz not null default now()
);

comment on table public.calendar_labels is
  'Display names and lanes for lcars-api calendars. Managed by lcars-api; see README.md.';

alter table public.calendar_labels enable row level security;

revoke all on table public.calendar_labels from anon, authenticated;
grant select, insert, update, delete on table public.calendar_labels to service_role;
