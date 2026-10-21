package main

// maxMetadataBytes is a Tiler memory policy for Win7 and other constrained
// systems, not an MBTiles or PMTiles format limit. Readers and writers share
// it so conversion cannot publish metadata that Tiler cannot read back.
const maxMetadataBytes = 16 << 20
