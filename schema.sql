CREATE TABLE query_cache (
    id serial primary key,
    query text,
    body jsonb,
    tsrange int8range,
    step bigint
);
CREATE INDEX query_cache_query ON query_cache USING btree (query);
CREATE INDEX query_cache_tsrange ON query_cache USING gist (tsrange);
