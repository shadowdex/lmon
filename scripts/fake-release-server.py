#!/usr/bin/env python3
"""A stand-in for GitHub's release URLs, for testing install.sh offline.

usage: fake-release-server.py ROOT PORTFILE

ROOT/<tag>/<file>   served at /releases/download/<tag>/<file>
ROOT/.latest        tag that /releases/latest redirects to (default v9.9.9)
ROOT/.no-latest     if it exists, /releases/latest returns 404 (no release yet)
"""
import http.server
import os
import sys

root, portfile = sys.argv[1], sys.argv[2]


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        p = self.path
        if p == "/releases/latest":
            if os.path.exists(os.path.join(root, ".no-latest")):
                return self.send_error(404)
            try:
                tag = open(os.path.join(root, ".latest")).read().strip()
            except FileNotFoundError:
                tag = "v9.9.9"
            self.send_response(302)
            self.send_header("Location", "http://%s/releases/tag/%s" % (self.headers["Host"], tag))
            self.end_headers()
        elif p.startswith("/releases/tag/"):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"release page")
        elif p.startswith("/releases/download/"):
            rel = p[len("/releases/download/"):]
            full = os.path.normpath(os.path.join(root, rel))
            if not full.startswith(os.path.abspath(root)) or not os.path.isfile(full):
                return self.send_error(404)
            data = open(full, "rb").read()
            self.send_response(200)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        else:
            self.send_error(404)


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
open(portfile, "w").write(str(server.server_address[1]))
server.serve_forever()
