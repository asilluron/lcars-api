//! Sends one chunk of training audio to an lcars-api server.
//!
//! ```sh
//! cargo run --example training -- nats://192.0.2.10:4222
//! ```
//!
//! Prints `accepted <bytes>` when the server has stored the chunk.
//!
//! When the server requires a device token, pass it as `NATS_USER` and
//! `NATS_PASSWORD`, the same way `examples/agenda.rs` does.

use std::process::ExitCode;

use async_nats::ConnectOptions;
use lcars_proto::headers;
use lcars_proto::prost::Message;
use lcars_proto::subjects;
use lcars_proto::v1::{PutTrainingAudioRequest, PutTrainingAudioResponse};

#[tokio::main]
async fn main() -> ExitCode {
    let mut args = std::env::args().skip(1);
    let Some(url) = args.next() else {
        eprintln!("usage: training <nats-url>");
        return ExitCode::from(2);
    };
    // Three 16-bit samples: 0, 1, -1.
    let request = PutTrainingAudioRequest {
        screen_id: "kitchen".into(),
        pcm: vec![0x00, 0x00, 0x01, 0x00, 0xff, 0xff],
    };
    match send(&url, request).await {
        Ok(accepted) => {
            println!("accepted {accepted}");
            ExitCode::SUCCESS
        }
        Err(err) => {
            eprintln!("error: {err}");
            ExitCode::FAILURE
        }
    }
}

async fn send(
    url: &str,
    request: PutTrainingAudioRequest,
) -> Result<u32, Box<dyn std::error::Error>> {
    let options = match (std::env::var("NATS_USER"), std::env::var("NATS_PASSWORD")) {
        (Ok(user), Ok(password)) => ConnectOptions::new().user_and_password(user, password),
        _ => ConnectOptions::new(),
    };
    let client = options.connect(url).await?;
    let reply = client
        .request(subjects::TRAINING_PUT, request.encode_to_vec().into())
        .await?;
    if let Some(code) = reply
        .headers
        .as_ref()
        .and_then(|h| h.get(headers::ERROR_CODE))
    {
        let msg = reply
            .headers
            .as_ref()
            .and_then(|h| h.get(headers::ERROR))
            .map(|m| m.as_str())
            .unwrap_or_default();
        return Err(format!("server returned {}: {msg}", code.as_str()).into());
    }
    let response = PutTrainingAudioResponse::decode(reply.payload)?;
    Ok(response.accepted_bytes)
}
