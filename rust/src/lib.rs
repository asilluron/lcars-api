//! Protobuf types and NATS subjects for the [lcars-api] home display API.
//!
//! lcars-api serves its API over NATS. Each subject carries one of the
//! messages in [`v1`], encoded with [prost]:
//!
//! | Subject                                | Kind          | Message                                              |
//! |----------------------------------------|---------------|------------------------------------------------------|
//! | [`subjects::CALENDAR_GET`]             | request/reply | [`v1::GetCalendarRequest`] → [`v1::GetCalendarResponse`] |
//! | [`subjects::CALENDAR_UPDATED`]         | event         | [`v1::CalendarUpdated`]                              |
//! | [`subjects::CAMERAS_LIST`]             | request/reply | [`v1::ListCamerasRequest`] → [`v1::ListCamerasResponse`] |
//! | [`subjects::LOCKS_LIST`]               | request/reply | [`v1::ListLocksRequest`] → [`v1::ListLocksResponse`] |
//! | [`subjects::LOCKS_LOCK`]               | request/reply | [`v1::LockDoorRequest`] → [`v1::LockDoorResponse`] |
//! | [`subjects::LOCKS_UNLOCK`]             | request/reply | [`v1::UnlockDoorRequest`] → [`v1::UnlockDoorResponse`] |
//! | [`subjects::TRAINING_PUT`]             | request/reply | [`v1::PutTrainingAudioRequest`] → [`v1::PutTrainingAudioResponse`] |
//!
//! A failed request gets a reply with an empty body and two headers:
//! [`headers::ERROR_CODE`] (an HTTP-style status such as `"400"`) and
//! [`headers::ERROR`] (a message).
//!
//! The crate is `no_std` (with `alloc`) when its default `std` feature is
//! disabled, for microcontroller targets.
//!
//! ```
//! use lcars_proto::prost::Message;
//! use lcars_proto::v1::GetCalendarRequest;
//!
//! let req = GetCalendarRequest { from: "2030-03-04".into(), days: 7 };
//! let body = req.encode_to_vec(); // send as a request on subjects::CALENDAR_GET
//! assert_eq!(GetCalendarRequest::decode(body.as_slice()).unwrap(), req);
//! ```
//!
//! See `examples/agenda.rs` for a calendar client, `examples/cameras.rs`
//! for listing cameras and opening the HTTP still and live stream,
//! `examples/locks.rs` for listing and locking a door, and
//! `examples/training.rs` for sending a chunk of training audio.
//!
//! [lcars-api]: https://github.com/asilluron/lcars-api

#![cfg_attr(not(feature = "std"), no_std)]
#![warn(missing_docs)]

/// The prost version the types are generated for, so clients can use
/// `lcars_proto::prost::Message` without depending on a matching version.
pub use prost;

/// Version 1 of the API messages, generated from `proto/lcars/v1` by
/// `make proto`.
#[allow(missing_docs)]
pub mod v1 {
    include!("gen/lcars/v1/lcars.v1.rs");
}

/// NATS subjects. These must match `internal/natsapi` in the server; a Go
/// test checks that they do.
pub mod subjects {
    /// Request a [`GetCalendarRequest`](crate::v1::GetCalendarRequest); the
    /// reply is a [`GetCalendarResponse`](crate::v1::GetCalendarResponse).
    pub const CALENDAR_GET: &str = "lcars.calendar.get";
    /// A [`CalendarUpdated`](crate::v1::CalendarUpdated) is published here
    /// when the calendar changes. Request the calendar again on receipt.
    pub const CALENDAR_UPDATED: &str = "lcars.calendar.updated";
    /// Request a [`ListCamerasRequest`](crate::v1::ListCamerasRequest); the
    /// reply is a [`ListCamerasResponse`](crate::v1::ListCamerasResponse).
    /// Each camera's `stream_path` and `snapshot_path` are HTTP paths on
    /// this server. Video is not sent on NATS. Close the stream response
    /// to stop that viewer.
    pub const CAMERAS_LIST: &str = "lcars.cameras.list";
    /// Request a [`ListLocksRequest`](crate::v1::ListLocksRequest); the
    /// reply is a [`ListLocksResponse`](crate::v1::ListLocksResponse).
    pub const LOCKS_LIST: &str = "lcars.locks.list";
    /// Send a [`LockDoorRequest`](crate::v1::LockDoorRequest); the reply is
    /// a [`LockDoorResponse`](crate::v1::LockDoorResponse). The server waits
    /// for Seam to confirm the action.
    pub const LOCKS_LOCK: &str = "lcars.locks.lock";
    /// Send an [`UnlockDoorRequest`](crate::v1::UnlockDoorRequest); the
    /// reply is an [`UnlockDoorResponse`](crate::v1::UnlockDoorResponse).
    /// The server waits for Seam to confirm the action.
    pub const LOCKS_UNLOCK: &str = "lcars.locks.unlock";
    /// Send a [`PutTrainingAudioRequest`](crate::v1::PutTrainingAudioRequest);
    /// the reply is a [`PutTrainingAudioResponse`](crate::v1::PutTrainingAudioResponse).
    /// The server stores the PCM under a name it chooses.
    pub const TRAINING_PUT: &str = "lcars.training.put";
}

/// Headers on error replies (the NATS service API convention).
pub mod headers {
    /// HTTP-style status code: `"400"` for an invalid request, `"503"` if
    /// the data source isn't configured on the server, `"500"` otherwise.
    pub const ERROR_CODE: &str = "Nats-Service-Error-Code";
    /// Human-readable error message.
    pub const ERROR: &str = "Nats-Service-Error";
}

/// The NATS service name, for `$SRV.PING.lcars`, `$SRV.INFO.lcars` and
/// `$SRV.STATS.lcars`.
pub const SERVICE_NAME: &str = "lcars";
