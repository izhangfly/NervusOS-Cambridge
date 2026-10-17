"""Fast unit tests for the ACT pipeline's pure logic (no LeRobot, no hardware).

    python3 -m pytest act/tests/test_units.py      or      python3 act/tests/test_units.py
"""
import sys
from pathlib import Path

ACT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ACT / "mac"))
sys.path.insert(0, str(ACT / "engine"))


def published_steps(steps, save_freq):
    """The checkpoints the supervisor would publish under the worker's plan (its own rule, restated)."""
    import worker
    every, first = worker.Worker.publish_plan(steps, save_freq)
    ckpts = sorted(set(list(range(save_freq, steps + 1, save_freq)) + [steps]))
    return [s for s in ckpts if s >= first and (s % every == 0 or s == steps)]


def test_publish_plan_always_publishes_the_final_checkpoint():
    for steps, save in [(100, 500), (500, 500), (7000, 5000), (20000, 5000), (60000, 5000), (100000, 5000), (100000, 10000)]:
        p = published_steps(steps, save)
        assert p and p[-1] == steps, (steps, save, p)


def test_publish_plan_publishes_about_four_from_the_second_half():
    for steps, save in [(20000, 5000), (60000, 5000), (100000, 5000), (100000, 10000)]:
        p = published_steps(steps, save)
        assert 2 <= len(p) <= 5 and p[0] >= steps * 0.3, (steps, save, p)


if __name__ == "__main__":
    import inspect
    fails = 0
    for name, fn in sorted(inspect.getmembers(sys.modules[__name__], inspect.isfunction)):
        if name.startswith("test_"):
            try:
                fn()
                print(f"PASS {name}")
            except AssertionError as e:
                fails += 1
                print(f"FAIL {name}: {e}")
    sys.exit(1 if fails else 0)
