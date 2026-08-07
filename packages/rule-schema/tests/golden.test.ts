import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { RuleTemplateSchema, templateSteps } from "../src/index.js";

const golden = JSON.parse(
  readFileSync(resolve(__dirname, "../fixtures/golden.json"), "utf-8")
);
const goldenUrlTpl = JSON.parse(
  readFileSync(resolve(__dirname, "../fixtures/golden_url_templating.json"), "utf-8")
);
const goldenSteps = JSON.parse(
  readFileSync(resolve(__dirname, "../fixtures/golden_steps.json"), "utf-8")
);

test("golden fixture parses cleanly", () => {
  const parsed = RuleTemplateSchema.parse(golden);
  expect(parsed.id).toBe("rt_golden");
  expect(parsed.source.type).toBe("csv");
  expect(parsed.preprocess).toHaveLength(2);
});

test("round-trips JSON losslessly", () => {
  const parsed = RuleTemplateSchema.parse(golden);
  const again = RuleTemplateSchema.parse(JSON.parse(JSON.stringify(parsed)));
  expect(again).toEqual(parsed);
});

test("url-templating fixture parses + round-trips", () => {
  const parsed = RuleTemplateSchema.parse(goldenUrlTpl);
  expect(parsed.destination.pathParams?.userId).toEqual({ $from: "id" });
  expect(parsed.destination.queryParams?.tags).toHaveLength(3);
  const again = RuleTemplateSchema.parse(JSON.parse(JSON.stringify(parsed)));
  expect(again).toEqual(parsed);
});

test("rejects pathParam with unknown sigil", () => {
  expect(() =>
    RuleTemplateSchema.parse({
      ...goldenUrlTpl,
      destination: {
        ...goldenUrlTpl.destination,
        pathParams: { userId: { $weird: "id" } }
      }
    })
  ).toThrow();
});

test("multi-step fixture parses + round-trips", () => {
  const parsed = RuleTemplateSchema.parse(goldenSteps);
  const steps = templateSteps(parsed);
  expect(steps).toHaveLength(2);
  expect(steps[0].name).toBe("createContact");
  expect(steps[1].destination.pathParams?.contactId).toEqual({
    $fromResponse: "createContact",
    path: "$.data.id"
  });
  const again = RuleTemplateSchema.parse(JSON.parse(JSON.stringify(parsed)));
  expect(again).toEqual(parsed);
});

test("single-destination template normalizes to one step", () => {
  const steps = templateSteps(RuleTemplateSchema.parse(golden));
  expect(steps).toHaveLength(1);
  expect(steps[0].name).toBe("main");
  expect(steps[0].destination.url).toBe("https://api.example.com/v1/customers");
});

test("rejects $fromResponse without JSONPath prefix", () => {
  const bad = JSON.parse(JSON.stringify(goldenSteps));
  bad.steps[1].destination.pathParams.contactId = {
    $fromResponse: "createContact",
    path: "data.id"
  };
  expect(() => RuleTemplateSchema.parse(bad)).toThrow();
});

test("steps take precedence when both shapes are present", () => {
  const both = { ...goldenSteps, destination: goldenUrlTpl.destination, mapping: { payload: {} } };
  const parsed = RuleTemplateSchema.parse(both);
  expect(templateSteps(parsed)[0].name).toBe("createContact");
});
