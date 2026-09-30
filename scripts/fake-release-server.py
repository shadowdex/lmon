#!/usr/bin/env python3
"""A stand-in for GitHub's release URLs, for testing install.sh offline.

usage: fake-release-server.py ROOT PORTFILE

ROOT/<tag>/<file>   served at /releases/download/<tag>/<file>
ROOT/.latest        tag that /releases/latest redirects to (default v9.9.9)
ROOT/.no-latest     if it exists, /releases/latest returns 404 (no release yet)
ROOT/.only-prereleases
                    if it exists, /releases/latest redirects to the /releases list,
                    which is what GitHub does when only pre-releases are published
"""
import faulthandler
import http.server
import os
import socketserver
import sys

# If startup ever hangs, print where after 8s (the test script waits 10s and
# reports this output), instead of failing with no explanation.
faulthandler.dump_traceback_later(8, exit=False)

root, portfile = sys.argv[1], sys.argv[2]


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        p = self.path
        if p == "/releases/latest":
            if os.path.exists(os.path.join(root, ".no-latest")):
                return self.send_error(404)
            if os.path.exists(os.path.join(root, ".only-prereleases")):
                self.send_response(302)
                self.send_header("Location", "http://%s/releases" % self.headers["Host"])
                self.end_headers()
                return
            try:
                tag = open(os.path.join(root, ".latest")).read().strip()
            except FileNotFoundError:
                tag = "v9.9.9"
            self.send_response(302)
            self.send_header("Location", "http://%s/releases/tag/%s" % (self.headers["Host"], tag))
            self.end_headers()
        elif p == "/releases":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"release list")
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


class Server(http.server.ThreadingHTTPServer):
    def server_bind(self):
        # HTTPServer.server_bind() calls socket.getfqdn(), a reverse DNS lookup
        # that can stall for a long time on CI runners. We don't need the name.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address[:2]


server = Server(("127.0.0.1", 0), Handler)
with open(portfile, "w") as f:  # closed (and flushed) before we report ready
    f.write(str(server.server_address[1]))
faulthandler.cancel_dump_traceback_later()
server.serve_forever()
