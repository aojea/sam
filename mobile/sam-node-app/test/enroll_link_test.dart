import 'package:flutter_test/flutter_test.dart';
import 'package:sam_agent/enroll_link.dart';

void main() {
  // Byte-for-byte the payload sam-one prints under its QR code.
  const printed =
      'mesh://enroll?server=https%3A%2F%2Fabc-def-123.trycloudflare.com&token=mesh_dev_0123456789abcdef';

  test('parses the link sam-one prints, tolerating pasted whitespace', () {
    final link = parseEnrollLink('  $printed\n');
    expect(link, isNotNull);
    expect(link!.server, 'https://abc-def-123.trycloudflare.com');
    expect(link.token, 'mesh_dev_0123456789abcdef');
    expect(link.serverHost, 'abc-def-123.trycloudflare.com');
    expect(link.tokenHint, 'mesh_dev_0123…');
  });

  test('accepts plaintext http only to loopback (emulator over adb reverse)', () {
    final link = parseEnrollLink(
        'mesh://enroll?server=http%3A%2F%2F127.0.0.1%3A18432&token=sam-bt-abc');
    expect(link?.server, 'http://127.0.0.1:18432');
    expect(link?.serverHost, '127.0.0.1');
    expect(isTrustedControlPlaneUrl('http://localhost:8080'), isTrue);
    expect(isTrustedControlPlaneUrl('http://[::1]:8080'), isTrue);
    expect(isTrustedControlPlaneUrl('http://192.168.1.50:18432'), isFalse);
    expect(isTrustedControlPlaneUrl('http://mesh.example.com'), isFalse);
    expect(isTrustedControlPlaneUrl('https://mesh.example.com'), isTrue);
  });

  test('rejects anything that is not a trustworthy mesh://enroll link', () {
    for (final raw in [
      'samone://enroll?server=https%3A%2F%2Fx&token=t',
      'mesh://join?server=https%3A%2F%2Fx&token=t',
      'mesh://enroll?server=https%3A%2F%2Fx',
      'mesh://enroll?token=t',
      'mesh://enroll?server=ftp%3A%2F%2Fx&token=t',
      'mesh://enroll?server=x.example.com&token=t',
      // Plaintext to a LAN host: the FFI would refuse it after burning the token.
      'mesh://enroll?server=http%3A%2F%2F192.168.1.50%3A18432&token=t',
      'mesh_dev_0123456789abcdef',
      'https://example.com/?token=t',
      '',
    ]) {
      expect(parseEnrollLink(raw), isNull, reason: raw);
    }
  });
}
