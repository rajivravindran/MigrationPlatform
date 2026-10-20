# Migration delivery

MigrationPlatform prepares, executes, and verifies data migrations into destination applications. These terms describe the target product; they do not certify that every capability is implemented.

## Language

**Customer organization**: The organization using the platform to conduct migrations.

**Destination application**: The system receiving migrated records through its supported APIs.
_Avoid_: Customer (when referring to an API destination)

**Migration campaign**: A coordinated body of migration work with explicitly versioned dependencies and tracked execution history.

**Environment**: An isolated installation, such as Development, UAT, or Production, with its own credentials, destination bindings, and execution evidence.

**Migration package**: An uploaded ZIP or tar.gz archive containing source files and a manifest describing their processing requirements.

**Migration job**: A package-level execution in a campaign and environment, containing one or more file executions.
_Avoid_: Batch (in new user-facing language)

**File execution**: The processing of one manifest entry using an explicitly bound template release, with its own outcomes and recovery history.

**Template**: A reusable definition of source interpretation, transformations, mappings, and destination operations.

**Template release**: An immutable, identifiable version of a template that can be tested and promoted between environments.

**Job definition**: A reusable description of package processing and dependency requirements, separate from the state of any execution.

**Promotion**: Moving an approved immutable definition or template release to another environment; execution results and destination data do not move with it.

**Dependency requirement**: A prerequisite identified by campaign, environment, dependency key, and dependency version that must have qualifying completion evidence before dependent work can start.

**Execution intent**: A distinct request to perform migration work, separate from delivery attempts made to fulfill that request.

**Reconciliation**: Comparing expected migration results with observed destination results, preserving missing, mismatched, and uncertain outcomes.
