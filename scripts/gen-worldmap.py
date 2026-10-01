#!/usr/bin/env python3
"""Regenerates internal/netpath/world.path, the land outline used by `lmon path --html`.

Source: Natural Earth 1:110m admin-0 countries (public domain,
https://www.naturalearthdata.com/about/terms-of-use/). The outline is projected
to a plain equirectangular grid (x = longitude, y = latitude, same scale on both
axes) cropped to 84N..58S, and written as one SVG path. Run:

    python3 scripts/gen-worldmap.py
"""
import json
import os
import urllib.request

URL = "https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson/ne_110m_admin_0_countries.geojson"
K = 1000 / 360  # user units per degree; the full 360 degrees of longitude are 1000 wide
TOP, BOTTOM = 84, -58  # no Antarctica, nothing north of Greenland
OUT = os.path.join(os.path.dirname(__file__), "..", "internal", "netpath", "world.path")


def project(lon, lat):
    return round((lon + 180) * K, 1), round((TOP - lat) * K, 1)


def ring_path(ring):
    pts = []
    for lon, lat in ring:
        p = project(lon, lat)
        if not pts or p != pts[-1]:  # drop points that round onto their neighbour
            pts.append(p)
    if len(pts) < 3:
        return ""
    cmds = ["M%g %g" % pts[0]] + ["L%g %g" % p for p in pts[1:]]
    return "".join(cmds) + "Z"


def main():
    with urllib.request.urlopen(URL) as r:
        data = json.load(r)
    parts = []
    for f in data["features"]:
        if f["properties"].get("CONTINENT") == "Antarctica":
            continue
        g = f["geometry"]
        polys = [g["coordinates"]] if g["type"] == "Polygon" else g["coordinates"]
        for poly in polys:
            for ring in poly:  # exterior first, then holes; the even-odd fill rule makes holes work
                parts.append(ring_path(ring))
    d = "".join(p for p in parts if p)
    with open(OUT, "w") as out:
        out.write(d)
    print("wrote %s: %d bytes, height %.1f units for width 1000" % (os.path.normpath(OUT), len(d), (TOP - BOTTOM) * K))


if __name__ == "__main__":
    main()
