import os
from collections.abc import Callable, Iterator

import pytest

from tests.helpers import child_pids


@pytest.fixture
def no_leaked_children() -> Iterator[Callable[[], list[int]]]:
    """Fails the test if it starts or finishes with live child processes."""
    if not os.path.isdir("/proc"):
        pytest.skip("process inspection requires /proc")
    assert child_pids() == [], "test started with live child processes"
    yield child_pids
    assert child_pids() == [], "test leaked child processes"
