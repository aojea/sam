# Agent Mesh Roadmap

Agent Mesh is developed in three clear stages: validating the core architecture (`pre-v0.1.0`), expanding ecosystem integrations on a stable foundation (`v0.1.0` – `v0.x`), and locking in long-term backward compatibility (`v1.0.0`).

---

## 1. Pre-`v0.1.0` — Exploratory and Core Foundation

**Versions:** `v0.1.0-alpha.x`, `v0.1.0-rc.x`  
**Stability:** Exploratory — rapid iteration to validate the core protocol, security model, and developer experience.

* **P2P Dataplane and Discovery:** Encrypted peer-to-peer connectivity (`libp2p` QUIC and TLS 1.3), Kademlia DHT service discovery, NAT hole-punching, and stateless `agentmesh-router` relays.
* **Named Service Primitives:** Deny-by-default routing over four named schemes (`mcp://`, `a2a://`, `inference://`, `egress://`) instead of flat IP subnets.
* **Identity and Task-Scoped Authorization:** Enrollment via OIDC, SPIFFE, and Kubernetes ServiceAccounts; channel-bound Biscuit credentials; compiled Datalog policy; and offline task attenuation (`tar_block`).
* **Unified API and SDKs:** Single protobuf contract (`api/agentmesh.proto`), `agentmesh-node` daemon, standalone `agentmesh-one` binary, and native TypeScript (`sdk/js`) and Python (`sdk/python`) SDKs.
* **Developer Testnets:** Public testnets and one-command onboarding (`agentmesh-node skill install`).

---

## 2. `v0.1.0` – `v0.x` — Stable Core, Integrations, and Ecosystem

**Versions:** `v0.1.0` through `v0.x.x`  
**Stability:** Stable for adoption — the core architecture and wire protocol are stable, with minor breaking changes possible between `0.x` minor releases as integrations mature.

* **Sandboxes and Kubernetes Interoperability:** Turnkey integration with Kubernetes and isolated agent sandboxes so existing workloads can connect and burst across platforms with zero credentials inside guest memory.
* **Agent Gateways and Service Meshes:** Native interoperability with agent gateways and service meshes over standard HTTP and OAuth 2.0 Token Exchange (`RFC 8693`).
* **Cloud Secret Managers and Identity Federation:** Integration with cloud secret managers, key management systems, certificate authorities, and outbound cloud token exchange services for customer-governed sovereign meshes.
* **Visibility, Audit Logging, and Fleet Operations:** OpenTelemetry metrics, tamper-evident audit logs across multi-hop agent calls, cryptographic geo-fencing (`region` labels), and multi-mesh fleet management.
* **Packaging and Ecosystem:** Production Helm charts, Kubernetes controllers, agent harness skills, and mobile and edge companion nodes.

---

## 3. `v1.0.0` and Beyond — Long-Term Production Stability

**Versions:** `v1.0.0+`  
**Stability:** Super stable — strict SemVer backward compatibility and enterprise-grade reliability.

* **Frozen Wire and Policy Contracts:** Guaranteed backward compatibility for `api/agentmesh.proto`, persisted node credentials (`MemberCredential`), and Datalog policy evaluation across `1.x` releases.
* **Independent Security Audits:** Third-party cryptographic and penetration audits covering Biscuit token verification, PeerID channel binding, and key rotation.
* **High Availability and Lifecycle Guarantees:** Multi-replica control plane high availability, zero-downtime upgrades, automated conformance testing, and a formal CVE disclosure and long-term support (LTS) policy.
