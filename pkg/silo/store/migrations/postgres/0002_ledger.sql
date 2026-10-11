-- The silo tree and the quota ledger (docs/silo/allocators.md, SV-27 to
-- SV-37; docs/silo/custom-allocators.md, "What v1 must already allow").
-- Package pkg/silo/tree owns silos; package pkg/silo/ledger owns the
-- rest. The same as sqlite/0002_ledger.sql, with BIGINT where SQLite's
-- INTEGER is 64-bit.

-- One row per silo. parent is '' for the root. path is the names from
-- the root (/team-a/ci), for people; everything durable uses id.
CREATE TABLE silos (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL,
    parent     TEXT    NOT NULL,
    depth      INTEGER NOT NULL,
    path       TEXT    NOT NULL,
    backing    TEXT    NOT NULL,
    state      TEXT    NOT NULL,
    labels     TEXT    NOT NULL DEFAULT '{}',
    created_at BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL
);
CREATE UNIQUE INDEX silos_parent_name ON silos (parent, name);
CREATE UNIQUE INDEX silos_path ON silos (path);

-- Allowed regions: always a subset of the parent's.
CREATE TABLE silo_regions (
    silo   TEXT NOT NULL,
    region TEXT NOT NULL,
    PRIMARY KEY (silo, region)
);

-- The class table. Kind and class are data: nothing switches on a
-- class name, and no table has a column per class.
--   sort:   held | metered
--   scope:  regional | global
--   period: '' (none), day, week or month, for a metered class
CREATE TABLE quota_classes (
    kind   TEXT NOT NULL,
    name   TEXT NOT NULL,
    unit   TEXT NOT NULL,
    sort   TEXT NOT NULL,
    scope  TEXT NOT NULL,
    period TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (kind, name)
);

-- silod's GPU config: accelerator type to GCE metric to class, one row
-- for standard use and one for Spot. A class from this table has quota
-- only in the regions whose host-project quotas carry the metric.
CREATE TABLE gpu_classes (
    accelerator TEXT    NOT NULL,
    spot        BOOLEAN NOT NULL,
    metric      TEXT    NOT NULL,
    class       TEXT    NOT NULL,
    PRIMARY KEY (accelerator, spot)
);

-- One row per silo, kind, class and region ('global' for a global
-- class). Amounts are integer millionths of the class's unit.
-- granted is kept in step with the allocation edges into the silo,
-- delegated with the edges out of it, and used with the charges it
-- pays. consumed is for metered classes and is 0 in v1.
CREATE TABLE quota (
    silo           TEXT    NOT NULL,
    kind           TEXT    NOT NULL,
    class          TEXT    NOT NULL,
    region         TEXT    NOT NULL,
    granted        BIGINT  NOT NULL DEFAULT 0,
    delegated      BIGINT  NOT NULL DEFAULT 0,
    used           BIGINT  NOT NULL DEFAULT 0,
    consumed       BIGINT  NOT NULL DEFAULT 0,
    over_committed BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (silo, kind, class, region)
);

-- Allocation edges. granted is their sum into a silo. In v1 the only
-- edge into a silo is from its parent, and the root's capacity is an
-- edge from '' (the host project, or install's setting).
CREATE TABLE allocations (
    from_silo  TEXT    NOT NULL,
    to_silo    TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    class      TEXT    NOT NULL,
    region     TEXT    NOT NULL,
    amount     BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL,
    PRIMARY KEY (from_silo, to_silo, kind, class, region)
);
CREATE INDEX allocations_to ON allocations (to_silo, kind, class, region);

-- Charges: what each object holds, and who pays for it. v1 writes
-- payer = home. Refunds and the recount of used group by payer.
CREATE TABLE charges (
    object     TEXT    NOT NULL,
    payer      TEXT    NOT NULL,
    home       TEXT    NOT NULL,
    via        TEXT    NOT NULL,
    kind       TEXT    NOT NULL,
    class      TEXT    NOT NULL,
    region     TEXT    NOT NULL,
    amount     BIGINT  NOT NULL,
    created_at BIGINT  NOT NULL,
    updated_at BIGINT  NOT NULL,
    PRIMARY KEY (object, payer, kind, class, region)
);
CREATE INDEX charges_payer ON charges (payer, kind, class, region);
CREATE INDEX charges_home ON charges (home, region);

INSERT INTO gpu_classes (accelerator, spot, metric, class) VALUES
    ('nvidia-tesla-t4',       FALSE, 'NVIDIA_T4_GPUS',                    'nvidia_t4_gpus'),
    ('nvidia-tesla-t4',       TRUE,  'PREEMPTIBLE_NVIDIA_T4_GPUS',        'spot_nvidia_t4_gpus'),
    ('nvidia-tesla-t4-vws',   FALSE, 'NVIDIA_T4_VWS_GPUS',                'nvidia_t4_vws_gpus'),
    ('nvidia-tesla-t4-vws',   TRUE,  'PREEMPTIBLE_NVIDIA_T4_VWS_GPUS',    'spot_nvidia_t4_vws_gpus'),
    ('nvidia-l4',             FALSE, 'NVIDIA_L4_GPUS',                    'nvidia_l4_gpus'),
    ('nvidia-l4',             TRUE,  'PREEMPTIBLE_NVIDIA_L4_GPUS',        'spot_nvidia_l4_gpus'),
    ('nvidia-l4-vws',         FALSE, 'NVIDIA_L4_VWS_GPUS',                'nvidia_l4_vws_gpus'),
    ('nvidia-l4-vws',         TRUE,  'PREEMPTIBLE_NVIDIA_L4_VWS_GPUS',    'spot_nvidia_l4_vws_gpus'),
    ('nvidia-tesla-a100',     FALSE, 'NVIDIA_A100_GPUS',                  'nvidia_a100_gpus'),
    ('nvidia-tesla-a100',     TRUE,  'PREEMPTIBLE_NVIDIA_A100_GPUS',      'spot_nvidia_a100_gpus'),
    ('nvidia-a100-80gb',      FALSE, 'NVIDIA_A100_80GB_GPUS',             'nvidia_a100_80gb_gpus'),
    ('nvidia-a100-80gb',      TRUE,  'PREEMPTIBLE_NVIDIA_A100_80GB_GPUS', 'spot_nvidia_a100_80gb_gpus'),
    ('nvidia-tesla-v100',     FALSE, 'NVIDIA_V100_GPUS',                  'nvidia_v100_gpus'),
    ('nvidia-tesla-v100',     TRUE,  'PREEMPTIBLE_NVIDIA_V100_GPUS',      'spot_nvidia_v100_gpus'),
    ('nvidia-tesla-p100',     FALSE, 'NVIDIA_P100_GPUS',                  'nvidia_p100_gpus'),
    ('nvidia-tesla-p100',     TRUE,  'PREEMPTIBLE_NVIDIA_P100_GPUS',      'spot_nvidia_p100_gpus'),
    ('nvidia-tesla-p100-vws', FALSE, 'NVIDIA_P100_VWS_GPUS',              'nvidia_p100_vws_gpus'),
    ('nvidia-tesla-p100-vws', TRUE,  'PREEMPTIBLE_NVIDIA_P100_VWS_GPUS',  'spot_nvidia_p100_vws_gpus'),
    ('nvidia-tesla-p4',       FALSE, 'NVIDIA_P4_GPUS',                    'nvidia_p4_gpus'),
    ('nvidia-tesla-p4',       TRUE,  'PREEMPTIBLE_NVIDIA_P4_GPUS',        'spot_nvidia_p4_gpus'),
    ('nvidia-tesla-p4-vws',   FALSE, 'NVIDIA_P4_VWS_GPUS',                'nvidia_p4_vws_gpus'),
    ('nvidia-tesla-p4-vws',   TRUE,  'PREEMPTIBLE_NVIDIA_P4_VWS_GPUS',    'spot_nvidia_p4_vws_gpus');

INSERT INTO quota_classes (kind, name, unit, sort, scope, period) VALUES
    ('gce', 'vcpu',                            'vCPU',  'held', 'regional', ''),
    ('gce', 'memory_mb',                       'MiB',   'held', 'regional', ''),
    ('gce', 'spot_vcpu',                       'vCPU',  'held', 'regional', ''),
    ('gce', 'spot_memory_mb',                  'MiB',   'held', 'regional', ''),
    ('gce', 'instances',                       'count', 'held', 'regional', ''),
    ('gce', 'standard_disk_gb',                'GB',    'held', 'regional', ''),
    ('gce', 'ssd_disk_gb',                     'GB',    'held', 'regional', ''),
    ('gce', 'pd_extreme_iops',                 'IOPS',  'held', 'regional', ''),
    ('gce', 'hyperdisk_balanced_gb',           'GB',    'held', 'regional', ''),
    ('gce', 'hyperdisk_balanced_iops',         'IOPS',  'held', 'regional', ''),
    ('gce', 'hyperdisk_balanced_throughput',   'MiB/s', 'held', 'regional', ''),
    ('gce', 'hyperdisk_extreme_gb',            'GB',    'held', 'regional', ''),
    ('gce', 'hyperdisk_extreme_iops',          'IOPS',  'held', 'regional', ''),
    ('gce', 'hyperdisk_throughput_gb',         'GB',    'held', 'regional', ''),
    ('gce', 'hyperdisk_throughput_throughput', 'MiB/s', 'held', 'regional', ''),
    ('gce', 'hyperdisk_ml_gb',                 'GB',    'held', 'regional', ''),
    ('gce', 'hyperdisk_ml_throughput',         'MiB/s', 'held', 'regional', ''),
    ('gce', 'local_ssd_gb',                    'GB',    'held', 'regional', ''),
    ('gce', 'spot_local_ssd_gb',               'GB',    'held', 'regional', ''),
    ('gce', 'subnetworks',                     'count', 'held', 'global',   ''),
    ('gce', 'firewall_rules',                  'count', 'held', 'global',   ''),
    ('gce', 'images',                          'count', 'held', 'global',   ''),
    ('gce', 'snapshots',                       'count', 'held', 'global',   ''),
    ('gce', 'image_storage_gb',                'GB',    'held', 'global',   ''),
    ('gce', 'snapshot_storage_gb',             'GB',    'held', 'global',   ''),
    ('gce', 'instance_templates',              'count', 'held', 'global',   ''),
    ('gce', 'gpus_all_regions',                'GPU',   'held', 'global',   ''),
    ('gce', 'agent_sessions',                  'count', 'held', 'global',   ''),
    ('model', 'usd',                           'USD',   'metered', 'global', 'month');

INSERT INTO quota_classes (kind, name, unit, sort, scope, period)
    SELECT 'gce', class, 'GPU', 'held', 'regional', '' FROM gpu_classes;
