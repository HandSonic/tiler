#!/usr/bin/env python3
"""Package the Windows builds with the files needed to run Tiler."""

import argparse
import hashlib
from pathlib import Path
import struct
import zipfile


def package(repo, dist, arch):
    exe = dist / ("tiler_windows_" + arch + ".exe")
    with exe.open("rb") as stream:
        if stream.read(2) != b"MZ":
            raise ValueError("Not a Windows executable: " + str(exe))
        stream.seek(0x3C)
        pe_offset = struct.unpack("<I", stream.read(4))[0]
        stream.seek(pe_offset)
        signature, machine = struct.unpack("<4sH", stream.read(6))
    expected_machine = {"amd64": 0x8664, "386": 0x14C}[arch]
    if signature != b"PE\0\0" or machine != expected_machine:
        raise ValueError("Executable architecture does not match " + arch)

    files = [(exe, "tiler.exe")]
    for name in ("conf.toml", "README.md", "README-EN.md"):
        files.append((repo / name, name))
    files.extend((file, file.relative_to(repo).as_posix()) for file in sorted((repo / "docs").rglob("*")) if file.is_file())
    files.append((repo / "internal/cover/LICENSE.md", "licenses/orb-tilecover-LICENSE.md"))
    files.append((repo / "internal/pmtiles/LICENSE", "licenses/pmtiles-LICENSE"))
    for name in ("golang-x-image-LICENSE", "golang-x-text-LICENSE"):
        files.append((repo / "third_party/licenses" / name, "licenses/" + name))
    geojson_files = sorted((repo / "geojson").rglob("*.geojson"))
    if not geojson_files:
        raise FileNotFoundError("No bundled GeoJSON files found")
    files.extend((file, file.relative_to(repo).as_posix()) for file in geojson_files)
    for suffix, name in ((".build.txt", "build-info.txt"), (".pe.txt", "pe-info.txt")):
        metadata = dist / (exe.name + suffix)
        if metadata.is_file():
            files.append((metadata, name))
    for source, _ in files:
        if not source.is_file():
            raise FileNotFoundError(str(source))

    archive = dist / ("tiler-win7-" + arch + ".zip")
    temporary = archive.with_suffix(".zip.tmp")
    try:
        with zipfile.ZipFile(temporary, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as output:
            for source, name in files:
                output.write(source, name)
        with zipfile.ZipFile(temporary) as output:
            damaged = output.testzip()
            if damaged:
                raise ValueError("Archive validation failed: " + damaged)
        temporary.replace(archive)
    finally:
        if temporary.exists():
            temporary.unlink()
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    print("{}  {}".format(digest, archive))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    repo = Path(__file__).resolve().parents[1]
    parser.add_argument("--dist", type=Path, default=repo / "dist")
    parser.add_argument("--arch", nargs="+", choices=("amd64", "386"), default=("amd64", "386"))
    args = parser.parse_args()
    for arch in args.arch:
        package(repo, args.dist.resolve(), arch)


if __name__ == "__main__":
    main()
