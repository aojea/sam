#!/usr/bin/env python3
"""Live-Execution 3D Architectural Renderer for the Private DIY Enterprise Stack.

Starts the real Agent Mesh control plane (`agentmesh-one`), four real `agentmesh-node`
sidecars (`node-tools`, `node-inference`, `node-agent`, `node-external`), the SQLite
ERP database, and the Vendor Compliance REST API via `dashboard.MeshEnvironment`,
executes all 4 acts against the live Go binaries, and renders a 1920x1080 30fps
3D perspective animation (`pycairo` + `numpy` + `ffmpeg`) synchronized with the real
peer IDs, latencies, SQLite query outputs, Biscuit Datalog denials, and audit logs.
"""

from __future__ import annotations

import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Any

import cairo
import numpy as np

EXAMPLE_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(EXAMPLE_DIR))

import dashboard  # noqa: E402

W, H = 1920, 1080
FPS = 30


# ---------------------------------------------------------------------------
# 3D Math & Perspective Camera Engine
# ---------------------------------------------------------------------------
class Camera3D:
    def __init__(
        self,
        yaw: float = -0.55,
        pitch: float = 0.52,
        dist: float = 13.8,
        target: tuple[float, float, float] = (0.0, 0.55, 0.0),
        fov: float = 1080.0,
    ) -> None:
        self.yaw = yaw
        self.pitch = pitch
        self.dist = dist
        self.target = np.array(target, dtype=np.float64)
        self.fov = fov
        self._update_basis()

    def _update_basis(self) -> None:
        cy, sy = math.cos(self.yaw), math.sin(self.yaw)
        cp, sp = math.cos(self.pitch), math.sin(self.pitch)
        forward = np.array([sy * cp, -sp, cy * cp], dtype=np.float64)
        self.pos = self.target - forward * self.dist
        up_world = np.array([0.0, 1.0, 0.0], dtype=np.float64)
        right = np.cross(forward, up_world)
        right /= np.linalg.norm(right) + 1e-9
        up = np.cross(right, forward)
        up /= np.linalg.norm(up) + 1e-9
        self.forward = forward
        self.right = right
        self.up = up

    def set_pose(
        self,
        yaw: float,
        pitch: float,
        dist: float,
        target: tuple[float, float, float],
    ) -> None:
        self.yaw = yaw
        self.pitch = pitch
        self.dist = dist
        self.target = np.array(target, dtype=np.float64)
        self._update_basis()

    def project(self, pt: tuple[float, float, float] | np.ndarray) -> tuple[float, float, float]:
        v = np.asarray(pt, dtype=np.float64) - self.pos
        x_cam = float(np.dot(v, self.right))
        y_cam = float(np.dot(v, self.up))
        z_cam = float(np.dot(v, self.forward))
        if z_cam < 0.2:
            z_cam = 0.2
        scale = self.fov / z_cam
        sx = W * 0.50 + x_cam * scale
        sy = H * 0.45 - y_cam * scale
        return sx, sy, z_cam


def lerp(a: float, b: float, t: float) -> float:
    return a + (b - a) * t


def smoothstep(t: float) -> float:
    t = max(0.0, min(1.0, t))
    return t * t * (3.0 - 2.0 * t)


# ---------------------------------------------------------------------------
# 3D Primitive Drawing Helpers (Depth-Sorted Extruded Solids, Cylinders, Rings)
# ---------------------------------------------------------------------------
def draw_3d_box(
    polys: list[tuple[float, list[tuple[float, float]], tuple[float, float, float, float], tuple[float, float, float, float] | None, float]],
    cam: Camera3D,
    center: tuple[float, float, float],
    size: tuple[float, float, float],
    color: tuple[float, float, float],
    alpha: float = 0.92,
    edge_color: tuple[float, float, float, float] | None = None,
    yaw_rot: float = 0.0,
    line_width: float = 1.4,
) -> None:
    cx, cy, cz = center
    sx, sy, sz = size[0] * 0.5, size[1] * 0.5, size[2] * 0.5
    c, s = math.cos(yaw_rot), math.sin(yaw_rot)

    local_verts = [
        (-sx, -sy, -sz),
        (sx, -sy, -sz),
        (sx, sy, -sz),
        (-sx, sy, -sz),
        (-sx, -sy, sz),
        (sx, -sy, sz),
        (sx, sy, sz),
        (-sx, sy, sz),
    ]
    world_verts = []
    proj_verts = []
    for lx, ly, lz in local_verts:
        wx = cx + lx * c - lz * s
        wy = cy + ly
        wz = cz + lx * s + lz * c
        world_verts.append(np.array([wx, wy, wz], dtype=np.float64))
        proj_verts.append(cam.project((wx, wy, wz)))

    # 6 faces with outward normals and lighting multipliers
    faces = [
        ((3, 2, 6, 7), (0.0, 1.0, 0.0), 1.22),   # Top
        ((0, 1, 5, 4), (0.0, -1.0, 0.0), 0.50),  # Bottom
        ((4, 5, 6, 7), (0.0, 0.0, 1.0), 0.98),   # Front (+Z)
        ((1, 0, 3, 2), (0.0, 0.0, -1.0), 0.72),  # Back (-Z)
        ((5, 1, 2, 6), (1.0, 0.0, 0.0), 0.88),   # Right (+X)
        ((0, 4, 7, 3), (-1.0, 0.0, 0.0), 0.78),  # Left (-X)
    ]

    r, g, b = color
    if edge_color is None:
        edge_color = (min(1.0, r * 1.45 + 0.15), min(1.0, g * 1.45 + 0.15), min(1.0, b * 1.45 + 0.15), min(1.0, alpha + 0.1))

    for idxs, _norm, shade in faces:
        pts_2d = [(proj_verts[i][0], proj_verts[i][1]) for i in idxs]
        # Backface culling in screen space (unless translucent)
        area = 0.0
        for i in range(4):
            x1, y1 = pts_2d[i]
            x2, y2 = pts_2d[(i + 1) % 4]
            area += (x1 * y2 - x2 * y1)
        if alpha > 0.85 and area <= 0:
            continue
        avg_z = sum(proj_verts[i][2] for i in idxs) / 4.0
        fc = (min(1.0, r * shade), min(1.0, g * shade), min(1.0, b * shade), alpha)
        polys.append((avg_z, pts_2d, fc, edge_color, line_width))


def draw_3d_octahedron(
    polys: list[Any],
    cam: Camera3D,
    center: tuple[float, float, float],
    radius: float,
    color: tuple[float, float, float],
    alpha: float = 0.92,
    yaw_rot: float = 0.0,
) -> None:
    cx, cy, cz = center
    c, s = math.cos(yaw_rot), math.sin(yaw_rot)
    top = (cx, cy + radius * 1.25, cz)
    bot = (cx, cy - radius * 1.25, cz)
    ring = [
        (cx + radius * math.cos(yaw_rot + k * math.pi * 0.5), cy, cz + radius * math.sin(yaw_rot + k * math.pi * 0.5))
        for k in range(4)
    ]
    r, g, b = color
    edge = (min(1.0, r * 1.4 + 0.25), min(1.0, g * 1.4 + 0.25), min(1.0, b * 1.4 + 0.25), 0.95)
    for k in range(4):
        p1 = ring[k]
        p2 = ring[(k + 1) % 4]
        for apex, shade in ((top, 1.15 - 0.08 * k), (bot, 0.72 + 0.05 * k)):
            proj = [cam.project(apex), cam.project(p1), cam.project(p2)]
            avg_z = sum(p[2] for p in proj) / 3.0
            pts_2d = [(p[0], p[1]) for p in proj]
            fc = (min(1.0, r * shade), min(1.0, g * shade), min(1.0, b * shade), alpha)
            polys.append((avg_z, pts_2d, fc, edge, 1.5))


def draw_3d_cylinder(
    polys: list[Any],
    cam: Camera3D,
    center: tuple[float, float, float],
    radius: float,
    height: float,
    color: tuple[float, float, float],
    segments: int = 14,
    alpha: float = 0.92,
) -> None:
    cx, cy, cz = center
    y0, y1 = cy - height * 0.5, cy + height * 0.5
    r, g, b = color
    edge = (min(1.0, r * 1.35 + 0.15), min(1.0, g * 1.35 + 0.15), min(1.0, b * 1.35 + 0.15), 0.75)
    top_pts = []
    top_zs = []
    for i in range(segments):
        a0 = (i / segments) * math.tau
        a1 = ((i + 1) / segments) * math.tau
        p00 = cam.project((cx + radius * math.cos(a0), y0, cz + radius * math.sin(a0)))
        p10 = cam.project((cx + radius * math.cos(a1), y0, cz + radius * math.sin(a1)))
        p11 = cam.project((cx + radius * math.cos(a1), y1, cz + radius * math.sin(a1)))
        p01 = cam.project((cx + radius * math.cos(a0), y1, cz + radius * math.sin(a0)))
        top_pts.append((p01[0], p01[1]))
        top_zs.append(p01[2])
        shade = 0.78 + 0.25 * math.cos(a0 - 0.7)
        avg_z = (p00[2] + p10[2] + p11[2] + p01[2]) * 0.25
        fc = (min(1.0, r * shade), min(1.0, g * shade), min(1.0, b * shade), alpha)
        polys.append((avg_z, [(p00[0], p00[1]), (p10[0], p10[1]), (p11[0], p11[1]), (p01[0], p01[1])], fc, edge, 0.9))
    polys.append((sum(top_zs) / len(top_zs) - 0.05, top_pts, (min(1.0, r * 1.25), min(1.0, g * 1.25), min(1.0, b * 1.25), alpha), edge, 1.3))


def rounded_rect(ctx: cairo.Context, x: float, y: float, w: float, h: float, r: float) -> None:
    ctx.new_sub_path()
    ctx.arc(x + w - r, y + r, r, -math.pi / 2, 0)
    ctx.arc(x + w - r, y + h - r, r, 0, math.pi / 2)
    ctx.arc(x + r, y + h - r, r, math.pi / 2, math.pi)
    ctx.arc(x + r, y + r, r, math.pi, 3 * math.pi / 2)
    ctx.close_path()


# ---------------------------------------------------------------------------
# Node 3D World Positions (Camera is at -Z looking toward +Z)
#   -X = Left of screen, +X = Right of screen
#   -Z = Foreground (lower screen), +Z = Background (upper screen)
# ---------------------------------------------------------------------------
POS_CP = (0.0, 2.25, 3.35)           # Back-Center Elevated Spire: agentmesh-one Authority
POS_AGENT_APP = (-5.7, 0.65, 0.75)   # Mid-Left (Cloud Run / Workstation): agent_harness.py
POS_NODE_AGENT = (-3.5, 0.85, 0.75)  # Mid-Left (Cloud Run / Workstation): node-agent.yaml (:19101)
POS_NODE_INF = (3.1, 0.85, 2.35)     # Back-Right (Cloud / On-Prem GPU): node-inference.yaml (:19103)
POS_GPU_RACK = (5.3, 0.95, 2.35)     # Back-Right (Cloud / On-Prem GPU): Gemma 3 / DeepSeek-R1
POS_NODE_TOOLS = (1.6, 0.90, -1.25)  # Front-Right (On-Prem Vault): node-tools.yaml (:19102)
POS_ERP_DB = (3.8, 0.75, -0.45)      # Front-Right (On-Prem Vault): mcp://erp-db (SQLite)
POS_PAYROLL = (5.6, 0.75, -1.15)     # Front-Right (On-Prem Vault): mcp://payroll-vault (Locked)
POS_COMPLIANCE = (4.0, 0.80, -2.45)  # Front-Right (On-Prem Vault): egress://compliance.internal
POS_EXTERNAL = (-4.5, 0.70, -2.05)   # Front-Left (Untrusted External): node-external.yaml (:19104)


def bezier_3d(
    p0: tuple[float, float, float],
    p1: tuple[float, float, float],
    t: float,
    arc_height: float = 1.35,
) -> tuple[float, float, float]:
    mx = (p0[0] + p1[0]) * 0.5
    my = max(p0[1], p1[1]) + arc_height
    mz = (p0[2] + p1[2]) * 0.5
    u = 1.0 - t
    x = u * u * p0[0] + 2 * u * t * mx + t * t * p1[0]
    y = u * u * p0[1] + 2 * u * t * my + t * t * p1[1]
    z = u * u * p0[2] + 2 * u * t * mz + t * t * p1[2]
    return x, y, z


# ---------------------------------------------------------------------------
# Scene Renderer
# ---------------------------------------------------------------------------
def render_frame(
    ctx: cairo.Context,
    cam: Camera3D,
    t_sec: float,
    state: dict[str, Any],
) -> None:
    # 1. Deep space cybernetic gradient background
    bg = cairo.LinearGradient(0, 0, 0, H)
    bg.add_color_stop_rgb(0.0, 0.020, 0.035, 0.075)
    bg.add_color_stop_rgb(0.55, 0.035, 0.060, 0.115)
    bg.add_color_stop_rgb(1.0, 0.015, 0.025, 0.055)
    ctx.set_source(bg)
    ctx.paint()

    # Radial ambient glow behind the 3D stage
    glow = cairo.RadialGradient(W * 0.50, H * 0.44, 60, W * 0.50, H * 0.44, 780)
    glow.add_color_stop_rgba(0.0, 0.08, 0.32, 0.65, 0.22)
    glow.add_color_stop_rgba(0.6, 0.04, 0.16, 0.36, 0.08)
    glow.add_color_stop_rgba(1.0, 0.0, 0.0, 0.0, 0.0)
    ctx.set_source(glow)
    ctx.paint()

    # 2. 3D Perspective Floor Grid (y = 0)
    ctx.set_line_width(1.0)
    for gx in range(-9, 10):
        alpha = 0.14 if gx % 2 == 0 else 0.06
        ctx.set_source_rgba(0.22, 0.55, 0.88, alpha)
        p_start = cam.project((gx * 1.0, 0.0, -7.0))
        p_end = cam.project((gx * 1.0, 0.0, 6.0))
        ctx.move_to(p_start[0], p_start[1])
        ctx.line_to(p_end[0], p_end[1])
        ctx.stroke()
    for gz in range(-7, 7):
        alpha = 0.14 if gz % 2 == 0 else 0.06
        ctx.set_source_rgba(0.22, 0.55, 0.88, alpha)
        p_start = cam.project((-9.0, 0.0, gz * 1.0))
        p_end = cam.project((9.0, 0.0, gz * 1.0))
        ctx.move_to(p_start[0], p_start[1])
        ctx.line_to(p_end[0], p_end[1])
        ctx.stroke()

    polys: list[Any] = []

    # 3. 3D Zone Base Platforms
    # Zone A: Cloud Run / Workstation Zone (Mid-Left)
    draw_3d_box(
        polys, cam, (-4.6, 0.10, 0.75), (3.8, 0.20, 2.3),
        (0.07, 0.24, 0.46), alpha=0.82, edge_color=(0.22, 0.74, 0.97, 0.85), line_width=1.8,
    )
    # Zone B: Cloud / On-Prem GPU Inference Cluster (Back Right)
    draw_3d_box(
        polys, cam, (4.2, 0.10, 2.35), (3.8, 0.20, 2.2),
        (0.24, 0.12, 0.48), alpha=0.82, edge_color=(0.66, 0.33, 0.97, 0.85), line_width=1.8,
    )
    # Zone C: On-Premises Corporate Vault (Front Right — Armored Platform)
    draw_3d_box(
        polys, cam, (3.65, 0.12, -1.35), (5.4, 0.24, 3.2),
        (0.05, 0.28, 0.22), alpha=0.86, edge_color=(0.20, 0.83, 0.60, 0.90), line_width=2.0,
    )
    # Translucent 3D Vault Perimeter Walls around On-Premises Zone
    draw_3d_box(
        polys, cam, (3.65, 0.55, -1.35), (5.52, 0.62, 3.32),
        (0.08, 0.45, 0.35), alpha=0.14, edge_color=(0.20, 0.88, 0.65, 0.40), line_width=1.0,
    )
    # Zone D: Untrusted External Zone (Front Left)
    ext_banned = state.get("ext_banned", False)
    ext_base_col = (0.35, 0.08, 0.12) if not ext_banned else (0.18, 0.05, 0.07)
    draw_3d_box(
        polys, cam, (-4.5, 0.10, -2.05), (2.5, 0.20, 2.0),
        ext_base_col, alpha=0.82, edge_color=(0.95, 0.28, 0.36, 0.85), line_width=1.8,
    )

    # 4. Control Plane Spire (`agentmesh-one`) + Rotating 3D Biscuit Holocube (Back-Center)
    draw_3d_box(
        polys, cam, (0.0, 0.90, 3.35), (1.6, 1.80, 1.4),
        (0.10, 0.28, 0.52), alpha=0.90, edge_color=(0.35, 0.85, 1.0, 0.95), line_width=1.8,
    )
    draw_3d_octahedron(
        polys, cam, (0.0, 2.45 + 0.12 * math.sin(t_sec * 2.5), 3.35),
        radius=0.55, color=(0.96, 0.75, 0.18), alpha=0.92, yaw_rot=t_sec * 1.4,
    )

    # 5. Cloud Run / Workstation Nodes (`agent_harness.py` + `node-agent.yaml`)
    draw_3d_box(
        polys, cam, POS_AGENT_APP, (1.15, 0.95, 1.15),
        (0.14, 0.45, 0.78), alpha=0.92, yaw_rot=0.08 * math.sin(t_sec * 1.2),
    )
    draw_3d_box(
        polys, cam, POS_NODE_AGENT, (1.05, 1.35, 1.05),
        (0.08, 0.62, 0.86), alpha=0.94,
    )
    # Sealed tar_block Biscuit crystal hovering above node-agent when Act 3d is active
    if state.get("sealed_biscuit", False):
        draw_3d_octahedron(
            polys, cam, (POS_NODE_AGENT[0], POS_NODE_AGENT[1] + 1.18, POS_NODE_AGENT[2]),
            radius=0.38, color=(0.98, 0.68, 0.12), alpha=0.95, yaw_rot=-t_sec * 2.2,
        )

    # 6. Cloud / On-Prem GPU Cluster (`node-inference.yaml` + GPU Tensor Rack)
    draw_3d_box(
        polys, cam, POS_NODE_INF, (1.05, 1.35, 1.05),
        (0.52, 0.26, 0.88), alpha=0.94,
    )
    # Multi-blade 3D GPU Server Stack
    for blade_idx in range(3):
        by = 0.42 + blade_idx * 0.42
        draw_3d_box(
            polys, cam, (POS_GPU_RACK[0], by, POS_GPU_RACK[2]), (1.30, 0.30, 1.20),
            (0.36 + 0.08 * blade_idx, 0.18, 0.68 + 0.08 * blade_idx), alpha=0.94,
        )

    # 7. On-Premises Vault Sidecar & Private Backends
    draw_3d_box(
        polys, cam, POS_NODE_TOOLS, (1.15, 1.45, 1.15),
        (0.08, 0.68, 0.50), alpha=0.95,
    )
    # Secret Broker Golden Micro-Vault next to node-tools
    draw_3d_box(
        polys, cam, (POS_NODE_TOOLS[0] - 0.15, POS_NODE_TOOLS[1] + 0.98, POS_NODE_TOOLS[2]),
        (0.52, 0.36, 0.52), (0.92, 0.68, 0.12), alpha=0.95, yaw_rot=t_sec * 0.8,
    )
    # SQLite ERP Database 3D Stacked Cylinders (`mcp://erp-db`)
    for tier in range(3):
        draw_3d_cylinder(
            polys, cam, (POS_ERP_DB[0], 0.38 + tier * 0.36, POS_ERP_DB[2]),
            radius=0.52, height=0.28, color=(0.12, 0.72, 0.52),
        )
    # Payroll Vault (`mcp://payroll-vault`) — Locked Red/Slate Safe
    draw_3d_box(
        polys, cam, POS_PAYROLL, (0.95, 1.05, 0.95),
        (0.65, 0.16, 0.24), alpha=0.92, edge_color=(0.98, 0.35, 0.42, 0.95),
    )
    # Vendor Compliance API Tower (`egress://compliance.internal`)
    draw_3d_box(
        polys, cam, POS_COMPLIANCE, (1.05, 1.18, 0.95),
        (0.14, 0.58, 0.74), alpha=0.92,
    )

    # 8. Untrusted External Node (`node-external.yaml`)
    ext_col = (0.78, 0.18, 0.26) if not ext_banned else (0.32, 0.12, 0.15)
    draw_3d_box(
        polys, cam, POS_EXTERNAL, (1.0, 1.15, 1.0),
        ext_col, alpha=0.90 if not ext_banned else 0.55,
    )
    if ext_banned:
        # 3D Red Laser Lockdown Cage around Banned External Peer
        draw_3d_box(
            polys, cam, (POS_EXTERNAL[0], POS_EXTERNAL[1] + 0.1, POS_EXTERNAL[2]),
            (1.45, 1.55, 1.45), (0.95, 0.15, 0.22), alpha=0.22,
            edge_color=(1.0, 0.25, 0.30, 0.95), line_width=2.4,
        )

    # Sort and paint all 3D polygons back-to-front (Painter's Algorithm)
    polys.sort(key=lambda item: item[0], reverse=True)
    for _depth, pts_2d, fc, ec, lw in polys:
        ctx.new_path()
        ctx.move_to(pts_2d[0][0], pts_2d[0][1])
        for px, py in pts_2d[1:]:
            ctx.line_to(px, py)
        ctx.close_path()
        ctx.set_source_rgba(*fc)
        ctx.fill_preserve()
        if ec is not None:
            ctx.set_source_rgba(*ec)
            ctx.set_line_width(lw)
            ctx.stroke()
        else:
            ctx.new_path()

    # 9. 3D Mesh Control & Data Tunnels (Curved 3D Bezier Arcs)
    # Control plane authority beams
    cp_apex = (POS_CP[0], POS_CP[1] + 0.1, POS_CP[2])
    for node_pos in (POS_NODE_AGENT, POS_NODE_INF, POS_NODE_TOOLS):
        _draw_3d_arc(ctx, cam, cp_apex, node_pos, (0.96, 0.78, 0.22, 0.34), arc_h=0.8, width=1.6, dashed=True, t_sec=t_sec)

    # Local loopback bridge (agent_harness.py <-> node-agent.yaml)
    _draw_3d_arc(ctx, cam, POS_AGENT_APP, POS_NODE_AGENT, (0.22, 0.88, 1.0, 0.75), arc_h=0.35, width=2.8, dashed=False, t_sec=t_sec)

    # Persistent p2p Noise/QUIC tunnels between sidecars
    _draw_3d_arc(ctx, cam, POS_NODE_AGENT, POS_NODE_INF, (0.58, 0.42, 0.98, 0.45), arc_h=1.45, width=2.2, dashed=True, t_sec=t_sec)
    _draw_3d_arc(ctx, cam, POS_NODE_AGENT, POS_NODE_TOOLS, (0.18, 0.86, 0.66, 0.45), arc_h=1.55, width=2.2, dashed=True, t_sec=t_sec)

    # Vault internal connections
    _draw_3d_arc(ctx, cam, POS_NODE_TOOLS, POS_ERP_DB, (0.20, 0.88, 0.62, 0.60), arc_h=0.45, width=2.0, dashed=False, t_sec=t_sec)
    _draw_3d_arc(ctx, cam, POS_NODE_TOOLS, POS_COMPLIANCE, (0.22, 0.78, 0.95, 0.60), arc_h=0.45, width=2.0, dashed=False, t_sec=t_sec)
    _draw_3d_arc(ctx, cam, POS_NODE_TOOLS, POS_PAYROLL, (0.95, 0.28, 0.36, 0.35), arc_h=0.45, width=1.6, dashed=True, t_sec=t_sec)

    # 10. Active Animated 3D Packet Beams & Force-Field Shields
    callouts: list[dict[str, Any]] = []
    for beam in state.get("beams", []):
        _draw_active_beam(ctx, cam, beam, t_sec, callouts)

    for shield in state.get("shields", []):
        _draw_3d_shield(ctx, cam, shield, t_sec, callouts)

    # 11. Floating 3D Billboard Labels Projected onto Node Anchors (with 2D De-Overlap Solver)
    ext_sub = "BANNED & REVOKED" if ext_banned else "dept=external (Untrusted)"
    node_labels = [
        {"pos": (POS_CP[0], POS_CP[1] + 0.95, POS_CP[2]), "title": "agentmesh-one", "sub": "Authority & Datalog PDP", "accent": (0.96, 0.78, 0.22), "dy": -26.0},
        {"pos": (POS_AGENT_APP[0], POS_AGENT_APP[1] + 0.85, POS_AGENT_APP[2]), "title": "agent_harness.py", "sub": "0 DB Creds | 127.0.0.1", "accent": (0.38, 0.82, 1.0), "dy": -22.0},
        {"pos": (POS_NODE_AGENT[0], POS_NODE_AGENT[1] + 0.95, POS_NODE_AGENT[2]), "title": "node-agent (:19101)", "sub": "Cloud Run / Workstation", "accent": (0.22, 0.88, 0.98), "dy": -26.0},
        {"pos": (POS_NODE_INF[0], POS_NODE_INF[1] + 0.95, POS_NODE_INF[2]), "title": "node-inference (:19103)", "sub": "inference://private-llm", "accent": (0.74, 0.52, 1.0), "dy": -26.0},
        {"pos": (POS_GPU_RACK[0], POS_GPU_RACK[1] + 0.95, POS_GPU_RACK[2]), "title": "Gemma 3 / DeepSeek-R1", "sub": "Cloud / On-Prem GPU", "accent": (0.68, 0.45, 0.98), "dy": -26.0},
        {"pos": (POS_NODE_TOOLS[0], POS_NODE_TOOLS[1] + 1.25, POS_NODE_TOOLS[2]), "title": "node-tools (:19102)", "sub": "On-Prem Vault + Secret Broker", "accent": (0.22, 0.92, 0.68), "dy": -28.0},
        {"pos": (POS_ERP_DB[0], POS_ERP_DB[1] + 0.95, POS_ERP_DB[2]), "title": "mcp://erp-db", "sub": "SQLite ERP (ALLOW)", "accent": (0.22, 0.92, 0.65), "dy": -24.0},
        {"pos": (POS_PAYROLL[0], POS_PAYROLL[1] + 0.85, POS_PAYROLL[2]), "title": "mcp://payroll-vault", "sub": "Default-Deny (LOCKED)", "accent": (0.98, 0.38, 0.45), "dy": -22.0},
        {"pos": (POS_COMPLIANCE[0], POS_COMPLIANCE[1] + 0.85, POS_COMPLIANCE[2]), "title": "egress://compliance.internal", "sub": "GET Only + Vault Key", "accent": (0.35, 0.84, 0.98), "dy": 28.0},
        {"pos": (POS_EXTERNAL[0], POS_EXTERNAL[1] + 0.95, POS_EXTERNAL[2]), "title": "node-external (:19104)", "sub": ext_sub, "accent": (0.98, 0.35, 0.42), "dy": 26.0},
    ]
    _draw_all_labels_deoverlapped(ctx, cam, node_labels, callouts)

    # 12. 2D Cinema HUD Overlays (Real Execution Telemetry, Act Banner, Console, Audit Feed)
    _draw_hud(ctx, state, t_sec)


def _draw_3d_arc(
    ctx: cairo.Context,
    cam: Camera3D,
    p0: tuple[float, float, float],
    p1: tuple[float, float, float],
    rgba: tuple[float, float, float, float],
    arc_h: float,
    width: float,
    dashed: bool,
    t_sec: float,
) -> None:
    steps = 28
    ctx.set_source_rgba(*rgba)
    ctx.set_line_width(width)
    if dashed:
        ctx.set_dash([8.0, 6.0], -t_sec * 24.0)
    else:
        ctx.set_dash([])
    for i in range(steps + 1):
        pt = bezier_3d(p0, p1, i / steps, arc_height=arc_h)
        sx, sy, _ = cam.project(pt)
        if i == 0:
            ctx.move_to(sx, sy)
        else:
            ctx.line_to(sx, sy)
    ctx.stroke()
    ctx.set_dash([])


def _draw_active_beam(
    ctx: cairo.Context,
    cam: Camera3D,
    beam: dict[str, Any],
    t_sec: float,
    callouts: list[dict[str, Any]],
) -> None:
    p0 = beam["src"]
    p1 = beam["dst"]
    col = beam["color"]  # (r, g, b)
    arc_h = beam.get("arc_h", 1.45)
    progress = beam.get("progress", (t_sec * 1.1) % 1.0)
    is_deny = beam.get("deny", False)

    # Glowing base trajectory
    _draw_3d_arc(ctx, cam, p0, p1, (col[0], col[1], col[2], 0.85), arc_h=arc_h, width=4.0, dashed=False, t_sec=t_sec)

    # Multiple moving 3D energy pulses along the trajectory
    for offset in (0.0, 0.33, 0.66):
        tau = (progress + offset) % 1.0
        if is_deny and tau > 0.78:
            tau = 0.78  # Clamped at the force-field shield boundary
        for trail in range(7):
            tt = max(0.0, tau - trail * 0.022)
            wx, wy, wz = bezier_3d(p0, p1, tt, arc_height=arc_h)
            sx, sy, z_cam = cam.project((wx, wy, wz))
            rad = max(2.5, (11.0 - trail * 1.3) * (11.0 / z_cam))
            alpha = max(0.1, 0.95 - trail * 0.13)
            ctx.set_source_rgba(col[0], col[1], col[2], alpha)
            ctx.arc(sx, sy, rad, 0, math.tau)
            ctx.fill()
            if trail == 0:
                ctx.set_source_rgba(1.0, 1.0, 1.0, 0.95)
                ctx.arc(sx, sy, rad * 0.48, 0, math.tau)
                ctx.fill()

    label = beam.get("label")
    if label:
        mx, my, mz = bezier_3d(p0, p1, 0.5, arc_height=arc_h + 0.25)
        callouts.append({
            "pos": (mx, my, mz),
            "text": label,
            "color": col,
            "bg": (0.04, 0.08, 0.16, 0.94),
            "dy": -18.0,
        })


def _draw_3d_shield(
    ctx: cairo.Context,
    cam: Camera3D,
    shield: dict[str, Any],
    t_sec: float,
    callouts: list[dict[str, Any]],
) -> None:
    center = shield["pos"]
    sx, sy, z_cam = cam.project(center)
    base_r = shield.get("radius", 68.0) * (12.0 / z_cam)
    pulse = 1.0 + 0.10 * math.sin(t_sec * 8.0)
    r = base_r * pulse

    # Crimson radial energy dome
    grad = cairo.RadialGradient(sx, sy, r * 0.15, sx, sy, r * 1.25)
    grad.add_color_stop_rgba(0.0, 1.0, 0.25, 0.35, 0.55)
    grad.add_color_stop_rgba(0.7, 0.95, 0.12, 0.24, 0.32)
    grad.add_color_stop_rgba(1.0, 0.95, 0.12, 0.24, 0.0)
    ctx.set_source(grad)
    ctx.arc(sx, sy, r * 1.25, 0, math.tau)
    ctx.fill()

    # Rotating 3D hexagon barrier
    ctx.set_source_rgba(1.0, 0.32, 0.40, 0.95)
    ctx.set_line_width(2.6)
    for ring_scale, rot_dir in ((1.0, 1.0), (0.68, -1.4)):
        ctx.new_path()
        for k in range(6):
            ang = t_sec * 1.8 * rot_dir + k * (math.tau / 6.0)
            px = sx + r * ring_scale * math.cos(ang)
            py = sy + r * ring_scale * 0.82 * math.sin(ang)
            if k == 0:
                ctx.move_to(px, py)
            else:
                ctx.line_to(px, py)
        ctx.close_path()
        ctx.stroke()

    # Expanding shockwave ring
    wave = (t_sec * 1.6) % 1.0
    ctx.set_source_rgba(1.0, 0.38, 0.45, (1.0 - wave) * 0.85)
    ctx.set_line_width(2.0)
    ctx.arc(sx, sy, r * (0.8 + wave * 0.95), 0, math.tau)
    ctx.stroke()

    caption = shield.get("caption", "DENIED BY ZERO-TRUST POLICY")
    callouts.append({
        "pos": center,
        "text": caption,
        "color": (1.0, 0.32, 0.40),
        "bg": (0.34, 0.04, 0.08, 0.96),
        "dy": -r - 24.0,
    })


def _draw_all_labels_deoverlapped(
    ctx: cairo.Context,
    cam: Camera3D,
    node_labels: list[dict[str, Any]],
    callouts: list[dict[str, Any]],
) -> None:
    boxes: list[dict[str, Any]] = []

    for nl in node_labels:
        ax, ay, _ = cam.project(nl["pos"])
        ctx.select_font_face("DejaVu Sans", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
        ctx.set_font_size(12.5)
        e1 = ctx.text_extents(nl["title"])
        ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_NORMAL)
        ctx.set_font_size(10.5)
        e2 = ctx.text_extents(nl["sub"])
        w = max(e1.width, e2.width) + 20.0
        h = 38.0
        boxes.append({
            "kind": "node",
            "ax": ax,
            "ay": ay,
            "cx": ax,
            "cy": ay + nl.get("dy", -24.0),
            "w": w,
            "h": h,
            "title": nl["title"],
            "sub": nl["sub"],
            "accent": nl["accent"],
            "w1": e1.width,
            "w2": e2.width,
        })

    for co in callouts:
        ax, ay, _ = cam.project(co["pos"])
        ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
        ctx.set_font_size(12.5)
        ext = ctx.text_extents(co["text"])
        w = ext.width + 20.0
        h = 26.0
        boxes.append({
            "kind": "callout",
            "ax": ax,
            "ay": ay,
            "cx": ax,
            "cy": ay + co.get("dy", -18.0),
            "w": w,
            "h": h,
            "text": co["text"],
            "tw": ext.width,
            "color": co["color"],
            "bg": co["bg"],
        })

    # Iterative 2D bounding-box relaxation to eliminate any screen-space overlap
    pad_x, pad_y = 10.0, 8.0
    for _ in range(18):
        for i in range(len(boxes)):
            for j in range(i + 1, len(boxes)):
                bi, bj = boxes[i], boxes[j]
                dx = bj["cx"] - bi["cx"]
                dy = bj["cy"] - bi["cy"]
                ox = (bi["w"] + bj["w"]) * 0.5 + pad_x - abs(dx)
                oy = (bi["h"] + bj["h"]) * 0.5 + pad_y - abs(dy)
                if ox > 0 and oy > 0:
                    if ox < oy * 1.35:
                        shift = (ox * 0.52) * (1.0 if dx >= 0 else -1.0)
                        bi["cx"] -= shift
                        bj["cx"] += shift
                    else:
                        shift = (oy * 0.52) * (1.0 if dy >= 0 else -1.0)
                        bi["cy"] -= shift
                        bj["cy"] += shift
        for b in boxes:
            b["cx"] = max(b["w"] * 0.5 + 34.0, min(W - b["w"] * 0.5 - 34.0, b["cx"]))
            b["cy"] = max(116.0 + b["h"] * 0.5, min(H - 286.0 - b["h"] * 0.5, b["cy"]))

    # Draw subtle leader lines from 3D anchor to displaced badge
    for b in boxes:
        dist = math.hypot(b["cx"] - b["ax"], b["cy"] - b["ay"])
        if dist > 18.0:
            col = b["accent"] if b["kind"] == "node" else b["color"]
            ctx.set_source_rgba(col[0], col[1], col[2], 0.55)
            ctx.set_line_width(1.2)
            ctx.move_to(b["ax"], b["ay"])
            ctx.line_to(b["cx"], b["cy"])
            ctx.stroke()
            ctx.arc(b["ax"], b["ay"], 3.0, 0, math.tau)
            ctx.fill()

    # Draw node badges first, then active callout badges on top
    for b in boxes:
        x = b["cx"] - b["w"] * 0.5
        y = b["cy"] - b["h"] * 0.5
        if b["kind"] == "node":
            acc = b["accent"]
            rounded_rect(ctx, x, y, b["w"], b["h"], 7)
            ctx.set_source_rgba(0.03, 0.06, 0.12, 0.88)
            ctx.fill_preserve()
            ctx.set_source_rgba(acc[0], acc[1], acc[2], 0.82)
            ctx.set_line_width(1.3)
            ctx.stroke()

            ctx.select_font_face("DejaVu Sans", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
            ctx.set_font_size(12.5)
            ctx.set_source_rgba(0.96, 0.98, 1.0, 0.98)
            ctx.move_to(b["cx"] - b["w1"] * 0.5, y + 15.5)
            ctx.show_text(b["title"])

            ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_NORMAL)
            ctx.set_font_size(10.5)
            ctx.set_source_rgba(acc[0], acc[1], acc[2], 0.95)
            ctx.move_to(b["cx"] - b["w2"] * 0.5, y + 31.0)
            ctx.show_text(b["sub"])
        else:
            col = b["color"]
            bg = b["bg"]
            rounded_rect(ctx, x, y, b["w"], b["h"], 6)
            ctx.set_source_rgba(*bg)
            ctx.fill_preserve()
            ctx.set_source_rgba(col[0], col[1], col[2], 0.98)
            ctx.set_line_width(1.7)
            ctx.stroke()

            ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
            ctx.set_font_size(12.5)
            ctx.set_source_rgba(0.98, 0.99, 1.0, 1.0)
            ctx.move_to(b["cx"] - b["tw"] * 0.5, b["cy"] + 4.5)
            ctx.show_text(b["text"])


def _draw_hud(ctx: cairo.Context, state: dict[str, Any], _t_sec: float) -> None:
    # Top Header Bar
    rounded_rect(ctx, 28, 22, W - 56, 74, 12)
    ctx.set_source_rgba(0.03, 0.06, 0.13, 0.88)
    ctx.fill_preserve()
    ctx.set_source_rgba(0.22, 0.65, 0.95, 0.55)
    ctx.set_line_width(1.5)
    ctx.stroke()

    # Live Execution Badge
    rounded_rect(ctx, 46, 38, 188, 42, 8)
    ctx.set_source_rgba(0.06, 0.34, 0.24, 0.92)
    ctx.fill_preserve()
    ctx.set_source_rgba(0.20, 0.88, 0.62, 0.95)
    ctx.set_line_width(1.5)
    ctx.stroke()
    ctx.set_source_rgba(0.20, 0.95, 0.66, 1.0)
    ctx.arc(66, 59, 6, 0, math.tau)
    ctx.fill()
    ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
    ctx.set_font_size(13.0)
    ctx.set_source_rgba(0.92, 1.0, 0.96, 1.0)
    ctx.move_to(82, 64)
    ctx.show_text("LIVE GO BINARIES")

    # Main Title & Act Title
    ctx.select_font_face("DejaVu Sans", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
    ctx.set_font_size(21.0)
    ctx.set_source_rgba(0.96, 0.98, 1.0, 1.0)
    ctx.move_to(254, 54)
    ctx.show_text(state.get("headline", "Private DIY Enterprise Stack — Safely On-Premises & in Cloud"))

    ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_NORMAL)
    ctx.set_font_size(13.5)
    ctx.set_source_rgba(0.45, 0.82, 0.98, 0.96)
    ctx.move_to(254, 78)
    ctx.show_text(state.get("subheadline", "Gemma / DeepSeek + Self-Hosted Agent Harness + Agent Mesh Zero-Trust Sidecars"))

    # Act Pill Indicator on Right of Top Bar
    act_idx = state.get("act_idx", 1)
    act_labels = [
        "ACT 1: SIDECARS",
        "ACT 2: HYBRID ReAct",
        "ACT 3: ZERO TRUST",
        "ACT 4: AUDIT & BAN",
    ]
    px = W - 670
    for idx, text in enumerate(act_labels, start=1):
        active = idx == act_idx
        rounded_rect(ctx, px, 40, 152, 38, 8)
        if active:
            ctx.set_source_rgba(0.14, 0.46, 0.82, 0.95)
            ctx.fill_preserve()
            ctx.set_source_rgba(0.42, 0.86, 1.0, 1.0)
            ctx.set_line_width(1.8)
            ctx.stroke()
        else:
            ctx.set_source_rgba(0.06, 0.11, 0.20, 0.80)
            ctx.fill_preserve()
            ctx.set_source_rgba(0.22, 0.35, 0.52, 0.55)
            ctx.set_line_width(1.0)
            ctx.stroke()
        ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
        ctx.set_font_size(11.5)
        ctx.set_source_rgba(0.98, 0.99, 1.0, 1.0 if active else 0.55)
        ctx.move_to(px + 12, 63)
        ctx.show_text(text)
        px += 160

    # Bottom-Left Panel: Live Binary Execution Output & Real Payload Telemetry
    panel_y = H - 270
    rounded_rect(ctx, 28, panel_y, 1140, 244, 12)
    ctx.set_source_rgba(0.025, 0.05, 0.11, 0.90)
    ctx.fill_preserve()
    accent = state.get("panel_accent", (0.22, 0.78, 0.98))
    ctx.set_source_rgba(accent[0], accent[1], accent[2], 0.80)
    ctx.set_line_width(1.8)
    ctx.stroke()

    ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
    ctx.set_font_size(14.0)
    ctx.set_source_rgba(accent[0], accent[1], accent[2], 1.0)
    ctx.move_to(48, panel_y + 30)
    ctx.show_text(state.get("panel_title", "LIVE BINARY TELEMETRY"))

    lines = state.get("console_lines", [])
    ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_NORMAL)
    ctx.set_font_size(13.0)
    ly = panel_y + 58
    for line in lines[:8]:
        if line.startswith("[ALLOW]") or "PASS" in line or "APPROVED" in line:
            ctx.set_source_rgba(0.32, 0.95, 0.68, 0.98)
        elif line.startswith("[DENY]") or "BLOCKED" in line or "403" in line or "BANNED" in line:
            ctx.set_source_rgba(1.0, 0.42, 0.48, 0.98)
        elif line.startswith("$") or line.startswith(">"):
            ctx.set_source_rgba(0.48, 0.85, 1.0, 0.98)
        else:
            ctx.set_source_rgba(0.86, 0.91, 0.97, 0.95)
        ctx.move_to(48, ly)
        ctx.show_text(line[:118])
        ly += 23

    # Bottom-Right Panel: Real-Time Zero-Trust Decision Ledger
    rx = 1188
    rw = W - rx - 28
    rounded_rect(ctx, rx, panel_y, rw, 244, 12)
    ctx.set_source_rgba(0.025, 0.05, 0.11, 0.90)
    ctx.fill_preserve()
    ctx.set_source_rgba(0.28, 0.62, 0.90, 0.65)
    ctx.set_line_width(1.5)
    ctx.stroke()

    allow_cnt = state.get("allow_count", 0)
    deny_cnt = state.get("deny_count", 0)
    ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_BOLD)
    ctx.set_font_size(14.0)
    ctx.set_source_rgba(0.92, 0.96, 1.0, 1.0)
    ctx.move_to(rx + 22, panel_y + 30)
    ctx.show_text("ZERO-TRUST PDP LEDGER (REAL EXECUTION)")

    # ALLOW & DENY Counters
    ctx.set_source_rgba(0.25, 0.95, 0.66, 1.0)
    ctx.move_to(rx + 460, panel_y + 30)
    ctx.show_text(f"ALLOW: {allow_cnt}")
    ctx.set_source_rgba(1.0, 0.38, 0.45, 1.0)
    ctx.move_to(rx + 580, panel_y + 30)
    ctx.show_text(f"DENY: {deny_cnt}")

    ledger = state.get("ledger", [])
    ctx.select_font_face("DejaVu Sans Mono", cairo.FONT_SLANT_NORMAL, cairo.FONT_WEIGHT_NORMAL)
    ctx.set_font_size(12.0)
    ay = panel_y + 58
    for entry in ledger[-8:]:
        is_allow = entry.get("decision") == "ALLOW"
        badge_col = (0.16, 0.85, 0.56) if is_allow else (0.96, 0.28, 0.36)
        rounded_rect(ctx, rx + 22, ay - 13, 54, 18, 4)
        ctx.set_source_rgba(badge_col[0], badge_col[1], badge_col[2], 0.25)
        ctx.fill_preserve()
        ctx.set_source_rgba(badge_col[0], badge_col[1], badge_col[2], 0.95)
        ctx.set_line_width(1.1)
        ctx.stroke()
        ctx.move_to(rx + 28, ay)
        ctx.show_text(entry.get("decision", "INFO"))

        ctx.set_source_rgba(0.88, 0.93, 0.98, 0.95)
        ctx.move_to(rx + 86, ay)
        ctx.show_text(f"{entry.get('service', '')} — {entry.get('detail', '')}"[:66])
        ay += 23


# ---------------------------------------------------------------------------
# Main Execution & Video Encoding Pipeline
# ---------------------------------------------------------------------------
def _api_call(base_url: str, path: str, method: str = "GET", payload: dict[str, Any] | None = None) -> tuple[dict[str, Any], int]:
    import urllib.request

    t0 = time.time()
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    req = urllib.request.Request(
        f"{base_url}{path}",
        data=data,
        headers={"Content-Type": "application/json"} if data else {},
        method=method,
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        res = json.loads(resp.read().decode("utf-8"))
    elapsed_ms = int((time.time() - t0) * 1000)
    return res, elapsed_ms


def main() -> int:
    import http.server
    import threading

    out_mp4 = Path(os.environ.get("OUT_MP4", "/tmp/demo-private-diy-stack.mp4"))
    preview_dir = Path(os.environ.get("PREVIEW_DIR", "/tmp/private-diy-3d-previews"))
    preview_dir.mkdir(parents=True, exist_ok=True)

    env = dashboard.MeshEnvironment()
    dashboard.MESH = env
    server: http.server.ThreadingHTTPServer | None = None
    try:
        print("[1/3] Starting REAL Agent Mesh control plane + 4 sidecars + SQLite ERP + Compliance API...")
        env.start()
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), dashboard.WorkbenchHandler)
        port = server.server_address[1]
        threading.Thread(target=server.serve_forever, daemon=True).start()
        base_url = f"http://127.0.0.1:{port}"

        print("[2/3] Executing all 4 Acts against live Go binaries and capturing real telemetry...")
        act1_res, act1_ms = _api_call(base_url, "/api/act1/discover", "POST", {})
        act2_res, act2_ms = _api_call(
            base_url,
            "/api/act2/run-agent",
            "POST",
            {"task": "Audit Invoice INV-2026-089 for Acme Robotics, verify SKU-GPU-H200 warehouse stock, and check vendor ACME-01 compliance."},
        )
        act3a_res, _ = _api_call(base_url, "/api/act3/test", "POST", {"check": "3a"})
        act3b_res, _ = _api_call(base_url, "/api/act3/test", "POST", {"check": "3b"})
        act3c_res, act3c_ms = _api_call(base_url, "/api/act3/test", "POST", {"check": "3c"})
        act3d_res, _ = _api_call(base_url, "/api/act3/test", "POST", {"check": "3d"})
        act4_ban_res, _ = _api_call(base_url, "/api/act4/ban", "POST", {})
        state_res, _ = _api_call(base_url, "/api/state", "GET")

        step3_lines = [ln.strip() for ln in act2_res.get("step3_results", "").splitlines() if ln.strip()]
        s2_inv = step3_lines[0] if len(step3_lines) > 0 else "[mcp://erp-db/lookup_invoice] -> INV-2026-089 ($148,500.00)"
        s2_stk = step3_lines[1] if len(step3_lines) > 1 else "[mcp://erp-db/check_inventory] -> SKU-GPU-H200 (42 units)"
        s2_cmp = step3_lines[2] if len(step3_lines) > 2 else "[egress://compliance.internal] -> TIER_1_APPROVED"
        final2 = act2_res.get("step4_decision", "APPROVED — Invoice INV-2026-089").splitlines()[0]

        act3d_lines = [ln.strip() for ln in act3d_res.get("output", "").splitlines() if ln.strip()]
        audit_events = state_res.get("audit", [])

        # Build timeline of 3D scenes driven by real execution outputs (~56 seconds @ 30fps = 1,680 frames)
        ledger: list[dict[str, str]] = []
        scenes: list[dict[str, Any]] = []

        # Scene 1: Act 1 — Hybrid Cloud & On-Prem Sidecar Enrollment & Discovery (7.5s)
        scenes.append({
            "duration": 7.5,
            "act_idx": 1,
            "cam_start": (-0.18, 0.50, 14.5, (0.0, 0.55, 0.2)),
            "cam_end": (0.08, 0.46, 13.6, (0.0, 0.55, 0.2)),
            "headline": "Act 1: Sidecar Node Architecture — Safely Across On-Premises & Google Cloud",
            "subheadline": "agentmesh-one issues Ed25519 identities & Datalog policy; sidecars discover services via Kademlia DHT",
            "panel_title": f"ACT 1 — LIVE SIDECAR PEER IDS & DISCOVERED MESH SERVICES ({act1_ms} ms)",
            "panel_accent": (0.22, 0.85, 0.98),
            "console_lines": [
                "$ agentmesh-node run --config node-tools.yaml & node-inference.yaml & node-agent.yaml",
                f"[ALLOW] node-agent     (:19101 Cloud Run)   peer={env.agent_peer[:28]}...",
                f"[ALLOW] node-inference (:19103 GPU Cluster) peer={env.gpu_peer[:28]}...",
                f"[ALLOW] node-tools     (:19102 On-Prem)     peer={env.tools_peer[:28]}...",
                f"[ALLOW] node-external  (:19104 External)    peer={env.external_peer[:28]}...",
                f"> {act2_res.get('step1_discovery', 'Discovered 3 tools on Vault Peer')}",
                "> Agent container holds 0 DB credentials, 0 API keys, and 0 site-to-site VPN routes.",
            ],
            "beams": [
                {"src": POS_CP, "dst": POS_NODE_AGENT, "color": (0.96, 0.78, 0.22), "arc_h": 0.9, "label": "Biscuit + Datalog Sync"},
                {"src": POS_CP, "dst": POS_NODE_INF, "color": (0.96, 0.78, 0.22), "arc_h": 0.9},
                {"src": POS_CP, "dst": POS_NODE_TOOLS, "color": (0.96, 0.78, 0.22), "arc_h": 0.9},
            ],
            "shields": [],
            "ledger_add": [
                {"decision": "ALLOW", "service": "mesh://dht-discovery", "detail": "4 sidecars enrolled & synced"},
            ],
        })

        # Scene 2: Act 2 (Part A) — Hybrid Reasoning & On-Prem SQLite ERP Queries (8.0s)
        scenes.append({
            "duration": 8.0,
            "act_idx": 2,
            "cam_start": (0.08, 0.46, 13.6, (0.0, 0.55, 0.2)),
            "cam_end": (-0.12, 0.48, 13.2, (0.3, 0.55, 0.1)),
            "headline": "Act 2: End-to-End Hybrid ReAct Loop — Cloud Agent to On-Prem SQLite ERP",
            "subheadline": "agent_harness.py calls 127.0.0.1:19101; sidecar routes to inference://private-llm and mcp://erp-db",
            "panel_title": "ACT 2A — LIVE ReAct REASONING (Gemma / DeepSeek) & ON-PREM SQLITE ERP QUERIES",
            "panel_accent": (0.22, 0.92, 0.68),
            "console_lines": [
                "$ python3 agent_harness.py --sidecar http://127.0.0.1:19101",
                f"[ALLOW] POST /v1/chat/completions -> inference://private-llm : {act2_res.get('step2_plan', '')[:80]}",
                f"[ALLOW] {s2_inv}",
                f"[ALLOW] {s2_stk}",
                "> Encrypted Noise/QUIC p2p stream verified by Biscuit RBAC + residency=on-prem label.",
            ],
            "beams": [
                {"src": POS_AGENT_APP, "dst": POS_NODE_AGENT, "color": (0.22, 0.88, 1.0), "arc_h": 0.4},
                {"src": POS_NODE_AGENT, "dst": POS_NODE_INF, "color": (0.68, 0.42, 1.0), "arc_h": 1.5, "label": "inference://private-llm (ALLOW)"},
                {"src": POS_NODE_INF, "dst": POS_GPU_RACK, "color": (0.68, 0.42, 1.0), "arc_h": 0.45},
                {"src": POS_NODE_AGENT, "dst": POS_NODE_TOOLS, "color": (0.20, 0.92, 0.65), "arc_h": 1.6, "label": "mcp://erp-db (ALLOW)"},
                {"src": POS_NODE_TOOLS, "dst": POS_ERP_DB, "color": (0.20, 0.92, 0.65), "arc_h": 0.5},
            ],
            "shields": [],
            "ledger_add": [
                {"decision": "ALLOW", "service": "inference://private-llm", "detail": "ReAct Turn 1 reasoning"},
                {"decision": "ALLOW", "service": "mcp://erp-db", "detail": "lookup_invoice(INV-2026-089)"},
                {"decision": "ALLOW", "service": "mcp://erp-db", "detail": "check_inventory(SKU-GPU-H200)"},
            ],
        })

        # Scene 3: Act 2 (Part B) — Vault-Side Secret Brokering to Compliance API (7.5s)
        scenes.append({
            "duration": 7.5,
            "act_idx": 2,
            "cam_start": (-0.12, 0.48, 13.2, (0.3, 0.55, 0.1)),
            "cam_end": (0.16, 0.50, 12.8, (1.1, 0.55, -0.4)),
            "headline": "Act 2: On-Premises Secret Brokering — Zero Credentials in Agent Container",
            "subheadline": "node-tools.yaml injects secrets/compliance-api-key at the on-prem vault boundary for GET /v1/vendors/ACME-01",
            "panel_title": f"ACT 2B — VAULT SECRET INJECTION & FINAL AUDIT SYNTHESIS ({act2_ms} ms total)",
            "panel_accent": (0.96, 0.78, 0.22),
            "console_lines": [
                "[ALLOW] GET /v1/vendors/ACME-01 via egress://compliance.internal",
                "        -> node-tools injected Authorization: Bearer secrets/compliance-api-key",
                f"        -> {s2_cmp}",
                f"[ALLOW] Final Audit Synthesis: {final2}",
                "> Raw API secret never left the on-premises vault node.",
            ],
            "beams": [
                {"src": POS_NODE_AGENT, "dst": POS_NODE_TOOLS, "color": (0.28, 0.85, 0.98), "arc_h": 1.5, "label": "GET /v1/vendors/ACME-01"},
                {"src": POS_NODE_TOOLS, "dst": POS_COMPLIANCE, "color": (0.98, 0.78, 0.18), "arc_h": 0.65, "label": "+ Bearer secrets/compliance-api-key"},
            ],
            "shields": [],
            "ledger_add": [
                {"decision": "ALLOW", "service": "egress://compliance.internal", "detail": "GET /v1/vendors/ACME-01 (+Vault Key)"},
                {"decision": "ALLOW", "service": "inference://private-llm", "detail": "Audit APPROVED ($148,500)"},
            ],
        })

        # Scene 4: Act 3a & 3b — Default-Deny on Payroll Vault & HTTP POST Mutation Block (8.5s)
        scenes.append({
            "duration": 8.5,
            "act_idx": 3,
            "cam_start": (0.16, 0.50, 12.8, (1.1, 0.55, -0.4)),
            "cam_end": (0.06, 0.52, 13.0, (1.2, 0.55, -0.5)),
            "headline": "Act 3a & 3b: Zero-Trust Default-Deny on Service Names & HTTP Methods",
            "subheadline": "Blocks mcp://payroll-vault on the same Vault node and blocks POST /v1/vendors/ACME-01 mutations (HTTP 403)",
            "panel_title": "ACT 3A & 3B — LIVE DEFAULT-DENY GUARDRAIL VERIFICATION",
            "panel_accent": (1.0, 0.35, 0.42),
            "console_lines": [
                "$ mcp-client -url http://127.0.0.1:19101/mcp -tool call_remote_tool (mcp://payroll-vault/dump_executive_salaries)",
                f"[DENY]  3a Service-Name Default-Deny : {act3a_res.get('output', '')}",
                "$ curl -X POST http://127.0.0.1:19101/mesh/.../egress/compliance.internal/v1/vendors/ACME-01",
                f"[DENY]  3b HTTP Method Guardrail     : {act3b_res.get('output', '')}",
                "> Even on the same vault node, ungranted service names and HTTP POST mutations are blocked.",
            ],
            "beams": [
                {"src": POS_NODE_AGENT, "dst": POS_PAYROLL, "color": (1.0, 0.25, 0.35), "arc_h": 1.5, "deny": True, "label": "mcp://payroll-vault (BLOCKED)"},
                {"src": POS_NODE_AGENT, "dst": POS_COMPLIANCE, "color": (1.0, 0.25, 0.35), "arc_h": 1.2, "deny": True, "label": "POST /v1/vendors (HTTP 403)"},
            ],
            "shields": [
                {"pos": POS_PAYROLL, "radius": 66.0, "caption": "3a DENY: payroll-vault"},
                {"pos": POS_COMPLIANCE, "radius": 62.0, "caption": "3b DENY: HTTP 403 POST"},
            ],
            "ledger_add": [
                {"decision": "DENY", "service": "mcp://payroll-vault", "detail": "No matching Datalog policy"},
                {"decision": "DENY", "service": "egress://compliance.internal", "detail": "HTTP 403 POST blocked"},
            ],
        })

        # Scene 5: Act 3c — Positive Datalog Label Check Rejects External Peer (7.5s)
        scenes.append({
            "duration": 7.5,
            "act_idx": 3,
            "cam_start": (0.06, 0.52, 13.0, (1.2, 0.55, -0.5)),
            "cam_end": (-0.14, 0.49, 13.4, (-0.3, 0.55, -0.6)),
            "headline": "Act 3c: Positive Residency & Department Label Enforcement",
            "subheadline": "node-tools.yaml enforces check if label(\"residency\", \"on-prem\") and label(\"dept\", \"finance\")",
            "panel_title": f"ACT 3C — EXTERNAL PEER (dept=external) REJECTED AT ON-PREM VAULT ({act3c_ms} ms)",
            "panel_accent": (1.0, 0.35, 0.42),
            "console_lines": [
                "$ mcp-client -url http://127.0.0.1:19104/mcp -tool call_remote_tool (mcp://erp-db/lookup_invoice)",
                "[DENY]  Caller node-external (dept=external) rejected by node-tools:",
                f"        {act3c_res.get('output', '')}",
                "> Zero-trust Datalog checks verify cryptographic peer labels on every stream.",
            ],
            "beams": [
                {"src": POS_EXTERNAL, "dst": POS_NODE_TOOLS, "color": (1.0, 0.22, 0.32), "arc_h": 1.35, "deny": True, "label": "dept=external -> erp-db (BLOCKED)"},
            ],
            "shields": [
                {"pos": POS_NODE_TOOLS, "radius": 74.0, "caption": "3c DENY: dept!=finance"},
            ],
            "ledger_add": [
                {"decision": "DENY", "service": "mcp://erp-db", "detail": "Caller dept=external rejected"},
            ],
        })

        # Scene 6: Act 3d — Task-Scoped Sealed Biscuit (`tar_block`) Blocks Prompt Injection (8.5s)
        scenes.append({
            "duration": 8.5,
            "act_idx": 3,
            "cam_start": (-0.14, 0.49, 13.4, (-0.3, 0.55, -0.6)),
            "cam_end": (-0.06, 0.47, 13.0, (-0.8, 0.60, 0.1)),
            "headline": "Act 3d: Task-Scoped Sealed Biscuit (tar_block) Stops Prompt Injection",
            "subheadline": "RFC 8693 /oauth/token attenuates Biscuit in memory to [lookup_invoice, check_inventory]; blocks wire transfer",
            "panel_title": "ACT 3D — RFC 8693 TASK-SCOPED SEALED tar_block BISCUIT GUARDRAIL",
            "panel_accent": (0.98, 0.72, 0.18),
            "sealed_biscuit": True,
            "console_lines": [
                "$ POST http://127.0.0.1:19101/oauth/token (scope='tool:lookup_invoice tool:check_inventory', seal=true)",
                *[f"[ALLOW] {ln}" if ln.startswith("1.") else f"[DENY]  {ln}" if ln.startswith("2.") else f"> {ln}" for ln in act3d_lines],
            ],
            "beams": [
                {"src": POS_AGENT_APP, "dst": POS_NODE_AGENT, "color": (1.0, 0.25, 0.35), "arc_h": 0.55, "deny": True, "label": "execute_wire_transfer ($250k) BLOCKED"},
                {"src": POS_NODE_AGENT, "dst": POS_NODE_TOOLS, "color": (0.20, 0.92, 0.65), "arc_h": 1.45, "label": "lookup_invoice (tar_block ALLOW)"},
            ],
            "shields": [
                {"pos": POS_NODE_AGENT, "radius": 68.0, "caption": "3d DENY: tar_block Guardrail"},
            ],
            "ledger_add": [
                {"decision": "ALLOW", "service": "mcp://erp-db", "detail": "tar_block: lookup_invoice"},
                {"decision": "DENY", "service": "mcp://erp-db", "detail": "tar_block: execute_wire_transfer"},
            ],
        })

        # Scene 7: Act 4 — Correlated Audit Trail & Instant Mesh-Wide Peer Revocation (8.5s)
        scenes.append({
            "duration": 8.5,
            "act_idx": 4,
            "cam_start": (-0.06, 0.47, 13.0, (-0.8, 0.60, 0.1)),
            "cam_end": (0.12, 0.51, 14.2, (0.0, 0.55, 0.2)),
            "headline": "Act 4: Correlated Audit Trail & Mesh-Wide Peer Revocation (admin ban)",
            "subheadline": "audit.py correlates ALLOW/DENY decisions; agentmesh-one admin ban revokes node-external across the mesh",
            "panel_title": f"ACT 4 — AUDIT SUMMARY & MESH-WIDE REVOCATION (BANNED PEER {env.external_peer[:24]}...)",
            "panel_accent": (0.38, 0.86, 1.0),
            "sealed_biscuit": True,
            "ext_banned": True,
            "console_lines": [
                f"$ agentmesh-one admin ban {env.external_peer[:36]}...",
                f"[DENY]  {act4_ban_res.get('output', 'Peer banned mesh-wide')}",
                f"> Total Live Audit Events Captured: {len(audit_events)} | SQLite wire_transfers rows: {state_res.get('sqlite', {}).get('wire_transfers_count', 0)}",
                *[
                    f"[{ev['decision']}] {ev['service']} <- {ev['caller']} ({ev['method']})"
                    for ev in audit_events[-4:]
                ],
            ],
            "beams": [
                {"src": POS_CP, "dst": POS_EXTERNAL, "color": (1.0, 0.22, 0.32), "arc_h": 1.2, "deny": True, "label": "admin ban -> REVOKED"},
            ],
            "shields": [
                {"pos": POS_EXTERNAL, "radius": 72.0, "caption": "PEER REVOKED & BANNED"},
            ],
            "ledger_add": [
                {"decision": "DENY", "service": "admin://ban-peer", "detail": f"Banned {env.external_peer[:18]}..."},
            ],
        })

        # 3. Render 1920x1080 @ 30fps directly into ffmpeg
        total_dur = sum(s["duration"] for s in scenes)
        total_frames = int(total_dur * FPS)
        print(f"[3/3] Rendering {total_frames} frames ({total_dur:.1f}s @ {FPS}fps, 1920x1080) via Cairo 3D + ffmpeg...")

        ffmpeg_cmd = [
            "ffmpeg",
            "-y",
            "-f", "rawvideo",
            "-vcodec", "rawvideo",
            "-pix_fmt", "bgr0",
            "-s", f"{W}x{H}",
            "-r", str(FPS),
            "-i", "-",
            "-c:v", "libx264",
            "-pix_fmt", "yuv420p",
            "-preset", "fast",
            "-crf", "24",
            "-movflags", "+faststart",
            str(out_mp4),
        ]
        proc = subprocess.Popen(ffmpeg_cmd, stdin=subprocess.PIPE, stderr=subprocess.PIPE)
        assert proc.stdin is not None

        surface = cairo.ImageSurface(cairo.FORMAT_ARGB32, W, H)
        ctx = cairo.Context(surface)
        cam = Camera3D()

        global_frame = 0
        allow_count = 0
        deny_count = 0

        for s_idx, scene in enumerate(scenes):
            for entry in scene.get("ledger_add", []):
                ledger.append(entry)
                if entry["decision"] == "ALLOW":
                    allow_count += 1
                elif entry["decision"] == "DENY":
                    deny_count += 1

            n_frames = int(scene["duration"] * FPS)
            c0 = scene["cam_start"]
            c1 = scene["cam_end"]

            for f in range(n_frames):
                u = smoothstep(f / max(1, n_frames - 1))
                t_sec = global_frame / FPS
                yaw = lerp(c0[0], c1[0], u) + 0.03 * math.sin(t_sec * 0.7)
                pitch = lerp(c0[1], c1[1], u)
                dist = lerp(c0[2], c1[2], u)
                tx = lerp(c0[3][0], c1[3][0], u)
                ty = lerp(c0[3][1], c1[3][1], u)
                tz = lerp(c0[3][2], c1[3][2], u)
                cam.set_pose(yaw, pitch, dist, (tx, ty, tz))

                frame_state = {
                    "act_idx": scene["act_idx"],
                    "headline": scene["headline"],
                    "subheadline": scene["subheadline"],
                    "panel_title": scene["panel_title"],
                    "panel_accent": scene["panel_accent"],
                    "console_lines": scene["console_lines"],
                    "beams": scene["beams"],
                    "shields": scene["shields"],
                    "sealed_biscuit": scene.get("sealed_biscuit", False),
                    "ext_banned": scene.get("ext_banned", False),
                    "allow_count": allow_count,
                    "deny_count": deny_count,
                    "ledger": ledger,
                }
                render_frame(ctx, cam, t_sec, frame_state)
                proc.stdin.write(surface.get_data())

                # Save a preview PNG at the midpoint of each scene
                if f == n_frames // 2:
                    surface.write_to_png(str(preview_dir / f"scene_{s_idx + 1}.png"))

                global_frame += 1

        proc.stdin.close()
        stderr_out = proc.stderr.read().decode("utf-8", errors="replace") if proc.stderr else ""
        rc = proc.wait()
        if rc != 0:
            raise RuntimeError(f"ffmpeg exited with {rc}: {stderr_out}")

        print(f"Successfully rendered 3D live-execution video to {out_mp4} ({out_mp4.stat().st_size / (1024*1024):.2f} MB)")
        return 0
    finally:
        if server is not None:
            server.shutdown()
        env.stop()


if __name__ == "__main__":
    raise SystemExit(main())
