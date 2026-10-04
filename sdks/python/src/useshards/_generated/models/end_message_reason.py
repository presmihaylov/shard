from enum import StrEnum


class EndMessageReason(StrEnum):
    REMOVED = "removed"
    STOPPED = "stopped"

    def __str__(self) -> str:
        return str(self.value)
