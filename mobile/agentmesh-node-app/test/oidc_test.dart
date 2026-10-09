import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:sam_agent/oidc.dart';

void main() {
  Future<http.Response> exchange(http.Client client) =>
      exchangeAuthorizationCode(
        tokenUrl: Uri.parse('https://auth.example.test/token'),
        clientId: 'test-client',
        code: 'test-code',
        redirectUri: 'http://127.0.0.1:13000/callback',
        verifier: 'test-verifier',
        client: client,
      );

  testWidgets('token exchange waits until the app resumes', (tester) async {
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);

    final requests = <http.Request>[];
    final client = MockClient((request) async {
      requests.add(request);
      return http.Response('{"id_token":"test-token"}', 200);
    });
    addTearDown(client.close);
    final response = exchange(client);
    await tester.pump();
    expect(requests, isEmpty);

    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    await tester.pump();
    expect(requests, isEmpty);

    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    await tester.pump();
    expect((await response).statusCode, 200);
    expect(requests, hasLength(1));
    expect(requests.single.url, Uri.parse('https://auth.example.test/token'));
    expect(requests.single.method, 'POST');
    expect(requests.single.bodyFields, {
      'grant_type': 'authorization_code',
      'client_id': 'test-client',
      'code': 'test-code',
      'redirect_uri': 'http://127.0.0.1:13000/callback',
      'code_verifier': 'test-verifier',
    });
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    await tester.pump();
    expect(requests, hasLength(1));
  });

  testWidgets('token exchange rechecks a short-lived resume', (tester) async {
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
    var requests = 0;
    final client = MockClient((request) async {
      requests++;
      return http.Response('{}', 200);
    });
    addTearDown(client.close);
    final response = exchange(client);

    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    await tester.pump();
    expect(requests, 0);

    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    await tester.pump();
    expect((await response).statusCode, 200);
    expect(requests, 1);
  });

  testWidgets('token exchange proceeds when already resumed', (tester) async {
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    var requests = 0;
    final client = MockClient((request) async {
      requests++;
      return http.Response('{"error":"invalid_grant"}', 400);
    });
    addTearDown(client.close);
    final response = await exchange(client);
    expect(response.statusCode, 400);
    expect(requests, 1);
  });

  testWidgets('device polling waits while verifying on this phone',
      (tester) async {
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
    final requests = <http.Request>[];
    final client = MockClient((request) async {
      requests.add(request);
      return http.Response('{"id_token":"test-token"}', 200);
    });
    addTearDown(client.close);
    final response = exchangeDeviceCode(
      tokenUrl: Uri.parse('https://auth.example.test/token'),
      clientId: 'test-client',
      deviceCode: 'test-device-code',
      isActive: () => true,
      client: client,
    );
    await tester.pump(const Duration(seconds: 30));
    expect(requests, isEmpty);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    await tester.pump();
    expect(requests, isEmpty);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    await tester.pump();
    expect((await response)?.statusCode, 200);
    expect(requests, hasLength(1));
    expect(requests.single.bodyFields, {
      'grant_type': 'urn:ietf:params:oauth:grant-type:device_code',
      'device_code': 'test-device-code',
      'client_id': 'test-client',
    });
  });

  testWidgets('canceling device login prevents a queued poll', (tester) async {
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
    var active = true;
    var requests = 0;
    final client = MockClient((request) async {
      requests++;
      return http.Response('{}', 200);
    });
    addTearDown(client.close);
    final response = exchangeDeviceCode(
      tokenUrl: Uri.parse('https://auth.example.test/token'),
      clientId: 'test-client',
      deviceCode: 'test-device-code',
      isActive: () => active,
      client: client,
    );
    active = false;
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.hidden);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.inactive);
    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
    await tester.pump();
    expect(await response, isNull);
    expect(requests, 0);
  });

  for (final error in ['authorization_pending', 'slow_down', 'expired_token']) {
    testWidgets('foreground device poll preserves $error', (tester) async {
      tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.resumed);
      var requests = 0;
      final client = MockClient((request) async {
        requests++;
        return http.Response('{"error":"$error"}', 400);
      });
      addTearDown(client.close);
      final response = await exchangeDeviceCode(
        tokenUrl: Uri.parse('https://auth.example.test/token'),
        clientId: 'test-client',
        deviceCode: 'test-device-code',
        isActive: () => true,
        client: client,
      );
      expect(response?.statusCode, 400);
      expect(response?.body, '{"error":"$error"}');
      expect(requests, 1);
    });
  }
}
