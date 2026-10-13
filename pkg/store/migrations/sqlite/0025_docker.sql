-- The Docker facade's objects and the local backend's quota classes
-- (docs/silo/local-backend.md, §4 "The Docker facade" and §11
-- "Classes", call L19). Package pkg/facade/local/docker owns
-- docker_objects.
--
-- docker_objects: one row per container, network, volume or image
-- record a silo holds. Names are the silo's own (call L7): (silo, kind,
-- name) is unique, and a silo never sees another's. id is the 64 hex
-- characters silod made (a volume's is its name, an image's its digest
-- without sha256:). body is the object as the facade keeps it, in JSON,
-- read and written only in Go.
CREATE TABLE docker_objects (
    silo       TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    id         TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    body       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (silo, kind, id),
    UNIQUE (silo, kind, name)
);

-- The local backend's classes. Regions don't apply on one host, so
-- every one is global. The root's capacity of each is set by the host's
-- capacity reader, or given at bootstrap; a root without it refuses
-- every charge.
INSERT INTO quota_classes (kind, name, unit, sort, scope, period) VALUES
    ('local', 'vcpu_m',         'mvCPU', 'held', 'global', ''),
    ('local', 'memory_mb',      'MiB',   'held', 'global', ''),
    ('local', 'disk_gb',        'GiB',   'held', 'global', ''),
    ('local', 'containers',     'count', 'held', 'global', ''),
    ('local', 'networks',       'count', 'held', 'global', ''),
    ('local', 'volumes',        'count', 'held', 'global', '');
