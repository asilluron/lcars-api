# lcars-proto

Protobuf types ([prost]) and NATS subjects for the [lcars-api] home display
API. The types are generated from `proto/lcars/v1` in the lcars-api
repository; don't edit `src/gen` by hand.

```toml
[dependencies]
lcars-proto = { git = "https://github.com/asilluron/lcars-api" }
# no_std (with alloc), e.g. for a microcontroller:
# lcars-proto = { git = "https://github.com/asilluron/lcars-api", default-features = false }
```

Pin a `rev` or `tag` in real projects so the types match the server you run.

```rust
use lcars_proto::prost::Message;
use lcars_proto::{headers, subjects, v1};

let req = v1::GetCalendarRequest { from: String::new(), days: 7 };
let reply = nats.request(subjects::CALENDAR_GET, req.encode_to_vec().into()).await?;
if let Some(code) = reply.headers.as_ref().and_then(|h| h.get(headers::ERROR_CODE)) {
    // error reply: code is e.g. "400"; the message is in headers::ERROR
}
let calendar = v1::GetCalendarResponse::decode(reply.payload)?;
```

When the server has authentication on, connect with a device token from its
admin page as the NATS user and password:

```rust
let client = async_nats::ConnectOptions::new()
    .user_and_password("dev_7f3k2m9q4x8w".into(), secret)
    .connect("nats://192.0.2.10:4222")
    .await?;
```

`examples/agenda.rs` is a complete client using [async-nats]. It reads the
token from `NATS_USER` and `NATS_PASSWORD`:

```sh
export NATS_USER=dev_7f3k2m9q4x8w NATS_PASSWORD=...              # if the server needs a token
cargo run --example agenda -- nats://192.0.2.10:4222            # next 7 days
cargo run --example agenda -- nats://192.0.2.10:4222 2030-03-04 3 --watch
```

[prost]: https://github.com/tokio-rs/prost
[async-nats]: https://github.com/nats-io/nats.rs
[lcars-api]: https://github.com/asilluron/lcars-api
