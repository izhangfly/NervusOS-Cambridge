"""Measure ACT train/inference throughput on this machine (MPS vs CPU).

Single-arm Piper shape: 6 joints + 1 gripper = 7 DoF, two camera views.
"""
import time

import torch
from lerobot.policies.act.configuration_act import ACTConfig
from lerobot.policies.act.modeling_act import ACTPolicy
from lerobot.configs.types import FeatureType, PolicyFeature

DOF = 7
CHUNK = 100
BATCH = 8
H, W = 480, 640


def build(device: str) -> ACTPolicy:
    cfg = ACTConfig(
        chunk_size=CHUNK,
        n_action_steps=CHUNK,
        input_features={
            "observation.state": PolicyFeature(type=FeatureType.STATE, shape=(DOF,)),
            "observation.images.top": PolicyFeature(type=FeatureType.VISUAL, shape=(3, H, W)),
            "observation.images.wrist": PolicyFeature(type=FeatureType.VISUAL, shape=(3, H, W)),
        },
        output_features={
            "action": PolicyFeature(type=FeatureType.ACTION, shape=(DOF,)),
        },
        device=device,
    )
    policy = ACTPolicy(cfg)
    policy.to(device)
    return policy


def batch(device: str) -> dict:
    return {
        "observation.state": torch.randn(BATCH, DOF, device=device),
        "observation.images.top": torch.rand(BATCH, 3, H, W, device=device),
        "observation.images.wrist": torch.rand(BATCH, 3, H, W, device=device),
        "action": torch.randn(BATCH, CHUNK, DOF, device=device),
        "action_is_pad": torch.zeros(BATCH, CHUNK, dtype=torch.bool, device=device),
    }


def sync(device: str) -> None:
    if device == "mps":
        torch.mps.synchronize()


def run(device: str, steps: int = 12) -> None:
    policy = build(device)
    n_params = sum(p.numel() for p in policy.parameters())
    print(f"\n=== {device.upper()} === ({n_params/1e6:.1f}M params)")

    opt = torch.optim.AdamW(policy.parameters(), lr=1e-5)
    b = batch(device)

    for i in range(steps):
        if i == 2:  # warmup done
            sync(device)
            t0 = time.perf_counter()
        loss, _ = policy.forward(b)
        loss.backward()
        opt.step()
        opt.zero_grad()
    sync(device)
    dt = (time.perf_counter() - t0) / (steps - 2)
    print(f"train: {dt*1000:.0f} ms/step (batch={BATCH})  ->  {1/dt:.2f} steps/s")
    print(f"       20k steps would take {20000*dt/3600:.1f} hours")

    policy.eval()
    policy.reset()
    obs = {k: v[:1] for k, v in b.items() if k.startswith("observation")}
    with torch.no_grad():
        for i in range(12):
            if i == 2:
                sync(device)
                t0 = time.perf_counter()
            policy.select_action(obs)
    sync(device)
    dt = (time.perf_counter() - t0) / 10
    print(f"inference: {dt*1000:.1f} ms/action")


if __name__ == "__main__":
    run("mps")
