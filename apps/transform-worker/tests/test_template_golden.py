import json
from pathlib import Path

from migration_worker.template import RuleTemplate

GOLDEN = Path(__file__).parents[3] / "packages" / "rule-schema" / "fixtures" / "golden.json"


def test_golden_parses():
    data = json.loads(GOLDEN.read_text())
    template = RuleTemplate.model_validate(data)
    assert template.id == "rt_golden"
    assert template.source.type == "csv"
    assert len(template.preprocess) == 2


def test_golden_roundtrip():
    data = json.loads(GOLDEN.read_text())
    template = RuleTemplate.model_validate(data)
    again = RuleTemplate.model_validate_json(template.model_dump_json(by_alias=True))
    assert again == template
