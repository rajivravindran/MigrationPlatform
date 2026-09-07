//! Phone-home license service: email-gated trials + 24h heartbeats.
//!
//! Thin wrapper around `migration_api::license_server`. Signs with the same
//! RSA key as `migration-admin license-sign`. **Never** ship the private key
//! in customer images; mount it at runtime.
//!
//! Env:
//! - `LICENSE_SIGNING_KEY_PATH` — PKCS#8 PEM (required)
//! - `LICENSE_SERVER_TOKEN`     — shared bearer token, ≥32 chars (required)
//! - `LICENSE_SERVER_BIND`      — default `0.0.0.0:8090`
//! - `LICENSE_TRIAL_DB`         — SQLite path, default `./data/trials.db`
//! - `TRIAL_DAYS`               — default `10`
//!
//! Subcommands (operate on `LICENSE_TRIAL_DB`, no network):
//!   migration-license-server revoke   --fingerprint <hex> [--licensee <name>|*] [--reason <text>]
//!   migration-license-server unrevoke --fingerprint <hex> [--licensee <name>]

use std::net::SocketAddr;

use anyhow::{bail, Context, Result};
use tokio::net::TcpListener;
use tracing::info;

use migration_api::license_server::{self, AppState, ServerConfig, DEFAULT_TRIAL_DAYS};

const DEFAULT_BIND: &str = "0.0.0.0:8090";
const DEFAULT_DB: &str = "./data/trials.db";

#[tokio::main]
async fn main() -> Result<()> {
    dotenvy::dotenv().ok();
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    let db_path = std::env::var("LICENSE_TRIAL_DB").unwrap_or_else(|_| DEFAULT_DB.into());
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        Some("revoke") => return revoke_cmd(&db_path, &args[1..], true),
        Some("unrevoke") => return revoke_cmd(&db_path, &args[1..], false),
        Some("help") | Some("-h") | Some("--help") => {
            eprintln!(
                "migration-license-server [revoke|unrevoke --fingerprint <hex> [--licensee <name>] [--reason <text>]]"
            );
            return Ok(());
        }
        Some(other) => bail!("unknown subcommand {other}; run with --help"),
        None => {}
    }

    let key_path = std::env::var("LICENSE_SIGNING_KEY_PATH").context(
        "LICENSE_SIGNING_KEY_PATH required (path to PKCS#8 PEM; keep offline in production)",
    )?;
    let signing_key_pem =
        std::fs::read_to_string(&key_path).with_context(|| format!("reading {key_path}"))?;
    let activation_token =
        std::env::var("LICENSE_SERVER_TOKEN").context("LICENSE_SERVER_TOKEN is required")?;
    let trial_days = std::env::var("TRIAL_DAYS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(DEFAULT_TRIAL_DAYS);
    let bind = std::env::var("LICENSE_SERVER_BIND").unwrap_or_else(|_| DEFAULT_BIND.into());

    let cfg = ServerConfig {
        signing_key_pem,
        activation_token,
        trial_days,
    };
    let db = license_server::open_db(&db_path)?;
    let state = AppState::new(db, cfg)?;
    let app = license_server::build_router(state);

    let addr: SocketAddr = bind.parse().context("LICENSE_SERVER_BIND")?;
    let listener = TcpListener::bind(addr).await?;
    info!(%addr, db = %db_path, trial_days, "migration-license-server listening");
    axum::serve(listener, app).await?;
    Ok(())
}

fn revoke_cmd(db_path: &str, args: &[String], revoke: bool) -> Result<()> {
    let mut fingerprint = None;
    let mut licensee: Option<String> = None;
    let mut reason = "revoked by vendor".to_string();
    let mut i = 0;
    while i < args.len() {
        let value = args
            .get(i + 1)
            .with_context(|| format!("{} requires a value", args[i]))?;
        match args[i].as_str() {
            "--fingerprint" => fingerprint = Some(value.clone()),
            "--licensee" => licensee = Some(value.clone()),
            "--reason" => reason = value.clone(),
            other => bail!("unknown flag {other}"),
        }
        i += 2;
    }
    let fingerprint = fingerprint.context("--fingerprint required (full hex install ID)")?;
    let conn = license_server::open_db(db_path)?;
    if revoke {
        let licensee = licensee.unwrap_or_else(|| "*".into());
        license_server::revoke(&conn, &fingerprint, &licensee, &reason)?;
        println!("revoked licensee={licensee:?} fingerprint={fingerprint} reason={reason:?}");
    } else {
        let n = license_server::unrevoke(&conn, &fingerprint, licensee.as_deref())?;
        println!("removed {n} revocation(s) for fingerprint={fingerprint}");
    }
    Ok(())
}
