#!/usr/bin/env python3
"""Check tiler's Compact v2 output with the real MapProxy reader and services.

Requires: python -m pip install 'MapProxy==6.0.1' pyproj
Run: python scripts/check-mapproxy.py --tiler ./tiler
This development check does not add a Python dependency to tiler.
"""

import argparse
from importlib.metadata import version
from io import BytesIO
from pathlib import Path
import subprocess
import tempfile
from urllib.parse import urlencode

from mapproxy.cache.compact import CompactCacheV2
from mapproxy.cache.tile import Tile
from mapproxy.wsgiapp import make_wsgi_app
from PIL import Image, ImageDraw
from werkzeug.test import Client
from werkzeug.wrappers import Response
import yaml


# Non-symmetric coordinates across 128-tile boundaries and at the maximum zoom.
COORDS = ((127, 130, 9), (128, 130, 9), (130, 127, 9), (130, 128, 9),
          (759981337, 375091241, 30))


def check_format(tiler, work, extension, tile_size=256):
    source, output = work / "tiles", work / "compact"
    originals = {}
    for index, (x, y, z) in enumerate(COORDS):
        image = Image.new("RGB", (tile_size, tile_size), (40 + index * 50, 80, 140))
        ImageDraw.Draw(image).rectangle((7, 19, 53, 91), fill=(255, 220, index * 30))
        path = source / str(z) / str(x) / f"{y}.{extension}"
        path.parent.mkdir(parents=True, exist_ok=True)
        image.save(path)
        originals[(x, y, z)] = path.read_bytes()

    subprocess.run(
        [str(tiler), "convert", "-input", str(source), "-output", str(output),
         "-output-type", "compactv2"],
        check=True,
    )

    example = Path(__file__).resolve().parents[1] / "docs/mapproxy-compactv2.yaml"
    config = yaml.safe_load(example.read_text(encoding="utf-8"))
    cache = config["caches"]["world_cache"]
    cache["cache"]["directory"] = str(output / "_alllayers")
    cache["format"] = "image/png" if extension == "png" else "image/jpeg"
    config["grids"]["webmercator"]["tile_size"] = [tile_size, tile_size]
    config_path = work / "mapproxy.yaml"
    config_path.write_text(yaml.safe_dump(config), encoding="utf-8")
    client = Client(make_wsgi_app(str(config_path), ignore_config_warnings=False), Response)
    reader = CompactCacheV2(str(output / "_alllayers"))
    service_extension = cache["format"].split("/")[1]

    for (x, y, z), expected in originals.items():
        tile = Tile((x, y, z))
        assert reader.load_tile(tile), f"MapProxy could not read {(x, y, z)}"
        assert tile.source.as_buffer().read() == expected, "Compact v2 reader changed tile bytes"
        query = urlencode({
            "SERVICE": "WMTS", "REQUEST": "GetTile", "VERSION": "1.0.0",
            "LAYER": "world", "STYLE": "default", "TILEMATRIXSET": "webmercator",
            "TILEMATRIX": z, "TILECOL": x, "TILEROW": y, "FORMAT": cache["format"],
        })
        paths = (
            f"/tiles/world/webmercator/{z}/{x}/{y}.{service_extension}",
            f"/tms/1.0.0/world/webmercator/{z - 1}/{x}/{(1 << z) - 1 - y}.{service_extension}",
            f"/service?{query}",
        )
        for path in paths:
            response = client.get(path)
            assert response.status_code == 200, (path, response.status, response.data[:400])
            assert response.data == expected, f"MapProxy served different bytes: {path}"

    if extension == "png" and tile_size == 256:
        def bundles():
            return {path: (path.stat().st_size, path.stat().st_mtime_ns)
                    for path in output.rglob("*.bundle")}
        before = bundles()
        response = client.get("/tiles/world/webmercator/9/0/0.png")
        assert response.status_code == 200, response.status
        with Image.open(BytesIO(response.data)) as missing:
            assert missing.convert("RGBA").getchannel("A").getextrema() == (0, 0)
        assert bundles() == before, "A missing tile request wrote to the Compact cache"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tiler", type=Path, required=True, help="built tiler executable")
    args = parser.parse_args()
    tiler = args.tiler.resolve(strict=True)
    with tempfile.TemporaryDirectory(prefix="tiler-mapproxy-") as directory:
        for extension, size in (("png", 256), ("jpg", 256), ("png", 512)):
            check_format(tiler, Path(directory) / f"{extension}-{size}", extension, size)
    print(f"MapProxy {version('MapProxy')}: PNG/JPEG Compact v2, 256/512px, XYZ, TMS and WMTS checks passed.")


if __name__ == "__main__":
    main()
