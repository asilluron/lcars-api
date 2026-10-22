//! Lists cameras from lcars-api and opens one still and one live stream.
//!
//! ```sh
//! cargo run --example cameras -- nats://192.0.2.10:4222 http://192.0.2.10:8080
//! ```
//!
//! Arguments: the NATS server URL, then the HTTP origin of the same server.
//! `stream_path` and `snapshot_path` from `lcars.cameras.list` are joined to
//! that origin. Video is not carried on NATS.
//!
//! When the server requires a device token, pass it as `NATS_USER` and
//! `NATS_PASSWORD`. The same pair is sent as HTTP Basic authentication.

use std::process::ExitCode;
use std::time::Duration;

use async_nats::ConnectOptions;
use lcars_proto::headers;
use lcars_proto::prost::Message;
use lcars_proto::subjects;
use lcars_proto::v1::{ListCamerasRequest, ListCamerasResponse};

#[tokio::main]
async fn main() -> ExitCode {
    let mut args = std::env::args().skip(1);
    let (Some(nats_url), Some(http_origin)) = (args.next(), args.next()) else {
        eprintln!("usage: cameras <nats-url> <http-origin>");
        return ExitCode::from(2);
    };
    match run(&nats_url, &http_origin).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("error: {error}");
            ExitCode::FAILURE
        }
    }
}

async fn run(nats_url: &str, http_origin: &str) -> Result<(), Box<dyn std::error::Error>> {
    let options = match (std::env::var("NATS_USER"), std::env::var("NATS_PASSWORD")) {
        (Ok(user), Ok(password)) => ConnectOptions::new().user_and_password(user, password),
        _ => ConnectOptions::new(),
    };
    let client = options.connect(nats_url).await?;
    let reply = client
        .request(
            subjects::CAMERAS_LIST,
            ListCamerasRequest {}.encode_to_vec().into(),
        )
        .await?;
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
        return Err(format!("server returned {}: {message}", code.as_str()).into());
    }
    let list = ListCamerasResponse::decode(reply.payload)?;
    let Some(camera) = list.cameras.into_iter().find(|camera| camera.has_camera) else {
        return Err("no camera".into());
    };
    println!("camera {} {}", camera.sn, camera.name);

    let http = reqwest::Client::builder()
        .connect_timeout(Duration::from_secs(30))
        .redirect(reqwest::redirect::Policy::none())
        .build()?;
    let snapshot = authed(http.get(join(http_origin, &camera.snapshot_path)?))
        .send()
        .await?;
    if !snapshot.status().is_success() {
        return Err(format!("snapshot {}", snapshot.status()).into());
    }
    let still = snapshot.bytes().await?;
    println!("snapshot {}", still.len());

    let mut stream = authed(http.get(join(http_origin, &camera.stream_path)?))
        .send()
        .await?;
    if !stream.status().is_success() {
        return Err(format!("stream {}", stream.status()).into());
    }
    let first = stream.chunk().await?.ok_or("empty stream")?;
    println!("stream {}", String::from_utf8_lossy(&first));
    drop(stream);
    Ok(())
}

fn authed(request: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
    match (std::env::var("NATS_USER"), std::env::var("NATS_PASSWORD")) {
        (Ok(user), Ok(password)) => request.basic_auth(user, Some(password)),
        _ => request,
    }
}

fn join(origin: &str, path: &str) -> Result<String, String> {
    if !path.starts_with('/') || path.starts_with("//") {
        return Err("camera path is not on this server".into());
    }
    Ok(format!("{}{path}", origin.trim_end_matches('/')))
}
