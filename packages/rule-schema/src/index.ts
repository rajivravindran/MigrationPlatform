import { z } from "zod";

export const SourceSchema = z.object({
  type: z.enum(["csv", "json", "xml", "salesforce", "watched_prefix"]),
  schema: z
    .array(
      z.object({
        name: z.string(),
        type: z.enum(["string", "integer", "number", "boolean", "datetime", "object"])
      })
    )
    .optional(),
  options: z.record(z.any()).optional(),
  connectorId: z.number().int().optional()
});

export const PreprocessStepSchema = z.object({
  id: z.string(),
  field: z.string(),
  fn: z.string(),
  args: z.record(z.any()).optional(),
  code: z.string().optional()
});

export const ValueExprSchema = z.union([
  z.string(),
  z.object({ $from: z.string().min(1) }).strict(),
  z.object({ $literal: z.string() }).strict(),
  z
    .object({
      $fromResponse: z.string().min(1),
      path: z.string().min(1).regex(/^\$/, "JSONPath must start with $")
    })
    .strict()
]);
export type ValueExpr = z.infer<typeof ValueExprSchema>;

export const QueryParamValueSchema = z.union([ValueExprSchema, z.array(ValueExprSchema)]);

export const DestinationSchema = z.object({
  type: z.literal("http"),
  method: z.enum(["GET", "POST", "PUT", "PATCH", "DELETE"]),
  url: z.string().min(1),
  pathParams: z.record(ValueExprSchema).optional(),
  queryParams: z.record(QueryParamValueSchema).optional(),
  headers: z.record(ValueExprSchema).optional(),
  auth: z
    .object({
      type: z.enum(["bearer", "basic", "api_key", "oauth2_client"]),
      secretRef: z.string().optional(),
      headerName: z.string().optional()
    })
    .optional(),
  idempotencyKey: z.any().optional(),
  idempotencyHeader: z.string().optional()
});

export const MappingSchema = z.object({ payload: z.record(z.any()) });

export const OnFailureSchema = z.enum(["stop", "continue"]);
export type OnFailure = z.infer<typeof OnFailureSchema>;

export const StepSchema = z.object({
  name: z.string().regex(/^[A-Za-z_][A-Za-z0-9_]*$/),
  description: z.string().optional(),
  mapping: MappingSchema.optional(),
  destination: DestinationSchema,
  /** After this step definitively fails: stop the chain (default) or continue. */
  onFailure: OnFailureSchema.optional()
});
export type Step = z.infer<typeof StepSchema>;

export const RetrySchema = z.object({
  maxAttempts: z.number().int().min(1).max(20).optional(),
  backoff: z.enum(["exponential", "fixed"]).optional(),
  initialIntervalMs: z.number().int().min(10).optional()
});

export const ExportColumnSchema = z.object({
  name: z.string().min(1),
  $from: z.string().min(1).optional(),
  $fromResponse: z.string().min(1).optional(),
  path: z.string().min(1).optional()
});

export const ExportSchema = z.object({
  columns: z.array(ExportColumnSchema).max(64).optional()
});

const RuleTemplateBase = z.object({
  id: z.string().min(1),
  version: z.number().int().min(1),
  name: z.string().min(1),
  source: SourceSchema,
  preprocess: z.array(PreprocessStepSchema),
  retry: RetrySchema.optional(),
  concurrency: z.number().int().min(1).max(512).optional(),
  export: ExportSchema.optional()
});

/** Single-destination (legacy / one-step) shape. */
export const SingleRuleTemplateSchema = RuleTemplateBase.extend({
  mapping: MappingSchema,
  destination: DestinationSchema
});

/** Multi-step chain shape. */
export const ChainRuleTemplateSchema = RuleTemplateBase.extend({
  steps: z.array(StepSchema).min(1).max(10)
});

// Chain shape is tried first so that a template carrying `steps` is always
// interpreted as a chain (the authoritative JSON Schema rejects templates
// with both shapes present; Zod is the advisory client-side mirror).
export const RuleTemplateSchema = z.union([ChainRuleTemplateSchema, SingleRuleTemplateSchema]);

export type RuleTemplate = z.infer<typeof RuleTemplateSchema>;

/** Normalize either shape into a step list for execution/preview. */
export function templateSteps(tpl: RuleTemplate): Step[] {
  if ("steps" in tpl && tpl.steps) return tpl.steps;
  const single = tpl as z.infer<typeof SingleRuleTemplateSchema>;
  return [{ name: "main", mapping: single.mapping, destination: single.destination }];
}
