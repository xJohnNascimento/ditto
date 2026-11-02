-- +goose Up

-- (arm_id, request_id) could not hold two consumptions by one request, and every scheduler
-- transaction shares one request id; a surrogate key lets an arm with several uses fire any number
-- of times.
alter table scenario_consumption drop constraint scenario_consumption_pkey;
alter table scenario_consumption add column id bigint generated always as identity primary key;
create index scenario_consumption_arm on scenario_consumption (arm_id);
