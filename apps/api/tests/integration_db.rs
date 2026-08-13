//! DB-layer integration tests. Spins up Postgres via testcontainers,
//! runs all sqlx migrations, and exercises the core enums/tables.
//!
//! These tests gate any schema regression in `apps/api/migrations/0001_init.sql`
//! (including the hash-partitioned `job_rows` and the `schedules` tables).

use sqlx::postgres::PgPoolOptions;
use sqlx::{PgPool, Row};
use testcontainers::runners::AsyncRunner;
use testcontainers_modules::postgres::Postgres as PgImage;

async fn setup() -> (testcontainers::ContainerAsync<PgImage>, PgPool) {
    let container = PgImage::default().start().await.expect("start postgres");
    let port = container.get_host_port_ipv4(5432).await.unwrap();
    let url = format!("postgres://postgres:postgres@127.0.0.1:{port}/postgres");
    let pool = PgPoolOptions::new()
        .max_connections(4)
        .connect(&url)
        .await
        .expect("connect pg");
    sqlx::migrate!("./migrations")
        .run(&pool)
        .await
        .expect("migrate");
    (container, pool)
}

#[tokio::test]
#[ignore] // requires docker; run via `cargo test --test integration_db -- --ignored`
async fn migrations_apply_and_seed_roundtrip() {
    let (_c, pool) = setup().await;

    let org_id: i64 =
        sqlx::query_scalar("INSERT INTO organizations (name) VALUES ($1) RETURNING id")
            .bind("Acme")
            .fetch_one(&pool)
            .await
            .unwrap();
    assert!(org_id > 0);

    let user_id: i64 = sqlx::query_scalar(
        "INSERT INTO users (org_id, email, role, password_hash) \
         VALUES ($1, $2, $3::user_role, $4) RETURNING id",
    )
    .bind(org_id)
    .bind("admin@acme.com")
    .bind("admin")
    .bind("$argon2id$v=19$m=4096,t=3,p=1$c29tZXNhbHQ$abc")
    .fetch_one(&pool)
    .await
    .unwrap();
    assert!(user_id > 0);

    let tpl_id: i64 = sqlx::query_scalar(
        "INSERT INTO rule_templates (org_id, template_key, version, name, schema_json, published, created_by) \
         VALUES ($1, $2, 1, $3, $4::jsonb, false, $5) RETURNING id",
    )
    .bind(org_id)
    .bind("rt_demo")
    .bind("Demo")
    .bind(r#"{"id":"rt_demo","version":1,"name":"Demo","source":{"type":"csv","schema":[]},"mapping":{"payload":{}},"destination":{"type":"http","method":"POST","url":"http://x"}}"#)
    .bind(user_id)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert!(tpl_id > 0);
}

#[tokio::test]
#[ignore]
async fn job_rows_partitioning_is_hash_modulo_32() {
    let (_c, pool) = setup().await;
    let count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_inherits i \
         JOIN pg_class p ON p.oid = i.inhparent \
         WHERE p.relname = 'job_rows'",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(count, 32, "job_rows should have 32 hash partitions");
}

#[tokio::test]
#[ignore]
async fn schedule_overlap_enum_accepts_policies() {
    let (_c, pool) = setup().await;
    for policy in [
        "skip",
        "buffer_one",
        "buffer_all",
        "cancel_other",
        "allow_all",
    ] {
        let row = sqlx::query("SELECT $1::schedule_overlap::text AS p")
            .bind(policy)
            .fetch_one(&pool)
            .await
            .unwrap();
        let got: String = row.get("p");
        assert_eq!(got, policy);
    }
}

#[tokio::test]
#[ignore]
async fn user_role_enum_rejects_unknown_role() {
    let (_c, pool) = setup().await;
    let org: i64 = sqlx::query_scalar("INSERT INTO organizations (name) VALUES ('O') RETURNING id")
        .fetch_one(&pool)
        .await
        .unwrap();
    let res = sqlx::query(
        "INSERT INTO users (org_id, email, role, password_hash) \
         VALUES ($1, 'x@y.z', $2::user_role, 'h')",
    )
    .bind(org)
    .bind("superuser")
    .execute(&pool)
    .await;
    assert!(res.is_err(), "superuser is not in user_role enum");
}
