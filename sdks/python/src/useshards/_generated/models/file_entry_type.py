from enum import StrEnum


class FileEntryType(StrEnum):
    DIR = "dir"
    FILE = "file"
    OTHER = "other"
    SYMLINK = "symlink"

    def __str__(self) -> str:
        return str(self.value)
