-- The compute/v1 facade's price table (docs/silo/gcp-backend.md,
-- "Instances" and "Disks", SV-66 and SV-69; allocators.md, "Classes").
-- Each row says that an object of a resource (instance, disk, image,
-- snapshot) and variant (an instance's provisioning model, STANDARD or
-- SPOT; a disk's type) charges one class by one measure of the object:
--   count       1 per object
--   guest_cpus  the machine type's guestCpus
--   memory_mb   the machine type's memoryMb
--   size_gb     a disk's sizeGb
--   storage_gb  an image's archiveSizeBytes or a snapshot's
--               storageBytes, in GB rounded up
-- running is set where the charge is held only while the instance isn't
-- TERMINATED (the usage clock follows GCE's, call 6). The facade looks
-- classes up here and in quota_classes, never by name in code, so a new
-- disk type, or an instance's GPUs and local SSD, are new rows (and,
-- for a new kind of quantity, a new measure).
CREATE TABLE facade_prices (
    resource TEXT    NOT NULL,
    variant  TEXT    NOT NULL,
    measure  TEXT    NOT NULL,
    class    TEXT    NOT NULL,
    running  BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (resource, variant, measure, class)
);

INSERT INTO facade_prices (resource, variant, measure, class, running) VALUES
    ('instance', 'STANDARD',    'guest_cpus', 'vcpu',                TRUE),
    ('instance', 'STANDARD',    'memory_mb',  'memory_mb',           TRUE),
    ('instance', 'STANDARD',    'count',      'instances',           FALSE),
    ('instance', 'SPOT',        'guest_cpus', 'spot_vcpu',           TRUE),
    ('instance', 'SPOT',        'memory_mb',  'spot_memory_mb',      TRUE),
    ('instance', 'SPOT',        'count',      'instances',           FALSE),
    ('disk',     'pd-standard', 'size_gb',    'standard_disk_gb',    FALSE),
    ('disk',     'pd-balanced', 'size_gb',    'ssd_disk_gb',         FALSE),
    ('disk',     'pd-ssd',      'size_gb',    'ssd_disk_gb',         FALSE),
    ('image',    '',            'count',      'images',              FALSE),
    ('image',    '',            'storage_gb', 'image_storage_gb',    FALSE),
    ('snapshot', '',            'count',      'snapshots',           FALSE),
    ('snapshot', '',            'storage_gb', 'snapshot_storage_gb', FALSE);

-- More of what a facade operation settles when it finishes. also is a
-- JSON list of further objects the call charged in full (the new disks
-- an instance insert creates), refunded with object if it fails.
-- on_success is what to refund from object (and, when it is the empty
-- list, from also) once it succeeds: a delete's whole charge, a stop's
-- vCPU and memory. '' refunds nothing.
ALTER TABLE facade_operations ADD COLUMN also TEXT NOT NULL DEFAULT '';
ALTER TABLE facade_operations ADD COLUMN on_success TEXT NOT NULL DEFAULT '';
