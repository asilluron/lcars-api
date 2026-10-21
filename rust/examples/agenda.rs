//! Prints the calendar from an lcars-api server, one line per day.
//!
//! ```sh
//! cargo run --example agenda -- nats://192.0.2.10:4222 2030-03-04 7
//! ```
//!
//! Arguments: the NATS server URL, then optionally the first day (YYYY-MM-DD,
//! default today) and the number of days (default 7). With `--watch` after
//! them, it prints the calendar again each time the server reports a change.

use std::process::ExitCode;

use async_nats::Client;
use futures::StreamExt;
use lcars_proto::headers;
use lcars_proto::prost::Message;
use lcars_proto::subjects;
use lcars_proto::v1::{CalendarDay, CalendarUpdated, GetCalendarRequest, GetCalendarResponse};

#[tokio::main]
async fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let watch = args.iter().any(|a| a == "--watch");
    let args: Vec<&str> = args
        .iter()
        .filter(|a| *a != "--watch")
        .map(String::as_str)
        .collect();
    let Some(url) = args.first() else {
        eprintln!("usage: agenda <nats-url> [from] [days] [--watch]");
        return ExitCode::from(2);
    };
    let req = GetCalendarRequest {
        from: args.get(1).copied().unwrap_or_default().to_string(),
        days: match args.get(2).map(|d| d.parse()) {
            None => 0, // the server's default
            Some(Ok(days)) => days,
            Some(Err(err)) => {
                eprintln!("invalid days: {err}");
                return ExitCode::from(2);
            }
        },
    };

    match run(url, req, watch).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("error: {err}");
            ExitCode::FAILURE
        }
    }
}

async fn run(
    url: &str,
    req: GetCalendarRequest,
    watch: bool,
) -> Result<(), Box<dyn std::error::Error>> {
    let client = async_nats::connect(url).await?;
    // Subscribe before the first request so no update is missed.
    let mut updates = client.subscribe(subjects::CALENDAR_UPDATED).await?;
    print_calendar(&get_calendar(&client, &req).await?);
    if !watch {
        return Ok(());
    }
    while let Some(msg) = updates.next().await {
        let update = CalendarUpdated::decode(msg.payload)?;
        println!("-- updated at {} --", update.updated_at_unix);
        print_calendar(&get_calendar(&client, &req).await?);
    }
    Ok(())
}

/// Requests the calendar, turning an error reply into an error.
async fn get_calendar(
    client: &Client,
    req: &GetCalendarRequest,
) -> Result<GetCalendarResponse, Box<dyn std::error::Error>> {
    let reply = client
        .request(subjects::CALENDAR_GET, req.encode_to_vec().into())
        .await?;
    if let Some(code) = reply
        .headers
        .as_ref()
        .and_then(|h| h.get(headers::ERROR_CODE))
    {
        let msg = reply.headers.as_ref().and_then(|h| h.get(headers::ERROR));
        let msg = msg.map(|m| m.as_str()).unwrap_or_default();
        return Err(format!("server returned {}: {msg}", code.as_str()).into());
    }
    Ok(GetCalendarResponse::decode(reply.payload)?)
}

fn print_calendar(cal: &GetCalendarResponse) {
    for day in &cal.days {
        println!("{}", format_day(day));
    }
}

/// Formats a day as "2030-03-04 Monday: work/Standup 09:00-09:15, ...".
fn format_day(day: &CalendarDay) -> String {
    let events: Vec<String> = day
        .events
        .iter()
        .map(|ev| {
            let when = if ev.all_day {
                "all-day".to_string()
            } else {
                format!(
                    "{}-{}",
                    ev.start_time.as_deref().unwrap_or("?"),
                    ev.end_time.as_deref().unwrap_or("?")
                )
            };
            format!("{}/{} {when}", ev.calendar, ev.title)
        })
        .collect();
    format!("{} {}: {}", day.date, day.weekday, events.join(", "))
}
