# Agent Mesh Roadmap

## Phase 1: Alpha

**Versions:** `v0.1.0-alpha.x`
**Status:** Under Construction / Ephemeral

The Alpha phase is focused on laying the foundational architecture, finalizing API contracts, and gathering community feedback. The network is functional but expects breaking changes as we iterate.

* **Core Objectives:**

  * Establish P2P routing and base node lifecycle.
  * Deploy community developer testnets.
  * Draft the initial architecture documentation.
  * Integrate baseline network services: MCP, A2A, Inference.

* **Exit Criteria:**

  * Core network primitives are stable.
  * Documentation accurately reflects the cryptographic boundaries and operational models.

## Phase 2: Beta (Feature Completion)

**Versions:** `v0.1.0` through `v0.x.x`
**Status:** Functional, but APIs may break

The Beta phase iterates on functional completeness based on real-world deployments. Breaking changes are isolated to minor version bumps (`0.2.0`, `0.3.0`).

* **Core Objectives:**

  * Full implementation of local Datalog attenuation (the "Autonomous Local Veto").
  * Uncooperative sandbox confinement for agent execution (`network=none`).
  * Bring-Your-Own (BYO) OIDC Identity Provider federation (e.g., Keycloak, Dex).
  * Fail-closed jurisdictional label routing (e.g., `X-Mesh-Required-Labels: jurisdiction=eu`).

* **Exit Criteria:**

  * All planned data-residency and control features are implemented and passing integration tests.
  * API and Datalog schemas are frozen.

## Phase 3: The Audit Freeze (Release Candidates)

**Versions:** `v1.0.0-rc.x`
**Status:** Feature Frozen / Undergoing Audit

During this phase, **no new features are merged**.
The codebase is strictly locked down to undergo rigorous external validation to ensure it meets our security and data-residency guarantees.

* **Core Objectives:**

  * **Security Audit:** Comprehensive third-party penetration testing, code review, and cryptography validation (focusing on Biscuit tokens and Ed25519 key handling).
  * **Compliance Audit:** Validation against data-residency and traceability frameworks (e.g., GDPR data residency, EU AI Act traceability, CSF SEAL-3).

* **Exit Criteria:**

  * All critical and high-severity security vulnerabilities are remediated.
  * External auditors officially sign off on the cryptographic and architectural claims.

## Phase 4: Production Stable

**Versions:** `v1.0.0` and beyond
**Status:** Mission-Critical Ready

The framework is certified for enterprise, regulated, and cross-border deployments.

* **Core Objectives:**

  * Strict enforcement of SemVer backward compatibility.

* **Exit Criteria:**

  * Continuous automated compliance scanning integrated into CI/CD.
  * Established incident response and CVE publication lifecycle.
