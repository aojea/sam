import 'dart:io';
import 'dart:convert';
import 'dart:math';
import 'dart:typed_data';
import 'package:flutter/services.dart';
import 'package:flutter/foundation.dart';

class SamDartMcpServer {
  static const MethodChannel _channel = MethodChannel('dev.sammesh.connect/mesh_expose');
  HttpServer? _server;
  final List<HttpResponse> _sseClients = [];
  // Android loopback is shared by every installed app, so the port alone is
  // no boundary: each start mints a token only the node is told about.
  String? _token;

  // Callbacks to check current feature status from UI
  final bool Function() isBatteryEnabled;
  final bool Function() isLocationEnabled;

  SamDartMcpServer({
    required this.isBatteryEnabled,
    required this.isLocationEnabled,
  });

  /// The URL the node dials, with this launch's credential as userinfo; the
  /// node turns that into an Authorization header and never advertises it.
  String get targetUrl {
    final s = _server;
    if (s == null || _token == null) throw StateError('not started');
    return 'http://:$_token@127.0.0.1:${s.port}';
  }

  static String newToken() {
    final rng = Random.secure();
    return base64Url.encode(List<int>.generate(32, (_) => rng.nextInt(256))).replaceAll('=', '');
  }

  /// Starts the Dart HTTP Server acting as an MCP backend on a random
  /// loopback port. The service it backs is declared in the node's start
  /// configuration; there is no runtime registration, so this server must be
  /// listening before the node starts and probes it.
  ///
  /// Throws if no port can be bound.
  Future<void> start({int port = 0}) async {
    if (_server != null) throw StateError('already started');
    _token = newToken();
    _server = await HttpServer.bind(InternetAddress.loopbackIPv4, port);
    debugPrint('Agent Mesh Dart MCP Server listening on port ${_server!.port}');

    _server!.listen((HttpRequest request) async {
      if (!_authorized(request)) {
        request.response.statusCode = HttpStatus.unauthorized;
        await request.response.close();
        return;
      }
      if (request.method == 'GET') {
        _handleSse(request);
      } else if (request.method == 'POST') {
        _handlePost(request);
      } else {
        request.response.statusCode = HttpStatus.methodNotAllowed;
        await request.response.close();
      }
    });
  }

  bool _authorized(HttpRequest request) {
    final token = _token;
    if (token == null) return false;
    return constantTimeEquals(request.headers.value(HttpHeaders.authorizationHeader), 'Bearer $token');
  }

  /// Compares a presented credential without leaking where it diverges.
  @visibleForTesting
  static bool constantTimeEquals(String? a, String b) {
    if (a == null || a.length != b.length) return false;
    var diff = 0;
    for (var i = 0; i < a.length; i++) {
      diff |= a.codeUnitAt(i) ^ b.codeUnitAt(i);
    }
    return diff == 0;
  }

  /// Stops the server
  Future<void> stop() async {
    await _server?.close(force: true);
    _server = null;
    _token = null;
    _sseClients.clear();
    debugPrint('Agent Mesh Dart MCP Server stopped');
  }

  static const int _maxRequestBodyBytes = 1024 * 1024;

  /// Handles SSE (Server-Sent Events) for MCP stream
  void _handleSse(HttpRequest request) async {
    request.response.headers.contentType = ContentType('text', 'event-stream');
    request.response.headers.add('Cache-Control', 'no-cache');
    request.response.headers.add('Connection', 'keep-alive');
    
    _sseClients.add(request.response);
    request.response.done.then((_) {
      _sseClients.remove(request.response);
    });
    
    try {
      // Initial connection event
      request.response.write('event: connected\ndata: {}\n\n');
      await request.response.flush();
    } catch (_) {
      _sseClients.remove(request.response);
    }
  }

  /// Handles MCP JSON-RPC Requests
  void _handlePost(HttpRequest request) async {
    if (request.contentLength > _maxRequestBodyBytes) {
      request.response.statusCode = HttpStatus.requestEntityTooLarge;
      await request.response.close();
      return;
    }

    final builder = BytesBuilder(copy: false);
    await for (final chunk in request) {
      if (builder.length + chunk.length > _maxRequestBodyBytes) {
        request.response.statusCode = HttpStatus.requestEntityTooLarge;
        await request.response.close();
        return;
      }
      builder.add(chunk);
    }
    final body = utf8.decode(builder.takeBytes(), allowMalformed: true);
    
    try {
      final jsonRpc = jsonDecode(body);
      final id = jsonRpc['id'];

      final response = await handleMcpRequest(jsonRpc);
      
      if (response == null) {
        // It was a notification or unhandled non-request
        if (id == null) {
          request.response.statusCode = HttpStatus.accepted; // 202 Accepted
        } else {
          request.response.statusCode = HttpStatus.notFound;
        }
        await request.response.close();
        return;
      }

      _sendJsonResponse(request, response);

    } catch (e) {
      debugPrint('Error processing MCP request: $e');
      _sendJsonError(request, null, -32700, 'Parse error');
    }
  }

  /// Core MCP Logic separated for testability
  Future<Map<String, dynamic>?> handleMcpRequest(Map<String, dynamic> jsonRpc) async {
    final method = jsonRpc['method'];
    final id = jsonRpc['id'];

    // 1. Handshake
    if (method == 'initialize') {
        return {
          'jsonrpc': '2.0',
          'id': id,
          'result': {
            'protocolVersion': '2024-11-05',
            'capabilities': {'tools': {}},
            'serverInfo': {'name': 'agentmesh-dart-sensors', 'version': '1.0.0'}
          }
        };
    }
    
    // 2. List Tools
    if (method == 'tools/list') {
        final tools = [];
        
        if (isBatteryEnabled()) {
          tools.add({
            'name': 'get_battery_status',
            'description': 'Returns the current battery level and charging status of the device.',
            'inputSchema': {'type': 'object', 'properties': {}}
          });
        }
        
        if (isLocationEnabled()) {
          tools.add({
            'name': 'get_location',
            'description': 'Returns the approximate location of the device, rounded to about a kilometre.',
            'inputSchema': {'type': 'object', 'properties': {}}
          });
        }

        return {
          'jsonrpc': '2.0',
          'id': id,
          'result': {
            'tools': tools
          }
        };
    }

    // 3. Call Tool
    if (method == 'tools/call') {
        final params = jsonRpc['params'];
        if (params is! Map<String, dynamic> || !params.containsKey('name')) {
          return {
            'jsonrpc': '2.0',
            'id': id,
            'error': {'code': -32602, 'message': 'Invalid params: missing tool name'}
          };
        }
        final toolName = params['name'];
        
        if (toolName == 'get_battery_status' && isBatteryEnabled()) {
            try {
              final batteryData = await _channel.invokeMethod('getBatteryData');
              return {
                'jsonrpc': '2.0',
                'id': id,
                'result': {
                  'content': [{'type': 'text', 'text': batteryData.toString()}]
                }
              };
            } catch (e) {
              return {
                'jsonrpc': '2.0',
                'id': id,
                'error': {'code': -32000, 'message': 'Failed to get battery data: $e'}
              };
            }
        }
        
        if (toolName == 'get_location' && isLocationEnabled()) {
             try {
              final locationData = await _channel.invokeMethod('getLocationData');
              return {
                'jsonrpc': '2.0',
                'id': id,
                'result': {
                  'content': [{'type': 'text', 'text': locationData.toString()}]
                }
              };
            } catch (e) {
               return {
                'jsonrpc': '2.0',
                'id': id,
                'error': {'code': -32000, 'message': 'Failed to get location data: $e'}
              };
            }
        }
        
        return {
          'jsonrpc': '2.0',
          'id': id,
          'error': {'code': -32601, 'message': 'Method not found or disabled'}
        };
    }

    // If it's a notification (no ID), we return null to indicate no body response
    if (id == null) {
      return null;
    }

    return {
      'jsonrpc': '2.0',
      'id': id,
      'error': {'code': -32601, 'message': 'Method not implemented'}
    };
  }

  void _sendJsonResponse(HttpRequest request, Map<String, dynamic> response) async {
    request.response.headers.contentType = ContentType.json;
    request.response.write(jsonEncode(response));
    await request.response.close();
  }

  void _sendJsonError(HttpRequest request, dynamic id, int code, String message) async {
    final response = {
      'jsonrpc': '2.0',
      'id': id,
      'error': {
        'code': code,
        'message': message
      }
    };
    request.response.headers.contentType = ContentType.json;
    request.response.write(jsonEncode(response));
    await request.response.close();
  }
}
