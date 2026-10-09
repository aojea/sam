import 'dart:async';

import 'package:flutter/widgets.dart';
import 'package:http/http.dart' as http;

Future<http.Response> exchangeAuthorizationCode({
  required Uri tokenUrl,
  required String clientId,
  required String code,
  required String redirectUri,
  required String verifier,
  http.Client? client,
}) async {
  await _waitForAppResume();
  return _postToken(
      tokenUrl,
      {
        'grant_type': 'authorization_code',
        'client_id': clientId,
        'code': code,
        'redirect_uri': redirectUri,
        'code_verifier': verifier,
      },
      client: client);
}

Future<http.Response?> exchangeDeviceCode({
  required Uri tokenUrl,
  required String clientId,
  required String deviceCode,
  required bool Function() isActive,
  http.Client? client,
}) async {
  if (!isActive()) return null;
  await _waitForAppResume();
  if (!isActive()) return null;
  return _postToken(
      tokenUrl,
      {
        'grant_type': 'urn:ietf:params:oauth:grant-type:device_code',
        'device_code': deviceCode,
        'client_id': clientId,
      },
      client: client);
}

Future<void> _waitForAppResume() async {
  final binding = WidgetsBinding.instance;
  while (binding.lifecycleState != AppLifecycleState.resumed) {
    final resumed = Completer<void>();
    final listener = AppLifecycleListener(onResume: () {
      if (!resumed.isCompleted) resumed.complete();
    });
    try {
      await resumed.future;
    } finally {
      listener.dispose();
    }
  }
}

Future<http.Response> _postToken(
  Uri tokenUrl,
  Map<String, String> body, {
  http.Client? client,
}) async {
  final requestClient = client ?? http.Client();
  try {
    return await requestClient.post(
      tokenUrl,
      headers: {'Content-Type': 'application/x-www-form-urlencoded'},
      body: body,
    );
  } finally {
    if (client == null) requestClient.close();
  }
}
