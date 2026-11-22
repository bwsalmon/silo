#![deny(unsafe_op_in_unsafe_fn)]
#[cfg(not(target_os = "linux"))]
compile_error!("Silo requires Linux");
pub mod auth;
pub mod cli;
pub mod control;
pub mod error;
pub mod linux;
pub mod policy;
pub mod supervisor;
