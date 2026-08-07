FROM rust:1-slim-bookworm AS builder
WORKDIR /src

RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config libssl-dev ca-certificates git curl \
    && rm -rf /var/lib/apt/lists/*

COPY Cargo.toml Cargo.lock ./
COPY apps/api/Cargo.toml apps/api/Cargo.toml
RUN mkdir -p apps/api/src apps/api/src/bin \
    && echo "fn main() {}" > apps/api/src/main.rs \
    && echo "pub fn stub() {}" > apps/api/src/lib.rs \
    && echo "fn main() {}" > apps/api/src/bin/admin.rs \
    && cargo build --release --locked --manifest-path apps/api/Cargo.toml \
    && rm -rf apps/api/src

COPY apps/api ./apps/api
COPY packages/rule-schema ./packages/rule-schema
# License verification public key, embedded into the binary via include_str!.
COPY infra/license/license_public_key.pem ./infra/license/license_public_key.pem
# Invalidate the primer-stub artifacts so cargo actually rebuilds with the
# real sources (BuildKit COPY can preserve mtimes, defeating cargo's
# incremental fingerprint check).
RUN find target/release/.fingerprint -maxdepth 1 -name 'migration-api-*' -exec rm -rf {} + 2>/dev/null || true; \
    rm -f target/release/migration-api target/release/migration-admin \
          target/release/deps/migration_api-* \
          target/release/deps/migration_api.* \
          target/release/deps/migration_admin-* 2>/dev/null || true; \
    find apps/api/src -name '*.rs' -exec touch {} +; \
    cargo build --release --locked --manifest-path apps/api/Cargo.toml --bins

# ---------- Migration runner stage ----------
# Runs `migration-admin migrate` against $DATABASE_URL and exits.
FROM debian:bookworm-slim AS migrate
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates libssl3 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 app
USER app
WORKDIR /app
COPY --from=builder /src/target/release/migration-admin /app/migration-admin
COPY apps/api/migrations /app/migrations
ENV MIGRATIONS_DIR=/app/migrations
ENTRYPOINT ["/app/migration-admin", "migrate"]

# ---------- API runtime stage ----------
FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates libssl3 curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 app
USER app
WORKDIR /app
COPY --from=builder /src/target/release/migration-api /app/migration-api
COPY --from=builder /src/target/release/migration-admin /app/migration-admin
COPY apps/api/migrations /app/migrations
ENV MIGRATIONS_DIR=/app/migrations
EXPOSE 8080 9464
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/migration-api"]
