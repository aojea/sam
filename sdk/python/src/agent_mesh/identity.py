# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""A mesh identity is an ed25519 key pair. Its peer ID is the one libp2p
derives, so the same key works in the SDK, in agentmesh-node and on the wire."""

from __future__ import annotations

import os

import multiaddr
from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey

from . import base58

PUBLIC_KEY_SIZE = 32
SEED_SIZE = 32

# libp2p crypto.proto PublicKey{Type: Ed25519 (1), Data: <32 bytes>}.
_LIBP2P_PUBLIC_KEY_PREFIX = bytes([0x08, 0x01, 0x12, 0x20])
# libp2p crypto.proto PrivateKey{Type: Ed25519 (1), Data: <seed || public>}.
_LIBP2P_PRIVATE_KEY_PREFIX = bytes([0x08, 0x01, 0x12, 0x40])
# multihash: identity function (0x00), digest length 36.
_IDENTITY_MULTIHASH_PREFIX = bytes([0x00, 0x24])


def libp2p_public_key(public_key_raw: bytes) -> bytes:
    """The libp2p protobuf encoding of an ed25519 public key."""
    if len(public_key_raw) != PUBLIC_KEY_SIZE:
        raise ValueError(f"ed25519 public key must be {PUBLIC_KEY_SIZE} bytes, got {len(public_key_raw)}")
    return _LIBP2P_PUBLIC_KEY_PREFIX + public_key_raw


def peer_id_from_public_key(public_key_raw: bytes) -> str:
    """The peer ID libp2p derives from an ed25519 public key (base58btc, "12D3Koo...")."""
    return base58.encode(_IDENTITY_MULTIHASH_PREFIX + libp2p_public_key(public_key_raw))


def canonical_peer_id(text: str) -> str:
    """The base58btc form of a peer ID written in any encoding libp2p accepts
    (base58btc multihash, CIDv1). Every key, ban set and comparison in Agent Mesh is
    on this form, as peer.ID.String() in Go; a string read off the wire or from
    a caller goes through here before it is used as one. Raises ValueError when
    the text is not a peer ID at all."""
    # py-multiaddr's p2p codec decodes both encodings and prints base58btc.
    try:
        return multiaddr.Multiaddr("/p2p/" + text).value_for_protocol("p2p")
    except Exception as err:  # noqa: BLE001 - the library raises its own parse error types
        raise ValueError(f"{text!r} is not a peer ID: {err}") from err


def verify_ed25519(public_key_raw: bytes, data: bytes, signature: bytes) -> bool:
    """Verifies an ed25519 signature with a raw 32-byte public key."""
    if len(public_key_raw) != PUBLIC_KEY_SIZE:
        return False
    try:
        Ed25519PublicKey.from_public_bytes(public_key_raw).verify(signature, data)
    except (InvalidSignature, ValueError):
        return False
    return True


class Identity:
    """An ed25519 key pair and the libp2p peer ID it maps to."""

    __slots__ = ("_private", "_seed", "public_key_raw", "peer_id")

    def __init__(self, seed: bytes):
        if len(seed) != SEED_SIZE:
            raise ValueError(f"ed25519 seed must be {SEED_SIZE} bytes, got {len(seed)}")
        self._seed = bytes(seed)
        self._private = Ed25519PrivateKey.from_private_bytes(self._seed)
        self.public_key_raw: bytes = self._private.public_key().public_bytes(
            serialization.Encoding.Raw, serialization.PublicFormat.Raw
        )
        self.peer_id: str = peer_id_from_public_key(self.public_key_raw)

    @classmethod
    def generate(cls) -> "Identity":
        return cls(os.urandom(SEED_SIZE))

    @classmethod
    def from_seed(cls, seed: bytes) -> "Identity":
        return cls(seed)

    @classmethod
    def from_libp2p_private_key(cls, data: bytes) -> "Identity":
        """Loads the libp2p protobuf private key encoding, the format agentmesh-node persists."""
        prefix = len(_LIBP2P_PRIVATE_KEY_PREFIX)
        if len(data) != prefix + 64 or not data.startswith(_LIBP2P_PRIVATE_KEY_PREFIX):
            raise ValueError("not a libp2p ed25519 private key")
        identity = cls(data[prefix : prefix + SEED_SIZE])
        if data[prefix + SEED_SIZE :] != identity.public_key_raw:
            raise ValueError("libp2p private key: public half does not match the seed")
        return identity

    def to_libp2p_private_key(self) -> bytes:
        """The libp2p protobuf private key encoding (seed || public key)."""
        return _LIBP2P_PRIVATE_KEY_PREFIX + self._seed + self.public_key_raw

    @property
    def seed(self) -> bytes:
        """The 32-byte ed25519 seed; what a libp2p implementation loads the key from."""
        return self._seed

    @property
    def libp2p_public_key(self) -> bytes:
        """The libp2p protobuf public key encoding, what the control plane stores."""
        return libp2p_public_key(self.public_key_raw)

    def sign(self, data: bytes) -> bytes:
        return self._private.sign(data)

    def verify(self, data: bytes, signature: bytes) -> bool:
        return verify_ed25519(self.public_key_raw, data, signature)

    def __repr__(self) -> str:
        return f"Identity({self.peer_id})"
