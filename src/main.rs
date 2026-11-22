use serde_json::Value;
use silo::{cli, error::Result};

fn main() {
    let raw: Vec<_> = std::env::args_os().skip(1).collect();
    let json = raw.iter().any(|s| s == "--json");
    let strings = raw
        .into_iter()
        .map(|s| s.into_string().map_err(|_| silo::error::invalid()))
        .collect::<Result<Vec<_>>>();
    let result: Result<Value> = strings
        .and_then(|args| cli::parse(&args))
        .and_then(cli::execute);
    match result {
        Ok(value) => {
            if json {
                println!("{value}");
            } else if let Some(help) = value.get("help").and_then(Value::as_str) {
                println!("{help}");
            } else {
                println!("{value}");
            }
        }
        Err(error) => {
            if json {
                println!("{}", error.value());
            } else {
                eprintln!("silo: {}", error.message);
            }
            std::process::exit(error.exit());
        }
    }
}
