//! Lists locks from lcars-api and unlocks then locks the first one.
//!
//! ```sh
//! cargo run --example locks -- nats://192.0.2.10:4222
//! ```
//!
//! When the server requires a device token, pass it as `NATS_USER` and
//! `NATS_PASSWORD`.

use std::process::ExitCode;

use async_nats::ConnectOptions;
use lcars_proto::headers;
use lcars_proto::prost::Message;
use lcars_proto::subjects;
use lcars_proto::v1::{ListLocksRequest, ListLocksResponse, LockDoorRequest, UnlockDoorRequest};

#[tokio::main]
async fn main() -> ExitCode {
    let Some(nats_url) = std::env::args().nth(1) else {
        eprintln!("usage: locks <nats-url>");
        return ExitCode::from(2);
    };
    match run(&nats_url).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("error: {error}");
            ExitCode::FAILURE
        }
    }
}

async fn run(nats_url: &str) -> Result<(), Box<dyn std::error::Error>> {
    let options = match (std::env::var("NATS_USER"), std::env::var("NATS_PASSWORD")) {
        (Ok(user), Ok(password)) => ConnectOptions::new().user_and_password(user, password),
        _ => ConnectOptions::new(),
    };
    let client = options.connect(nats_url).await?;
    let reply = client
        .request(
            subjects::LOCKS_LIST,
            ListLocksRequest {}.encode_to_vec().into(),
        )
        .await?;
    check_error(&reply)?;
    let list = ListLocksResponse::decode(reply.payload)?;
    let Some(lock) = list.locks.into_iter().next() else {
        return Err("no lock".into());
    };
    println!("lock {} {} locked={}", lock.id, lock.name, lock.locked);

    if lock.can_unlock {
        let reply = client
            .request(
                subjects::LOCKS_UNLOCK,
                UnlockDoorRequest {
                    id: lock.id.clone(),
                }
                .encode_to_vec()
                .into(),
            )
            .await?;
        check_error(&reply)?;
        println!("unlocked {}", lock.id);
    }
    if lock.can_lock {
        let reply = client
            .request(
                subjects::LOCKS_LOCK,
                LockDoorRequest {
                    id: lock.id.clone(),
                }
                .encode_to_vec()
                .into(),
            )
            .await?;
        check_error(&reply)?;
        println!("locked {}", lock.id);
    }
    Ok(())
}

fn check_error(reply: &async_nats::Message) -> Result<(), String> {
    if let Some(code) = reply
        .headers
        .as_ref()
        .and_then(|headers| headers.get(headers::ERROR_CODE))
    {
        let message = reply
            .headers
            .as_ref()
            .and_then(|headers| headers.get(headers::ERROR))
            .map(|value| value.as_str())
            .unwrap_or_default();
        return Err(format!("server returned {}: {message}", code.as_str()));
    }
    Ok(())
}
