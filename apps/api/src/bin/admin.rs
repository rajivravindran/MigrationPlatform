//! Admin CLI for trusted shells: migrations, user creation, secret rotation,
//! schedule ops, org bootstrap.

use std::env;

use anyhow::{bail, Context, Result};
use sqlx::postgres::PgPoolOptions;

use migration_api::config::Config;
use migration_api::security::{license, passwords, MasterKey};
use migration_api::telemetry;

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<()> {
    dotenvy::dotenv().ok();
    telemetry::init_tracing("migration-admin", None).ok();

    let args: Vec<String> = env::args().skip(1).collect();
    if args.is_empty() {
        print_help();
        return Ok(());
    }

    // License commands run offline on the vendor's machine — they must not
    // demand the deployment env (MASTER_KEY, DATABASE_URL, ...).
    match args[0].as_str() {
        "license-keygen" => return license_keygen(&args[1..]),
        "license-sign" => return license_sign(&args[1..]),
        "license-verify" => return license_verify(&args[1..]),
        "help" | "-h" | "--help" => {
            print_help();
            return Ok(());
        }
        _ => {}
    }

    let cfg = Config::from_env()?;
    match args[0].as_str() {
        "migrate" => migrate(&cfg).await,
        "create-org" => create_org(&cfg, &args[1..]).await,
        "create-user" => create_user(&cfg, &args[1..]).await,
        "upsert-secret" => upsert_secret(&cfg, &args[1..]).await,
        other => bail!("unknown subcommand {other}; run `admin help` for usage"),
    }
}

fn print_help() {
    eprintln!(
        r#"migration-admin CLI

    admin migrate
    admin create-org     --name <name> [--slug <slug>]
    admin create-user    --org <id> --email <email> --role <role> --password <pw>
    admin upsert-secret  --org <id> --name <n>       --value <raw_string>
    admin license-keygen --out-dir <dir>
    admin license-sign   --key <private.pem> --licensee <name> --expires <YYYY-MM-DD>
                         [--features a,b] [--max-seats <n>] [--kind trial|commercial]
                         [--fingerprint <hex>] [--require-heartbeat] --out <license.json>
                         (--require-heartbeat needs --fingerprint; the install must then
                          reach the license server every 24h, with 72h offline grace)
    admin license-verify --file <license.json> [--public-key <public.pem>]
"#
    );
}

fn license_keygen(args: &[String]) -> Result<()> {
    let mut out_dir = ".".to_string();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--out-dir" => {
                out_dir = args[i + 1].clone();
                i += 2
            }
            other => bail!("unknown flag {other}"),
        }
    }
    use rsa::pkcs8::{EncodePrivateKey, EncodePublicKey};
    let mut rng = rand::thread_rng();
    let priv_key = rsa::RsaPrivateKey::new(&mut rng, 2048)?;
    let pub_key = rsa::RsaPublicKey::from(&priv_key);
    std::fs::create_dir_all(&out_dir)?;
    let priv_path = format!("{out_dir}/license_signing_key.pem");
    let pub_path = format!("{out_dir}/license_public_key.pem");
    std::fs::write(
        &priv_path,
        priv_key
            .to_pkcs8_pem(rsa::pkcs8::LineEnding::LF)?
            .as_bytes(),
    )?;
    std::fs::write(
        &pub_path,
        pub_key.to_public_key_pem(rsa::pkcs8::LineEnding::LF)?,
    )?;
    println!(
        "wrote {priv_path} (keep OFFLINE) and {pub_path} (embed in builds via infra/license/)"
    );
    Ok(())
}

fn license_sign(args: &[String]) -> Result<()> {
    let mut key_path = None;
    let mut licensee = None;
    let mut expires = None;
    let mut features: Vec<String> = Vec::new();
    let mut max_seats = None;
    let mut kind = license::LicenseKind::Commercial;
    let mut fingerprint: Option<String> = None;
    let mut require_heartbeat = false;
    let mut out = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--require-heartbeat" => {
                require_heartbeat = true;
                i += 1
            }
            "--key" => {
                key_path = Some(args[i + 1].clone());
                i += 2
            }
            "--licensee" => {
                licensee = Some(args[i + 1].clone());
                i += 2
            }
            "--expires" => {
                expires = Some(args[i + 1].clone());
                i += 2
            }
            "--features" => {
                features = args[i + 1]
                    .split(',')
                    .map(|s| s.trim().to_string())
                    .collect();
                i += 2
            }
            "--max-seats" => {
                max_seats = Some(args[i + 1].parse::<u32>()?);
                i += 2
            }
            "--kind" => {
                kind = match args[i + 1].as_str() {
                    "trial" => license::LicenseKind::Trial,
                    "commercial" => license::LicenseKind::Commercial,
                    other => bail!("--kind must be trial or commercial, got {other}"),
                };
                i += 2;
            }
            "--fingerprint" => {
                fingerprint = Some(args[i + 1].clone());
                i += 2
            }
            "--out" => {
                out = Some(args[i + 1].clone());
                i += 2
            }
            other => bail!("unknown flag {other}"),
        }
    }
    let key_path = key_path.context("--key required")?;
    let licensee = licensee.context("--licensee required")?;
    let expires = expires.context("--expires required (YYYY-MM-DD or RFC3339)")?;
    let out = out.context("--out required")?;

    let expires_at = chrono::DateTime::parse_from_rfc3339(&expires)
        .map(|dt| dt.with_timezone(&chrono::Utc))
        .or_else(|_| {
            chrono::NaiveDate::parse_from_str(&expires, "%Y-%m-%d")
                .map(|d| d.and_hms_opt(23, 59, 59).unwrap().and_utc())
        })
        .context("--expires must be YYYY-MM-DD or RFC3339")?;
    if expires_at <= chrono::Utc::now() {
        bail!("--expires is already in the past");
    }
    if let Some(fp) = fingerprint.as_deref() {
        if fp.len() < 16 || !fp.chars().all(|c| c.is_ascii_hexdigit()) {
            bail!("--fingerprint must be the full hex install ID (see Settings → License)");
        }
    }
    if require_heartbeat && fingerprint.is_none() {
        bail!("--require-heartbeat requires --fingerprint so the server can bind the heartbeat");
    }

    let doc = license::LicenseDoc {
        licensee,
        expires_at,
        features,
        max_seats,
        issued_at: Some(chrono::Utc::now()),
        kind,
        fingerprint: fingerprint.map(|f| f.to_ascii_lowercase()),
        requires_heartbeat: require_heartbeat,
    };
    let pem = std::fs::read_to_string(&key_path).with_context(|| format!("reading {key_path}"))?;
    let signed = license::sign_license(&doc, &pem)?;
    std::fs::write(&out, &signed)?;
    println!(
        "license for {:?} kind={:?} (expires {}, heartbeat={}) written to {out}",
        doc.licensee, doc.kind, doc.expires_at, doc.requires_heartbeat
    );
    Ok(())
}

fn license_verify(args: &[String]) -> Result<()> {
    use rsa::pkcs8::DecodePublicKey;
    let mut file = None;
    let mut public_key = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--file" => {
                file = Some(args[i + 1].clone());
                i += 2
            }
            "--public-key" => {
                public_key = Some(args[i + 1].clone());
                i += 2
            }
            other => bail!("unknown flag {other}"),
        }
    }
    let file = file.context("--file required")?;
    let raw = std::fs::read_to_string(&file)?;
    let key = match public_key {
        Some(p) => rsa::RsaPublicKey::from_public_key_pem(&std::fs::read_to_string(&p)?)?,
        None => rsa::RsaPublicKey::from_public_key_pem(include_str!(
            "../../../../infra/license/license_public_key.pem"
        ))?,
    };
    let doc = license::verify_license(&raw, &key)?;
    println!(
        "VALID: licensee={:?} kind={:?} expires={} features={:?} max_seats={:?} fingerprint={:?} requires_heartbeat={}",
        doc.licensee,
        doc.kind,
        doc.expires_at,
        doc.features,
        doc.max_seats,
        doc.fingerprint,
        doc.requires_heartbeat
    );
    Ok(())
}

async fn migrate(cfg: &Config) -> Result<()> {
    let pool = PgPoolOptions::new()
        .max_connections(2)
        .connect(&cfg.database_url)
        .await?;
    sqlx::migrate!("./migrations").run(&pool).await?;
    println!("migrations applied");
    Ok(())
}

async fn create_org(cfg: &Config, args: &[String]) -> Result<()> {
    let mut name = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--name" => {
                name = Some(args[i + 1].clone());
                i += 2
            }
            // accepted for backward-compat with older docs/scripts; ignored
            // because the schema only has organizations(name).
            "--slug" => i += 2,
            other => bail!("unknown flag {other}"),
        }
    }
    let name = name.context("--name required")?;

    let pool = PgPoolOptions::new()
        .max_connections(2)
        .connect(&cfg.database_url)
        .await?;
    let row: (i64,) = sqlx::query_as(
        "INSERT INTO organizations (name) VALUES ($1)
         ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
         RETURNING id",
    )
    .bind(&name)
    .fetch_one(&pool)
    .await?;
    println!("org id={} name={}", row.0, name);
    Ok(())
}

async fn create_user(cfg: &Config, args: &[String]) -> Result<()> {
    let mut org = None;
    let mut email = None;
    let mut role = "viewer".to_string();
    let mut password = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--org" => {
                org = Some(args[i + 1].parse::<i64>()?);
                i += 2
            }
            "--email" => {
                email = Some(args[i + 1].clone());
                i += 2
            }
            "--role" => {
                role = args[i + 1].clone();
                i += 2
            }
            "--password" => {
                password = Some(args[i + 1].clone());
                i += 2
            }
            other => bail!("unknown flag {other}"),
        }
    }
    let org = org.context("--org required")?;
    let email = email.context("--email required")?;
    let password = password.context("--password required")?;

    let pool = PgPoolOptions::new()
        .max_connections(2)
        .connect(&cfg.database_url)
        .await?;
    let hash = passwords::hash_password(&password).map_err(|e| anyhow::anyhow!("{e}"))?;
    let row: (i64,) = sqlx::query_as(
        "INSERT INTO users (org_id, email, role, password_hash) VALUES ($1, $2, $3::user_role, $4)
         ON CONFLICT (email) DO UPDATE SET role = EXCLUDED.role, password_hash = EXCLUDED.password_hash
         RETURNING id",
    )
    .bind(org)
    .bind(&email)
    .bind(&role)
    .bind(&hash)
    .fetch_one(&pool)
    .await?;
    println!("user id={} email={} role={}", row.0, email, role);
    Ok(())
}

async fn upsert_secret(cfg: &Config, args: &[String]) -> Result<()> {
    let mut org = None;
    let mut name = None;
    let mut value = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--org" => {
                org = Some(args[i + 1].parse::<i64>()?);
                i += 2
            }
            "--name" => {
                name = Some(args[i + 1].clone());
                i += 2
            }
            "--value" => {
                value = Some(args[i + 1].clone());
                i += 2
            }
            other => bail!("unknown flag {other}"),
        }
    }
    let org = org.context("--org required")?;
    let name = name.context("--name required")?;
    let value = value.context("--value required")?;

    let key = MasterKey::from_env_value(&cfg.master_key)?;
    let (ct, nonce) = key.encrypt(value.as_bytes())?;
    let pool = PgPoolOptions::new()
        .max_connections(2)
        .connect(&cfg.database_url)
        .await?;
    let row: (i64,) = sqlx::query_as(
        "INSERT INTO secrets (org_id, name, ciphertext, nonce) VALUES ($1, $2, $3, $4)
         ON CONFLICT (org_id, name) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, nonce = EXCLUDED.nonce, updated_at = now()
         RETURNING id",
    )
    .bind(org)
    .bind(&name)
    .bind(&ct)
    .bind(&nonce)
    .fetch_one(&pool)
    .await?;
    println!("secret id={} name={}", row.0, name);
    Ok(())
}
