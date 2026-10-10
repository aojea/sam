/// Device enrollment links.
///
/// A control plane (agentmesh-one's terminal QR code, or any operator holding a
/// bootstrap token) hands a device everything it needs in one string:
///
///     mesh://enroll?server=<control-plane-url>&token=<bootstrap-token>
///
/// This mirrors `api.EnrollURI` / `api.ParseEnrollURI` in the Go tree and
/// must stay in lockstep with it: same scheme, same host, same two query
/// parameters, same transport rule for the server URL. The token is an
/// ordinary bootstrap token, so the link grants exactly one enrollment in
/// the token's role until it expires.
///
/// The control plane is the device's trust root, so the server must be
/// https; plaintext http is accepted only to a loopback host (an emulator
/// reaching the workstation over `adb reverse`). The Go node client inside
/// the FFI enforces the same rule, so accepting more here would only defer
/// the failure to after the single-use token was spent.
library;

import 'dart:io' show InternetAddress;

class EnrollLink {
  const EnrollLink({required this.server, required this.token});

  /// Control plane base URL the device enrolls against.
  final String server;

  /// Bootstrap token spent by POST /enroll.
  final String token;

  /// Hostname shown to the user before they confirm the enrollment.
  String get serverHost => Uri.parse(server).host;

  /// Enough of the token to recognise it, never enough to reuse it.
  String get tokenHint {
    if (token.length <= 12) return token;
    return '${token.substring(0, 12)}…';
  }
}

/// Parses a `mesh://enroll` link; returns null for anything else, including
/// a bare token, so callers can fall back to token-only entry.
EnrollLink? parseEnrollLink(String raw) {
  final Uri uri;
  try {
    uri = Uri.parse(raw.trim());
  } on FormatException {
    return null;
  }
  if (uri.scheme != 'mesh' || uri.host != 'enroll') return null;
  final server = uri.queryParameters['server'] ?? '';
  final token = uri.queryParameters['token'] ?? '';
  if (token.isEmpty) return null;
  if (!isTrustedControlPlaneUrl(server)) return null;
  return EnrollLink(server: server, token: token);
}

/// Whether a device may take [server] as its control plane: https, or http
/// to a loopback host. Same rule as `api.ValidateControlPlaneTransport`.
bool isTrustedControlPlaneUrl(String server) {
  final uri = Uri.tryParse(server);
  if (uri == null || uri.host.isEmpty) return false;
  switch (uri.scheme) {
    case 'https':
      return true;
    case 'http':
      return uri.host.toLowerCase() == 'localhost' ||
          (InternetAddress.tryParse(uri.host)?.isLoopback ?? false);
    default:
      return false;
  }
}
