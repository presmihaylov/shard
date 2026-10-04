from enum import StrEnum


class EventStatus(StrEnum):
    BUILDING = "building"
    CACHED = "cached"
    LAYER = "layer"
    PULLED = "pulled"
    PULLING = "pulling"
    UNPACKED = "unpacked"
    UNPACKING = "unpacking"

    def __str__(self) -> str:
        return str(self.value)
