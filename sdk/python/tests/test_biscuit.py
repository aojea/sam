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

import base64
import json
from datetime import datetime, timedelta, timezone
from pathlib import Path

import biscuit_auth as ba
import pytest

from agent_mesh.biscuit import ROLE_ROUTER, BiscuitVerificationError, require_role, verify_peer_biscuit
from agent_mesh.controlplane import ROLE_NODE

FIXTURE = json.loads((Path(__file__).resolve().parents[2] / "testdata" / "biscuit_vectors.json").read_text())
# The vectors expire in 2035; pin "now" so they stay valid for the test's lifetime.
NOW = datetime(2026, 9, 23, tzinfo=timezone.utc)


@pytest.mark.parametrize("v", FIXTURE["vectors"], ids=[v["name"] for v in FIXTURE["vectors"]])
def test_go_minted_biscuit(v):
    trusted = [bytes.fromhex(k) for k in v["trusted_keys"]]
    token = base64.b64decode(v["biscuit"])
    if not v["valid"]:
        with pytest.raises(BiscuitVerificationError):
            verify_peer_biscuit(token, v["peer_id"], trusted, NOW)
        return
    verified = verify_peer_biscuit(token, v["peer_id"], trusted, NOW)
    assert verified.peer_id == v["peer_id"]
    assert verified.expiration == datetime.fromtimestamp(v["expiration"], tz=timezone.utc)
    assert verified.roles == v["roles"]
    assert verified.verifying_key.hex() == FIXTURE["control_plane_key"]
    if v["has_router_role"]:
        require_role(verified, ROLE_ROUTER)
        with pytest.raises(BiscuitVerificationError):
            require_role(verified, ROLE_NODE)
    else:
        require_role(verified, ROLE_NODE)
        with pytest.raises(BiscuitVerificationError):
            require_role(verified, ROLE_ROUTER)
        assert verified.labels == FIXTURE["node_token_labels"]


def test_valid_token_is_refused_past_its_expiration():
    v = next(x for x in FIXTURE["vectors"] if x["valid"])
    after = datetime.fromtimestamp(v["expiration"], tz=timezone.utc) + timedelta(seconds=1)
    with pytest.raises(BiscuitVerificationError, match="expired"):
        verify_peer_biscuit(base64.b64decode(v["biscuit"]), v["peer_id"], [bytes.fromhex(k) for k in v["trusted_keys"]], after)


def test_no_trusted_keys_means_nothing_verifies():
    v = next(x for x in FIXTURE["vectors"] if x["valid"])
    with pytest.raises(BiscuitVerificationError, match="no trusted control plane key"):
        verify_peer_biscuit(base64.b64decode(v["biscuit"]), v["peer_id"], [], NOW)


def test_tokens_minted_here_verify_and_report_their_facts():
    kp = ba.KeyPair()
    token = ba.BiscuitBuilder(
        'node("12D3KooWA4Xop1JaT3MHxwYMkCepYsv4iPVopMXwCz5iHYdBfeSB");'
        "expiration(2035-01-01T00:00:00Z); expiration(2034-06-01T00:00:00Z);"
        'role("mesh:role:node"); label("team", "plat\\"form");'
    ).build(kp.private_key)
    verified = verify_peer_biscuit(token.to_bytes(), "12D3KooWA4Xop1JaT3MHxwYMkCepYsv4iPVopMXwCz5iHYdBfeSB", [kp.public_key.to_bytes()], NOW)
    # The earliest expiration binds.
    assert verified.expiration == datetime(2034, 6, 1, tzinfo=timezone.utc)
    assert verified.labels == {"team": 'plat"form'}
    assert verified.roles == ["mesh:role:node"]
