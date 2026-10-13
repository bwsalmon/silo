-- Private upstream content in the pull-through cache
-- (docs/silo/local-registry.md, §8 "Private upstream images", call
-- L72). Package pkg/registry owns these columns.
--
-- private is whether the upstream wanted credentials for the object
-- when it was last asked anonymously, classified_at when that was. An
-- object classified longer ago than public_ttl is asked again. A
-- private object is served only after the caller's own credentials
-- revalidate it (a manifest) or through an authorisation for a manifest
-- that references it (a blob); authorisations are held in silod's
-- memory and never here, and no credential or token is ever stored.
-- Both indexes have them: the whole host's and the isolated sharing
-- domains' (0028). Rows from before this step are public, classified
-- at 0, so they are asked again on their next request.
ALTER TABLE registry_cache_objects ADD COLUMN private INTEGER NOT NULL DEFAULT 0;
ALTER TABLE registry_cache_objects ADD COLUMN classified_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE registry_cache_domain_objects ADD COLUMN private INTEGER NOT NULL DEFAULT 0;
ALTER TABLE registry_cache_domain_objects ADD COLUMN classified_at BIGINT NOT NULL DEFAULT 0;
