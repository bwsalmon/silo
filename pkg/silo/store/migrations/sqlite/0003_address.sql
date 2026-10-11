-- The address-space allocator (docs/silo/allocators.md, SV-38 to
-- SV-42). Package pkg/silo/ledger/address owns these tables.
--
-- Every range is an aligned IPv4 CIDR, kept as its first address (an
-- unsigned 32-bit integer) and its prefix length. Free space is not
-- stored: it is a block minus what is delegated, own or cooling in it.

-- Blocks. Each is one of silo's blocks, carved from parent's map, where
-- it is the delegated range. The root's block has parent ''. The
-- gateway and control ranges are blocks of the system entries
-- system:gateway and system:control, with the root as parent.
CREATE TABLE address_blocks (
    silo       TEXT    NOT NULL,
    start      INTEGER NOT NULL,
    bits       INTEGER NOT NULL,
    parent     TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (silo, start)
);
-- Two of one parent's delegated ranges never share a first address.
CREATE UNIQUE INDEX address_blocks_parent_start ON address_blocks (parent, start);

-- Own ranges: a subnet's primary or secondary range, placed by the
-- tenant inside silo's blocks. object is the facade's name for it.
CREATE TABLE address_own (
    silo       TEXT    NOT NULL,
    object     TEXT    NOT NULL,
    start      INTEGER NOT NULL,
    bits       INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (silo, object)
);

-- Freed blocks, back in holder's map but not reused until an hour of
-- database time after freed_at (call 24).
CREATE TABLE address_freed (
    holder   TEXT    NOT NULL,
    start    INTEGER NOT NULL,
    bits     INTEGER NOT NULL,
    freed_at INTEGER NOT NULL,
    PRIMARY KEY (holder, start, bits)
);

-- A block cap a parent raised for one child. A silo with no row has
-- the default of 4.
CREATE TABLE address_caps (
    silo       TEXT    PRIMARY KEY,
    max_blocks INTEGER NOT NULL
);
