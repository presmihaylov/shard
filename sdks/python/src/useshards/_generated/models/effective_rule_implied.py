from enum import StrEnum


class EffectiveRuleImplied(StrEnum):
    DNS = "dns"
    DNS_RULE = "dns-rule"

    def __str__(self) -> str:
        return str(self.value)
