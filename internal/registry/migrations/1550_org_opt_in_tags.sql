-- The AI opt-in tags of an organization (internal/api/identity.go): what Studio's PATCH
-- /platform/organizations/{slug} saves when an Owner picks how much of the database the AI Assistant
-- may send to the model provider (AI_SQL_GENERATOR_OPT_IN: schema, AI_LOG_GENERATOR_OPT_IN: logs,
-- AI_DATA_GENERATOR_OPT_IN: data). Range 1550-1559 belongs to the AI settings of Studio. Additive
-- only (invariant I6): a release one minor behind never reads or writes the column and keeps working.
--
-- An empty array is the default, as on hosted: the organization has not opted in, and Studio asks.

alter table supavise.organizations add column opt_in_tags text[] not null default '{}';
