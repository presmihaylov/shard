from enum import StrEnum


class SandboxState(StrEnum):
    CREATED = "created"
    FAILED = "failed"
    PAUSED = "paused"
    PENDING = "pending"
    RUNNING = "running"
    STOPPED = "stopped"
    UNRESPONSIVE = "unresponsive"

    def __str__(self) -> str:
        return str(self.value)
