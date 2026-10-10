---
title: Privacy Policy
linkTitle: Privacy
weight: 900
description: "Privacy policy and data transparency declaration for Agent Mesh and Agent Mesh Connect."
---

**Last Updated:** September 17, 2026

This Privacy Policy explains how Agent Mesh Connect ("we", "us", or "our") collects, uses, and protects your information when you use our mobile application.
Agent Mesh Connect is an open-source Agent Mesh network client designed for zero-trust, peer-to-peer connectivity.

## 1. Information We Collect

To function as a background mesh network node, Agent Mesh Connect requests the following device permissions and data:

* **Location Data (Foreground and Background):** We request access to your device's precise and coarse location (`ACCESS_FINE_LOCATION`, `ACCESS_COARSE_LOCATION`, and `ACCESS_BACKGROUND_LOCATION`).
* **Network State:** We monitor your WiFi and network connections to maintain the mesh tunnel.
* **Node Identity:** Cryptographic keys (such as Ed25519 root keys), biscuits, and local API tokens used for mesh authentication and zero-trust identity federation.

## 2. How We Use Location Data

Because Agent Mesh Connect operates as a continuous mesh network router, it requires **Background Location** access to maintain network stability, handle routing via the Model Context Protocol (MCP), and keep the foreground service alive even when the app is closed.

* **Ephemeral Processing:** Your location data is processed ephemerally in memory to facilitate real-time mesh routing.
* **No Server Storage:** We do not store your location history on any central servers, control planes, or databases.

## 3. Data Sharing and End-to-End Encryption

Agent Mesh Connect does **not** sell, rent, or share your personal data with third-party data brokers, analytics companies, or advertising networks. The architecture contains no telemetry, tracking, or proprietary phone-home mechanisms. 

* **Within the Mesh:** Information necessary for routing (which may optionally include location data if configured by the user via MCP) is transmitted strictly to other authorized participants within your specific Agent Mesh network.
* **Encrypted in Transit:** All data transmitted across the mesh network is secured using end-to-end encryption. The centralized control plane cannot access or decrypt your local node traffic.

## 4. Data Retention and Deletion

Cryptographic keys and local identity tokens are stored strictly locally on your device. Because we do not store your personal data or location history on our servers, there is no centralized data to delete. 

You can instantly erase all app data, destroy your local node identity, and revoke access by uninstalling the Agent Mesh Connect application or clearing the app's local storage in your device settings.

## 5. Contact Us

If you have any questions about this Privacy Policy, architecture, or how your data is handled, please contact us at:

* **GitHub Repository:** [https://github.com/google/agentmesh](https://github.com/google/agentmesh)
