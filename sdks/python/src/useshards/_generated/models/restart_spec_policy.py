from enum import StrEnum


class RestartSpecPolicy(StrEnum):
    ALWAYS = "always"
    NO = "no"
    ON_FAILURE = "on-failure"
    UNLESS_STOPPED = "unless-stopped"

    def __str__(self) -> str:
        return str(self.value)
