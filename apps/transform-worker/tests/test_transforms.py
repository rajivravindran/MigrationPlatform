from migration_worker.template import PreprocessStep, RuleTemplate
from migration_worker.transforms import build_payload, preprocess_row


def test_preprocess_builtin_chain():
    steps = [
        PreprocessStep(id="1", field="email", fn="builtin.lowercase"),
        PreprocessStep(id="2", field="email", fn="builtin.trim", args={}),
    ]
    assert preprocess_row({"email": "  Foo@EXAMPLE.com "}, steps)["email"] == "foo@example.com"


def test_build_payload_simple_mapping():
    template = {
        "mapping": {
            "payload": {
                "email": {"$from": "email"},
                "country_code": {"$py": "row['country'][:2].upper()"},
            }
        }
    }
    out = build_payload({"email": "a@b", "country": "usa"}, template)
    assert out["email"] == "a@b"
    assert out["country_code"] == "US"


def test_rule_template_apply_end_to_end():
    tpl = RuleTemplate.model_validate(
        {
            "id": "rt",
            "version": 1,
            "name": "t",
            "source": {"type": "csv"},
            "preprocess": [{"id": "1", "field": "email", "fn": "builtin.lowercase"}],
            "mapping": {"payload": {"email": {"$from": "email"}}},
            "destination": {
                "type": "http",
                "method": "POST",
                "url": "https://example.com/x",
            },
        }
    )
    row = {"email": "A@B"}
    pre = preprocess_row(row, tpl.preprocess)
    payload = build_payload(pre, tpl.model_dump(by_alias=True))
    assert payload == {"email": "a@b"}
