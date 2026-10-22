package main

// maxMetadataBytes is a Tiler memory policy for Win7 and other constrained
// systems, not an MBTiles or PMTiles format limit. Readers and writers share
// it so conversion cannot publish metadata that Tiler cannot read back.
const maxMetadataBytes = 16 << 20

// PMTiles readers charge the root and active leaf entries together. The
// writer must share this limit so its own output remains readable.
const pmDirectoryEntryLimit uint64 = 1 << 20
