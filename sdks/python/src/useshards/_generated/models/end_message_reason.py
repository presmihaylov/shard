from enum import StrEnum


class EndMessageReason(StrEnum):
    ENDED = "ended"
    REMOVED = "removed"
    STOPPED = "stopped"

    def __str__(self) -> str:
        return str(self.value)
