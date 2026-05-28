//! contributors-webhook — admission webhook for ai-memory.
//!
//! Listens on POST `/enrich`. For each page write, reads the actor info
//! from the admission `ctx` (`agent`, `user`, `sub`, `client`,
//! `session_id`) and appends/updates an entry in
//! `frontmatter.contributors` (append-only set keyed by `agent + client`).
//!
//! ## Response
//!
//! - `200 OK` with the mutated `{ page: { frontmatter } }` when actor is
//!   non-empty (i.e. there's something to attribute).
//! - `204 No Content` when actor is empty (anonymous / internal write) —
//!   nothing to attribute, leaves the page unchanged.
//!
//! ## Wire shape (admission contract)
//!
//! Engine sends: `{ page: { path, frontmatter, body }, ctx: { actor, ... } }`
//! Engine consumes: `{ page: { frontmatter?, body? } }` (200) or empty (204).
//!
//! ## Configuration
//!
//! Listen address via `LISTEN_ADDR` (default `0.0.0.0:8080`).

use std::net::SocketAddr;
use std::str::FromStr;

use axum::{Json, Router, http::StatusCode, response::IntoResponse, routing::post};
use jiff::Timestamp;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value, json};

#[derive(Debug, Deserialize)]
struct WebhookPayload {
    page: PagePayload,
    ctx: CtxPayload,
}

#[derive(Debug, Deserialize)]
struct PagePayload {
    #[serde(default)]
    path: String,
    #[serde(default)]
    frontmatter: Value,
    // body not needed for this webhook — we only touch frontmatter.
    #[serde(default)]
    #[allow(dead_code)]
    body: String,
}

#[derive(Debug, Deserialize)]
struct CtxPayload {
    #[serde(default)]
    actor: Actor,
}

#[derive(Debug, Deserialize, Default)]
struct Actor {
    agent: Option<String>,
    user: Option<String>,
    sub: Option<String>,
    client: Option<String>,
    #[serde(default)]
    #[allow(dead_code)]
    session_id: Option<String>,
}

#[derive(Debug, Serialize)]
struct Contributor {
    agent: String,
    user: Option<String>,
    sub: Option<String>,
    client: String,
    first_seen: String,
    last_seen: String,
    writes: u64,
}

async fn enrich(Json(payload): Json<WebhookPayload>) -> impl IntoResponse {
    let actor = payload.ctx.actor;
    // No identifying info → nothing to attribute. Return 204 to keep the
    // page unchanged (lets the engine skip mutation entirely).
    let (Some(agent), Some(client)) = (actor.agent.as_ref(), actor.client.as_ref()) else {
        tracing::debug!(
            path = %payload.page.path,
            "actor has no agent or client; returning 204"
        );
        return (StatusCode::NO_CONTENT).into_response();
    };

    let now = Timestamp::now().to_string();

    // Convert frontmatter to a JSON object, creating one if absent.
    let mut fm = match payload.page.frontmatter {
        Value::Object(m) => m,
        Value::Null => Map::new(),
        other => {
            tracing::warn!(
                path = %payload.page.path,
                "frontmatter is not an object ({}); replacing with object containing contributors",
                value_type(&other)
            );
            Map::new()
        }
    };

    // Existing contributors list (or empty).
    let existing: Vec<Value> = fm
        .get("contributors")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();

    // Match on (agent + client) — composite key.
    let key = format!("{agent}|{client}");
    let mut updated = false;
    let mut new_list: Vec<Value> = existing
        .into_iter()
        .map(|entry| {
            let entry_key = entry
                .as_object()
                .map(|o| {
                    format!(
                        "{}|{}",
                        o.get("agent").and_then(Value::as_str).unwrap_or(""),
                        o.get("client").and_then(Value::as_str).unwrap_or(""),
                    )
                })
                .unwrap_or_default();
            if entry_key == key {
                updated = true;
                let mut obj = entry.as_object().cloned().unwrap_or_default();
                obj.insert("last_seen".into(), Value::String(now.clone()));
                let writes = obj
                    .get("writes")
                    .and_then(Value::as_u64)
                    .unwrap_or(0)
                    .saturating_add(1);
                obj.insert("writes".into(), json!(writes));
                // Keep entry stable (don't churn unrelated fields).
                if let Some(user) = actor.user.as_ref() {
                    obj.entry("user")
                        .or_insert_with(|| Value::String(user.clone()));
                }
                if let Some(sub) = actor.sub.as_ref() {
                    obj.entry("sub")
                        .or_insert_with(|| Value::String(sub.clone()));
                }
                Value::Object(obj)
            } else {
                entry
            }
        })
        .collect();

    if !updated {
        let new_entry = Contributor {
            agent: agent.clone(),
            user: actor.user.clone(),
            sub: actor.sub.clone(),
            client: client.clone(),
            first_seen: now.clone(),
            last_seen: now,
            writes: 1,
        };
        new_list.push(serde_json::to_value(new_entry).unwrap_or(Value::Null));
    }

    fm.insert("contributors".into(), Value::Array(new_list));

    tracing::debug!(
        path = %payload.page.path,
        agent = %agent,
        client = %client,
        updated = updated,
        "contributors enriched"
    );

    let body = json!({ "page": { "frontmatter": Value::Object(fm) } });
    (StatusCode::OK, Json(body)).into_response()
}

async fn health() -> impl IntoResponse {
    (StatusCode::OK, "ok\n")
}

fn value_type(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let addr: SocketAddr = std::env::var("LISTEN_ADDR")
        .ok()
        .as_deref()
        .map(SocketAddr::from_str)
        .transpose()?
        .unwrap_or_else(|| SocketAddr::from(([0, 0, 0, 0], 8080)));

    let app = Router::new()
        .route("/enrich", post(enrich))
        .route("/healthz", axum::routing::get(health));

    let listener = tokio::net::TcpListener::bind(addr).await?;
    tracing::info!(addr = %addr, "contributors-webhook listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

async fn shutdown_signal() {
    let ctrl_c = async {
        tokio::signal::ctrl_c().await.ok();
    };
    #[cfg(unix)]
    let terminate = async {
        if let Ok(mut sig) = tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
        {
            sig.recv().await;
        }
    };
    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();
    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }
    tracing::info!("shutdown signal received");
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    fn payload(actor_json: Value, fm: Value) -> WebhookPayload {
        let raw = json!({
            "page": { "path": "p.md", "frontmatter": fm, "body": "" },
            "ctx": { "actor": actor_json },
        });
        serde_json::from_value(raw).unwrap()
    }

    #[tokio::test]
    async fn anonymous_actor_returns_204() {
        use axum::body::to_bytes;
        let resp = enrich(Json(payload(json!({}), Value::Null)))
            .await
            .into_response();
        assert_eq!(resp.status(), StatusCode::NO_CONTENT);
        let body = to_bytes(resp.into_body(), 1024).await.unwrap();
        assert!(body.is_empty());
    }

    #[tokio::test]
    async fn known_actor_appends_new_contributor() {
        use axum::body::to_bytes;
        let resp = enrich(Json(payload(
            json!({"agent": "claude-code", "user": "djalmajr", "client": "c1"}),
            Value::Null,
        )))
        .await
        .into_response();
        assert_eq!(resp.status(), StatusCode::OK);
        let body = to_bytes(resp.into_body(), 64 * 1024).await.unwrap();
        let v: Value = serde_json::from_slice(&body).unwrap();
        let arr = v["page"]["frontmatter"]["contributors"].as_array().unwrap();
        assert_eq!(arr.len(), 1);
        assert_eq!(arr[0]["agent"], "claude-code");
        assert_eq!(arr[0]["client"], "c1");
        assert_eq!(arr[0]["user"], "djalmajr");
        assert_eq!(arr[0]["writes"], 1);
        assert!(arr[0]["first_seen"].is_string());
        assert_eq!(arr[0]["first_seen"], arr[0]["last_seen"]);
    }

    #[tokio::test]
    async fn second_write_same_actor_increments_writes() {
        use axum::body::to_bytes;
        // First write
        let resp1 = enrich(Json(payload(
            json!({"agent": "claude-code", "user": "djalmajr", "client": "c1"}),
            Value::Null,
        )))
        .await
        .into_response();
        let body1 = to_bytes(resp1.into_body(), 64 * 1024).await.unwrap();
        let v1: Value = serde_json::from_slice(&body1).unwrap();
        let fm_after_first = v1["page"]["frontmatter"].clone();

        // Sleep to ensure last_seen advances on the second call.
        tokio::time::sleep(std::time::Duration::from_millis(15)).await;

        // Second write with same agent+client
        let resp2 = enrich(Json(payload(
            json!({"agent": "claude-code", "user": "djalmajr", "client": "c1"}),
            fm_after_first,
        )))
        .await
        .into_response();
        let body2 = to_bytes(resp2.into_body(), 64 * 1024).await.unwrap();
        let v2: Value = serde_json::from_slice(&body2).unwrap();
        let arr = v2["page"]["frontmatter"]["contributors"].as_array().unwrap();
        assert_eq!(arr.len(), 1, "no duplicate entry");
        assert_eq!(arr[0]["writes"], 2);
        assert!(arr[0]["last_seen"] != arr[0]["first_seen"]);
    }

    #[tokio::test]
    async fn different_clients_create_distinct_entries() {
        use axum::body::to_bytes;
        let resp1 = enrich(Json(payload(
            json!({"agent": "claude-code", "client": "c1"}),
            Value::Null,
        )))
        .await
        .into_response();
        let body1 = to_bytes(resp1.into_body(), 64 * 1024).await.unwrap();
        let fm = serde_json::from_slice::<Value>(&body1).unwrap()["page"]["frontmatter"].clone();

        let resp2 = enrich(Json(payload(
            json!({"agent": "codex", "client": "c2"}),
            fm,
        )))
        .await
        .into_response();
        let body2 = to_bytes(resp2.into_body(), 64 * 1024).await.unwrap();
        let arr = serde_json::from_slice::<Value>(&body2).unwrap()["page"]["frontmatter"]
            ["contributors"]
            .as_array()
            .unwrap()
            .clone();
        assert_eq!(arr.len(), 2);
        let agents: BTreeMap<&str, &str> = arr
            .iter()
            .map(|e| {
                (
                    e["agent"].as_str().unwrap(),
                    e["client"].as_str().unwrap(),
                )
            })
            .collect();
        assert_eq!(agents.get("claude-code"), Some(&"c1"));
        assert_eq!(agents.get("codex"), Some(&"c2"));
    }
}
