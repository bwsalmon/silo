-- The rest of the compute/v1 facade's price table (docs/silo/gcp-backend.md,
-- "GPUs", "Local SSD" and "Disks", SV-67 to SV-69; allocators.md, "GPU
-- classes", SV-28). The measures added:
--   gpus           an instance's GPUs, attached and bundled
--   gpus:<type>    its GPUs of one accelerator type
--   local_ssd_gb   its local SSD, attached and bundled, 375 GB a partition
--   iops           a disk's provisionedIops
--   throughput     a disk's provisionedThroughput
-- Every GPU type in gpu_classes gets a row, standard and Spot; one added
-- later (ledger.AddGPU) gets its row with it.
INSERT INTO facade_prices (resource, variant, measure, class, running) VALUES
    ('instance', 'STANDARD',             'gpus',         'gpus_all_regions',                TRUE),
    ('instance', 'SPOT',                 'gpus',         'gpus_all_regions',                TRUE),
    ('instance', 'STANDARD',             'local_ssd_gb', 'local_ssd_gb',                    FALSE),
    ('instance', 'SPOT',                 'local_ssd_gb', 'spot_local_ssd_gb',               FALSE),
    ('disk',     'pd-extreme',           'size_gb',      'ssd_disk_gb',                     FALSE),
    ('disk',     'pd-extreme',           'iops',         'pd_extreme_iops',                 FALSE),
    ('disk',     'hyperdisk-balanced',   'size_gb',      'hyperdisk_balanced_gb',           FALSE),
    ('disk',     'hyperdisk-balanced',   'iops',         'hyperdisk_balanced_iops',         FALSE),
    ('disk',     'hyperdisk-balanced',   'throughput',   'hyperdisk_balanced_throughput',   FALSE),
    ('disk',     'hyperdisk-extreme',    'size_gb',      'hyperdisk_extreme_gb',            FALSE),
    ('disk',     'hyperdisk-extreme',    'iops',         'hyperdisk_extreme_iops',          FALSE),
    ('disk',     'hyperdisk-throughput', 'size_gb',      'hyperdisk_throughput_gb',         FALSE),
    ('disk',     'hyperdisk-throughput', 'throughput',   'hyperdisk_throughput_throughput', FALSE),
    ('disk',     'hyperdisk-ml',         'size_gb',      'hyperdisk_ml_gb',                 FALSE),
    ('disk',     'hyperdisk-ml',         'throughput',   'hyperdisk_ml_throughput',         FALSE);

INSERT INTO facade_prices (resource, variant, measure, class, running)
    SELECT 'instance', CASE WHEN spot THEN 'SPOT' ELSE 'STANDARD' END, 'gpus:' || accelerator, class, TRUE
    FROM gpu_classes;
