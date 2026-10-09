import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:sam_agent/mcp_server.dart';

void main() {
  group('SamDartMcpServer Compliance Tests', () {
    late SamDartMcpServer server;
    bool batteryEnabled = true;
    bool locationEnabled = true;

    setUp(() {
      server = SamDartMcpServer(
        isBatteryEnabled: () => batteryEnabled,
        isLocationEnabled: () => locationEnabled,
      );
    });

    test('Handle initialize request', () async {
      final request = {
        'jsonrpc': '2.0',
        'id': 1,
        'method': 'initialize',
        'params': {
          'protocolVersion': '2024-11-05',
          'capabilities': {},
          'clientInfo': {'name': 'test-client', 'version': '1.0.0'}
        }
      };

      final response = await server.handleMcpRequest(request);

      expect(response, isNotNull);
      expect(response!['jsonrpc'], '2.0');
      expect(response['id'], 1);
      expect(response['result'], isNotNull);
      expect(response['result']['protocolVersion'], '2024-11-05');
      expect(response['result']['serverInfo']['name'], 'sam-dart-sensors');
    });

    test('Handle notifications/initialized (should return null for no body)', () async {
      final request = {
        'jsonrpc': '2.0',
        // No ID for notifications
        'method': 'notifications/initialized',
        'params': {}
      };

      final response = await server.handleMcpRequest(request);

      // Null response indicates HTTP 202 Accepted with no body
      expect(response, isNull);
    });

    test('Handle tools/list request', () async {
      final request = {
        'jsonrpc': '2.0',
        'id': 2,
        'method': 'tools/list',
        'params': {}
      };

      final response = await server.handleMcpRequest(request);

      expect(response, isNotNull);
      expect(response!['id'], 2);
      expect(response['result'], isNotNull);
      final tools = response['result']['tools'] as List;
      expect(tools.length, 2);
      expect(tools[0]['name'], 'get_battery_status');
      expect(tools[1]['name'], 'get_location');
    });

    test('Handle tools/list request with disabled battery', () async {
      batteryEnabled = false;
      final request = {
        'jsonrpc': '2.0',
        'id': 3,
        'method': 'tools/list',
        'params': {}
      };

      final response = await server.handleMcpRequest(request);

      expect(response, isNotNull);
      final tools = response!['result']['tools'] as List;
      expect(tools.length, 1);
      expect(tools[0]['name'], 'get_location');
      
      // Reset
      batteryEnabled = true;
    });

    test('Handle unhandled method', () async {
      final request = {
        'jsonrpc': '2.0',
        'id': 4,
        'method': 'unsupported/method',
        'params': {}
      };

      final response = await server.handleMcpRequest(request);

      expect(response, isNotNull);
      expect(response!['error'], isNotNull);
      expect(response['error']['code'], -32601); // Method not implemented
    });
  });

  // Android loopback is shared by every installed app, so the sensor server
  // must refuse anyone who does not hold this launch's token; only the node,
  // told the token through target_url userinfo, may reach the sensors.
  group('SamDartMcpServer loopback authentication', () {
    late SamDartMcpServer server;

    setUp(() async {
      server = SamDartMcpServer(
        isBatteryEnabled: () => true,
        isLocationEnabled: () => false,
      );
      await server.start();
    });

    tearDown(() => server.stop());

    Future<HttpClientResponse> post(String? authorization) async {
      final target = Uri.parse(server.targetUrl);
      final client = HttpClient();
      try {
        final req = await client.postUrl(Uri(scheme: 'http', host: target.host, port: target.port, path: '/'));
        if (authorization != null) {
          req.headers.set(HttpHeaders.authorizationHeader, authorization);
        }
        req.headers.contentType = ContentType.json;
        req.write(jsonEncode({'jsonrpc': '2.0', 'id': 1, 'method': 'tools/list', 'params': {}}));
        return await req.close();
      } finally {
        client.close();
      }
    }

    test('binds a random loopback port and carries the token as userinfo', () {
      final target = Uri.parse(server.targetUrl);
      expect(target.host, '127.0.0.1');
      expect(target.port, isNot(0));
      expect(target.userInfo, startsWith(':'));
      expect(target.userInfo.length, greaterThan(32));
    });

    test('refuses a request without the token', () async {
      final resp = await post(null);
      expect(resp.statusCode, HttpStatus.unauthorized);
    });

    test('refuses a request with a wrong token', () async {
      final resp = await post('Bearer not-the-token');
      expect(resp.statusCode, HttpStatus.unauthorized);
    });

    test('serves a request with this launch token', () async {
      final token = Uri.parse(server.targetUrl).userInfo.substring(1);
      final resp = await post('Bearer $token');
      expect(resp.statusCode, HttpStatus.ok);
      final body = jsonDecode(await utf8.decoder.bind(resp).join());
      expect((body['result']['tools'] as List).single['name'], 'get_battery_status');
    });

    test('each start mints a new token', () async {
      final first = server.targetUrl;
      await server.stop();
      await server.start();
      expect(server.targetUrl, isNot(first));
    });

    test('constant-time comparison rejects length and content mismatches', () {
      expect(SamDartMcpServer.constantTimeEquals('Bearer abc', 'Bearer abc'), isTrue);
      expect(SamDartMcpServer.constantTimeEquals('Bearer abd', 'Bearer abc'), isFalse);
      expect(SamDartMcpServer.constantTimeEquals('Bearer ab', 'Bearer abc'), isFalse);
      expect(SamDartMcpServer.constantTimeEquals(null, 'Bearer abc'), isFalse);
    });
  });
}
