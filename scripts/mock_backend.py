"""Minimal mock backend that responds with JSON for E2E testing."""
import json
from http.server import HTTPServer, BaseHTTPRequestHandler

class Handler(BaseHTTPRequestHandler):
    def do_ANY(self):
        body = json.dumps({
            "status": "ok",
            "source": "upstream",
            "method": self.command,
            "path": self.path,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = do_ANY

    def log_message(self, fmt, *args):
        print(f"[mock] {fmt % args}")

if __name__ == "__main__":
    srv = HTTPServer(("0.0.0.0", 9000), Handler)
    print("mock backend listening on :9000")
    srv.serve_forever()
