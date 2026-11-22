use serde_json::Value;
use silo::{cli, error::Result};

fn main() {
    let raw: Vec<_> = std::env::args_os().skip(1).collect();
    if raw.len() == 1 && raw[0] == "__workload_exec" {
        std::process::exit(if silo::trampoline::entry().is_ok() {
            0
        } else {
            5
        });
    }
    if raw.len() == 1 && raw[0] == "__namespace_helper" {
        match silo::helper::entry() {
            Ok(_) => return,
            Err(error) => {
                let exit = error.exit();
                let _ = silo::helper::emit(&silo::helper::Event::Error { error });
                std::process::exit(exit);
            }
        }
    }
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
