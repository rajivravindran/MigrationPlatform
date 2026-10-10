import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { DestinationSchema } from "../src/index.js";

const jsonSchema = JSON.parse(
  readFileSync(resolve(__dirname, "../schema/rule-template.schema.json"), "utf-8")
);

const destination = {
  type: "http",
  method: "POST",
  url: "https://api.example.com/v1/customers"
};

test("delivery modes parse with their required parts", () => {
  for (const delivery of [
    { mode: "manual" },
    { mode: "idempotent", idempotencyRetentionHours: 24 },
    { mode: "lookup", lookup: { url: "https://api.example.com/v1/customers", foundPath: "$.data[0].id" } }
  ]) {
    expect(DestinationSchema.parse({ ...destination, delivery }).delivery).toEqual(delivery);
  }
});

test("delivery rejects unknown modes, fields and out-of-range retention", () => {
  for (const delivery of [
    { mode: "exactly_once" },
    { mode: "manual", retries: 3 },
    { mode: "idempotent", idempotencyRetentionHours: 0 },
    { mode: "idempotent", idempotencyRetentionHours: 8761 },
    { mode: "lookup", lookup: { url: "u", foundPath: "data.id" } },
    { mode: "lookup", lookup: { method: "POST", url: "u", foundPath: "$.id" } }
  ]) {
    expect(() => DestinationSchema.parse({ ...destination, delivery })).toThrow();
  }
});

test("auth secretRef is a secret name, never a value", () => {
  expect(DestinationSchema.parse({ ...destination, auth: { type: "bearer", secretRef: "crm.token_v2" } }).auth?.secretRef).toBe(
    "crm.token_v2"
  );
  for (const secretRef of ["", "has space", "Bearer abc/def", "x".repeat(129)]) {
    expect(() => DestinationSchema.parse({ ...destination, auth: { type: "bearer", secretRef } })).toThrow();
  }
});

test("zod and the JSON Schema agree on delivery and auth enums", () => {
  const defs = jsonSchema.$defs;
  expect(defs.delivery.properties.mode.enum).toEqual(["idempotent", "lookup", "manual"]);
  expect(DestinationSchema.shape.delivery.unwrap().shape.mode.options).toEqual(defs.delivery.properties.mode.enum);
  const auth = DestinationSchema.shape.auth.unwrap().shape;
  const jsonAuth = defs.destination.properties.auth.properties;
  expect(auth.type.options).toEqual(jsonAuth.type.enum);
  expect(new RegExp(jsonAuth.secretRef.pattern).test("crm.token_v2")).toBe(true);
});
