import json
import time
import jwt
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

# The signing key is generated fresh on every start and the JWKS derived from
# it, so no private key lives in the repository: an issuer whose key is public
# would let anyone mint a token for any identity a control plane trusts.
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

def _b64url_uint(n):
    import base64
    raw = n.to_bytes((n.bit_length() + 7) // 8, 'big')
    return base64.urlsafe_b64encode(raw).rstrip(b'=').decode('ascii')

_KEY = rsa.generate_private_key(public_exponent=65537, key_size=2048)
PRIVATE_KEY = _KEY.private_bytes(
    serialization.Encoding.PEM,
    serialization.PrivateFormat.PKCS8,
    serialization.NoEncryption(),
)
_PUB = _KEY.public_key().public_numbers()
JWKS = {
  "keys": [
    {
      "kty": "RSA",
      "alg": "RS256",
      "use": "sig",
      "kid": "test-key-id",
      "n": _b64url_uint(_PUB.n),
      "e": _b64url_uint(_PUB.e),
    }
  ]
}

import os
ISSUER = os.getenv('OIDC_ISSUER', 'http://mock-oidc:18080')

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/.well-known/openid-configuration':
            body = {
                'issuer': ISSUER,
                'authorization_endpoint': f'{ISSUER}/auth',
                'token_endpoint': f'{ISSUER}/token',
                'device_authorization_endpoint': f'{ISSUER}/device/code',
                'jwks_uri': f'{ISSUER}/keys'
            }
            data = json.dumps(body).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if self.path == '/keys':
            data = json.dumps(JWKS).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'ok')
        return

    def do_POST(self):
        if self.path == '/device/code':
            body = {
                'device_code': 'dev_code_123',
                'user_code': 'ABCD-1234',
                'verification_uri': 'http://example.com/verify',
                'expires_in': 60,
                'interval': 1
            }
            data = json.dumps(body).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        if self.path == '/token':
            # Parse form data from post body
            content_length = int(self.headers.get('Content-Length', 0))
            body = self.rfile.read(content_length).decode('utf-8')
            params = urllib.parse.parse_qs(body) if body else {}

            client_id = params.get('client_id', [''])[0]
            grant_type = params.get('grant_type', [''])[0]

            # Only the grants a SAM component drives are served, so a token
            # is proof of which flow obtained it: client credentials for a
            # configured node, the device grant, with the code handed out
            # above, for an interactive join that has no browser.
            if grant_type == 'urn:ietf:params:oauth:grant-type:device_code':
                granted = params.get('device_code', [''])[0] == 'dev_code_123'
            elif grant_type == 'client_credentials':
                granted = True
            else:
                granted = False
            if not granted:
                error = 'invalid_grant' if grant_type else 'invalid_request'
                data = json.dumps({'error': error}).encode('utf-8')
                self.send_response(400)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Length', str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return

            # Assign groups and roles based on client_id
            groups = ['data-scientist']
            roles = ['mesh:role:node']
            if client_id == 'admin-client':
                groups = ['admin']
                roles = ['mesh:role:admin']
            elif client_id == 'router-client':
                groups = ['routers']
                roles = ['mesh:role:router']

            payload = {
                'iss': ISSUER,
                'aud': 'agentmesh-audience',
                'sub': 'test-user',
                'exp': int(time.time()) + 3600,
                'groups': groups,
                'roles': roles
            }
            token = jwt.encode(payload, PRIVATE_KEY, algorithm='RS256', headers={'kid': 'test-key-id'})
            body = {
                'access_token': token,
                'id_token': token,
                'token_type': 'Bearer',
                'expires_in': 3600
            }
            data = json.dumps(body).encode('utf-8')
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_response(404)
        self.end_headers()

print("Mock OIDC server ready", flush=True)
HTTPServer(('0.0.0.0', 18080), Handler).serve_forever()
